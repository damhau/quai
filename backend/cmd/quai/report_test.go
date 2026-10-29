package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/pgtest"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

const reportToken = "run-token-for-report-tests"

// quaiForReports serves Quai with infra-live, the example repository, whose run token is
// reportToken, and returns its URL and its store.
func quaiForReports(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "infra-live.git"), filepath.Join(dir, "work")
	for _, args := range [][]string{{"init", "--quiet", "--bare", "--initial-branch=main", bare}, {"clone", "--quiet", bare, work}} {
		gitIn(t, "", args...)
	}
	if err := os.CopyFS(work, os.DirFS("../../../examples/gitops-repo")); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "add", "--all")
	gitIn(t, work, "commit", "--quiet", "-m", "the existing infrastructure")
	gitIn(t, work, "push", "--quiet", "origin", "HEAD:main")

	doc := filepath.Join(dir, "repositories.yaml")
	if err := os.WriteFile(doc, []byte(`apiVersion: quai.dev/v1beta1
kind: Repository
metadata: { name: infra-live }
spec:
  connector: generic
  url: `+bare+`
  runner: { reportToken: { ref: env:QUAI_TEST_REPORT_TOKEN_SHA256 } }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(reportToken))
	t.Setenv("QUAI_TEST_REPORT_TOKEN_SHA256", hex.EncodeToString(sum[:]))

	cfg, err := config.Load("", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repositories.Path, cfg.Git.CacheDir = doc, filepath.Join(dir, "cache")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := pgtest.Store(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), ln, s, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("serve did not stop")
		}
	})
	return "http://" + ln.Addr().String(), s
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...) // #nosec G204 -- test helper
	cmd.Dir = dir
	cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + os.TempDir(), "PATH=" + os.Getenv("PATH"),
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com", "GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func env(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }

const plans = "../../internal/engine/tfplan/testdata/"

// TestReport: quai report fills the commit, the pull request and the run's page from each CI's
// variables, reads the token from the environment or a file, and sends the plan.
func TestReport(t *testing.T) {
	server, s := quaiForReports(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(reportToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	quai := map[string]string{"QUAI_URL": server, "QUAI_REPOSITORY": "infra-live"}
	with := func(vars map[string]string) map[string]string {
		for k, v := range quai {
			vars[k] = v
		}
		return vars
	}
	cases := []struct {
		name        string
		opts        reportOptions
		vars        map[string]string
		pullRequest string
		url         string
	}{
		{"GitHub Actions, the token from the environment", reportOptions{plan: plans + "prod.json"},
			with(map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SHA": "1111111111111111111111111111111111111111", "GITHUB_REF": "refs/pull/7/merge",
				"GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "acme/infra", "GITHUB_RUN_ID": "42", "GITHUB_RUN_ATTEMPT": "2", "QUAI_RUN_TOKEN": reportToken}),
			"7", "https://github.com/acme/infra/actions/runs/42/attempts/2"},
		{"Azure Pipelines, the token from a file", reportOptions{},
			with(map[string]string{"TF_BUILD": "True", "BUILD_SOURCEVERSION": "2222222222222222222222222222222222222222", "SYSTEM_PULLREQUEST_PULLREQUESTID": "412",
				"SYSTEM_COLLECTIONURI": "https://dev.azure.com/acme/", "SYSTEM_TEAMPROJECT": "Infra Live", "BUILD_BUILDID": "88", "QUAI_RUN_TOKEN_FILE": tokenFile}),
			"412", "https://dev.azure.com/acme/Infra%20Live/_build/results?buildId=88"},
		{"GitLab CI, flags over the CI's, with a cost", reportOptions{pullRequest: "6", tokenFile: tokenFile, costDelta: "142.4", costCurrency: "CHF"},
			with(map[string]string{"GITLAB_CI": "true", "CI_COMMIT_SHA": "3333333333333333333333333333333333333333", "CI_MERGE_REQUEST_IID": "5",
				"CI_PIPELINE_URL": "https://gitlab.acme.ch/infra/live/-/pipelines/9"}),
			"6", "https://gitlab.acme.ch/infra/live/-/pipelines/9"},
	}
	for _, c := range cases {
		o := c.opts
		o.stack, o.kind, o.status = "envs/prod", "plan", "succeeded"
		var out bytes.Buffer
		if err := o.run(t.Context(), &out, env(c.vars)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		runs, err := s.ListRuns(t.Context(), db.ListRunsParams{Repository: "infra-live", MaxRows: 1})
		if err != nil || len(runs) != 1 {
			t.Fatalf("%s: runs %v, %v", c.name, runs, err)
		}
		if r := runs[0]; r.PullRequest != c.pullRequest || r.Url != c.url || r.Kind != "plan" || r.Stack != "envs/prod" {
			t.Errorf("%s: the run = %+v", c.name, r)
		}
		if c.opts.costDelta != "" && string(runs[0].Cost) != `{"currency": "CHF", "monthlyDelta": 142.4}` {
			t.Errorf("%s: the cost = %s", c.name, runs[0].Cost)
		}
		if c.opts.plan != "" && !strings.Contains(out.String(), "+2 ~1 -2, 4 resources") {
			t.Errorf("%s: %q", c.name, out.String())
		}
	}
}

// TestReportRefused: a report Quai refuses, or cannot send, fails with why.
func TestReportRefused(t *testing.T) {
	server, _ := quaiForReports(t)
	vars := map[string]string{"QUAI_URL": server, "QUAI_REPOSITORY": "infra-live", "QUAI_RUN_TOKEN": reportToken}
	base := reportOptions{stack: "envs/prod", kind: "apply", status: "succeeded", commit: "abcdef1"}
	for name, c := range map[string]struct {
		change func(*reportOptions, map[string]string)
		want   string
	}{
		"a wrong token":  {func(_ *reportOptions, v map[string]string) { v["QUAI_RUN_TOKEN"] = "wrong" }, "refused by Quai: 401 Unauthorized: invalid run token"},
		"a wrong kind":   {func(o *reportOptions, _ map[string]string) { o.kind = "drift" }, "refused by Quai: 422 Unprocessable Entity"},
		"no token":       {func(_ *reportOptions, v map[string]string) { delete(v, "QUAI_RUN_TOKEN") }, "no run token"},
		"no commit":      {func(o *reportOptions, _ map[string]string) { o.commit = "" }, "no commit"},
		"no server":      {func(_ *reportOptions, v map[string]string) { delete(v, "QUAI_URL") }, "no Quai"},
		"a plan file":    {func(o *reportOptions, _ map[string]string) { o.plan = "report_test.go" }, "is not JSON"},
		"a missing file": {func(o *reportOptions, _ map[string]string) { o.tokenFile = "nope" }, "nope"},
		"a cost alone":   {func(o *reportOptions, _ map[string]string) { o.costDelta = "12" }, "go together"},
		"a wrong cost":   {func(o *reportOptions, _ map[string]string) { o.costDelta, o.costCurrency = "much", "CHF" }, "--cost-monthly-delta"},
	} {
		o, v := base, map[string]string{}
		for k, val := range vars {
			v[k] = val
		}
		if name == "a missing file" {
			delete(v, "QUAI_RUN_TOKEN")
		}
		c.change(&o, v)
		err := o.run(t.Context(), io.Discard, env(v))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Through the command: the required flags, and a refusal is an error (exit status 1).
	t.Setenv("QUAI_URL", server)
	t.Setenv("QUAI_REPOSITORY", "infra-live")
	t.Setenv("QUAI_RUN_TOKEN", "wrong")
	cmd := newRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"report", "--stack", "envs/prod", "--kind", "apply"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `"status" not set`) {
		t.Errorf("without --status = %v", err)
	}
	cmd = newRootCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"report", "--stack", "envs/prod", "--kind", "apply", "--status", "failed", "--commit", "abcdef1"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("a refused report = %v", err)
	}
}

func TestReportRetries(t *testing.T) {
	reportRetries = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { reportRetries = []time.Duration{2 * time.Second, 5 * time.Second} })
	// Nothing listens: the report is tried three times, then fails.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	_ = ln.Close()
	o := reportOptions{stack: "envs/prod", kind: "apply", status: "succeeded", commit: "abcdef1"}
	err := o.run(t.Context(), io.Discard, env(map[string]string{"QUAI_URL": "http://" + addr, "QUAI_REPOSITORY": "infra-live", "QUAI_RUN_TOKEN": "t"}))
	if err == nil || !strings.Contains(err.Error(), "could not reach Quai") {
		t.Errorf("unreachable = %v", err)
	}
}

// TestRunnerTemplates: the CI templates of deploy/runners parse, and call quai report with flags
// it has.
func TestRunnerTemplates(t *testing.T) {
	flag := regexp.MustCompile(`--([a-z][a-z-]*)`)
	for _, file := range []string{"azure-pipelines/quai-report.yml", "github-actions/action.yml", "gitlab-ci/quai-report.yml"} {
		b, err := os.ReadFile("../../../deploy/runners/" + file)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil || len(doc) == 0 {
			t.Errorf("%s: %v", file, err)
		}
		cmd := newReportCmd()
		seen := map[string]bool{}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "report") && !strings.Contains(line, "args+=") {
				continue
			}
			for _, m := range flag.FindAllStringSubmatch(line, -1) {
				seen[m[1]] = true
				if cmd.Flags().Lookup(m[1]) == nil {
					t.Errorf("%s: quai report has no --%s", file, m[1])
				}
			}
		}
		for _, required := range []string{"stack", "kind", "status", "plan"} {
			if !seen[required] {
				t.Errorf("%s: no --%s", file, required)
			}
		}
	}
}
