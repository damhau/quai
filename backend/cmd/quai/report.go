package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/netx"
)

// reportOptions are what `quai report` sends (docs/design/06-runs-status-and-drift.md#run-report-protocol).
type reportOptions struct {
	server, repository, stack, kind, status string
	plan, commit, pullRequest, url          string
	tokenFile, caFile                       string
}

// maxPlan is the largest plan Quai accepts with a report.
const maxPlan = 60 << 20

// reportRetries are the waits before each retry of a report Quai could not take.
var reportRetries = []time.Duration{2 * time.Second, 5 * time.Second}

func newReportCmd() *cobra.Command {
	var o reportOptions
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Report a plan, apply, sync or drift check to Quai, from a pipeline",
		Long: "Posts a run report to Quai (POST /api/v1/runs), with the plan when --plan names the\n" +
			"output of terraform show -json. The commit, the pull request and the run's URL come from\n" +
			"the CI's environment (Azure Pipelines, GitHub Actions, GitLab CI) unless flags say them.\n\n" +
			"Environment: QUAI_URL (--server), QUAI_REPOSITORY (--repository), QUAI_RUN_TOKEN, or\n" +
			"QUAI_RUN_TOKEN_FILE (--token-file) naming a file that holds it, and QUAI_CA_FILE\n" +
			"(--ca-file) for an internal CA. The token is never a flag. Exits with status 1 when Quai\n" +
			"refuses the report.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return o.run(cmd.Context(), cmd.OutOrStdout(), os.Getenv)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.server, "server", "", "Quai's URL, such as https://quai.example.com (QUAI_URL)")
	f.StringVar(&o.repository, "repository", "", "the repository's name in Quai (QUAI_REPOSITORY)")
	f.StringVar(&o.stack, "stack", "", "the stack the run is for, such as envs/prod")
	f.StringVar(&o.kind, "kind", "", "plan, apply, sync or drift-check")
	f.StringVar(&o.status, "status", "", "running, succeeded or failed")
	f.StringVar(&o.plan, "plan", "", "the plan, as terraform show -json writes it")
	f.StringVar(&o.commit, "commit", "", "the commit the run is at (default: the CI's)")
	f.StringVar(&o.pullRequest, "pull-request", "", "the pull request the run is for (default: the CI's)")
	f.StringVar(&o.url, "url", "", "the run's page (default: the CI's)")
	f.StringVar(&o.tokenFile, "token-file", "", "a file holding the run token (QUAI_RUN_TOKEN_FILE)")
	f.StringVar(&o.caFile, "ca-file", "", "PEM CA certificates to trust besides the system's (QUAI_CA_FILE)")
	for _, name := range []string{"stack", "kind", "status"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func (o reportOptions) run(ctx context.Context, out io.Writer, getenv func(string) string) error {
	o.server, o.repository = or(o.server, getenv("QUAI_URL")), or(o.repository, getenv("QUAI_REPOSITORY"))
	o.tokenFile, o.caFile = or(o.tokenFile, getenv("QUAI_RUN_TOKEN_FILE")), or(o.caFile, getenv("QUAI_CA_FILE"))
	ci := ciRun(getenv)
	o.commit, o.pullRequest, o.url = or(o.commit, ci.commit), or(o.pullRequest, ci.pullRequest), or(o.url, ci.url)
	switch {
	case o.server == "":
		return errors.New("no Quai: set --server or QUAI_URL")
	case o.repository == "":
		return errors.New("no repository: set --repository or QUAI_REPOSITORY")
	case o.commit == "":
		return errors.New("no commit: set --commit, or run in Azure Pipelines, GitHub Actions or GitLab CI")
	}
	token, err := runToken(getenv("QUAI_RUN_TOKEN"), o.tokenFile)
	if err != nil {
		return err
	}
	body := map[string]any{"repository": o.repository, "stack": o.stack, "commit": o.commit, "kind": o.kind, "status": o.status}
	for k, v := range map[string]string{"pullRequest": o.pullRequest, "url": o.url} {
		if v != "" {
			body[k] = v
		}
	}
	if o.plan != "" {
		content, err := readPlan(o.plan)
		if err != nil {
			return err
		}
		body["plan"] = map[string]any{"format": "terraform-json", "content": content}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	network, err := netx.New(o.caFile)
	if err != nil {
		return err
	}
	endpoint, err := url.JoinPath(o.server, "/api/v1/runs")
	if err != nil {
		return fmt.Errorf("--server: %w", err)
	}
	status, answer, err := post(ctx, network.Client(), endpoint, token, b)
	if err != nil {
		return err
	}
	if status != http.StatusAccepted {
		return fmt.Errorf("refused by Quai: %s", refusal(status, answer))
	}
	var run struct {
		ID            string `json:"id"`
		RequestNumber int64  `json:"requestNumber"`
		Summary       *struct{ Add, Change, Destroy int }
		Plan          *struct {
			Resources []json.RawMessage `json:"resources"`
			Unmapped  []json.RawMessage `json:"unmapped"`
		} `json:"plan"`
	}
	_ = json.Unmarshal(answer, &run)
	line := fmt.Sprintf("reported %s %s for %s at %s: run %s", o.kind, o.status, o.stack, short(o.commit), run.ID)
	if run.RequestNumber != 0 {
		line += fmt.Sprintf(", request #%d", run.RequestNumber)
	}
	if run.Summary != nil {
		line += fmt.Sprintf(", +%d ~%d -%d", run.Summary.Add, run.Summary.Change, run.Summary.Destroy)
	}
	if run.Plan != nil {
		line += fmt.Sprintf(", %d resources", len(run.Plan.Resources))
		if n := len(run.Plan.Unmapped); n > 0 {
			line += fmt.Sprintf(" (%d Terraform resources no type claims)", n)
		}
	}
	_, err = fmt.Fprintln(out, line)
	return err
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// ci is what a CI says of the run it is in.
type ci struct{ commit, pullRequest, url string }

var githubPullRef = regexp.MustCompile(`^refs/pull/([0-9]+)/merge$`)

// ciRun reads the commit, the pull request and the run's page from the CI's variables.
func ciRun(getenv func(string) string) ci {
	switch {
	case strings.EqualFold(getenv("TF_BUILD"), "true"): // Azure Pipelines
		c := ci{commit: getenv("BUILD_SOURCEVERSION"), pullRequest: or(getenv("SYSTEM_PULLREQUEST_PULLREQUESTID"), getenv("SYSTEM_PULLREQUEST_PULLREQUESTNUMBER"))}
		if base, project, build := getenv("SYSTEM_COLLECTIONURI"), getenv("SYSTEM_TEAMPROJECT"), getenv("BUILD_BUILDID"); base != "" && project != "" && build != "" {
			c.url = strings.TrimSuffix(base, "/") + "/" + url.PathEscape(project) + "/_build/results?buildId=" + url.QueryEscape(build)
		}
		return c
	case getenv("GITHUB_ACTIONS") == "true":
		c := ci{commit: getenv("GITHUB_SHA")}
		if m := githubPullRef.FindStringSubmatch(getenv("GITHUB_REF")); m != nil {
			c.pullRequest = m[1]
		}
		if server, repo, id := getenv("GITHUB_SERVER_URL"), getenv("GITHUB_REPOSITORY"), getenv("GITHUB_RUN_ID"); server != "" && repo != "" && id != "" {
			c.url = server + "/" + repo + "/actions/runs/" + id
			if attempt := getenv("GITHUB_RUN_ATTEMPT"); attempt != "" {
				c.url += "/attempts/" + attempt
			}
		}
		return c
	case getenv("GITLAB_CI") == "true":
		return ci{commit: getenv("CI_COMMIT_SHA"), pullRequest: getenv("CI_MERGE_REQUEST_IID"), url: getenv("CI_PIPELINE_URL")}
	}
	return ci{}
}

// runToken is the token from the environment, or the file that holds it.
func runToken(fromEnv, file string) (string, error) {
	if fromEnv != "" {
		return strings.TrimSpace(fromEnv), nil
	}
	if file == "" {
		return "", errors.New("no run token: set QUAI_RUN_TOKEN, or QUAI_RUN_TOKEN_FILE or --token-file to a file holding it")
	}
	b, err := os.ReadFile(file) // #nosec G304 -- the file the user names
	if err != nil {
		return "", fmt.Errorf("the run token: %w", err)
	}
	if t := strings.TrimSpace(string(b)); t != "" {
		return t, nil
	}
	return "", fmt.Errorf("the run token: %s is empty", file)
}

// readPlan reads a plan file, which must be JSON.
func readPlan(file string) (json.RawMessage, error) {
	f, err := os.Open(file) // #nosec G304 -- the file the user names
	if err != nil {
		return nil, fmt.Errorf("the plan: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxPlan+1))
	if err != nil {
		return nil, fmt.Errorf("the plan: %w", err)
	}
	if len(b) > maxPlan {
		return nil, fmt.Errorf("the plan: %s is larger than %d MiB", file, maxPlan>>20)
	}
	if !json.Valid(b) {
		return nil, fmt.Errorf("the plan: %s is not JSON: write it with terraform show -json", file)
	}
	return b, nil
}

// post sends a report, again after a failure to connect or an answer that Quai is unavailable.
func post(ctx context.Context, client *http.Client, endpoint, token string, body []byte) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "quai-report/"+version)
		resp, err := client.Do(req)
		var status int
		var answer []byte
		if err == nil {
			status = resp.StatusCode
			answer, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
		}
		retry := err != nil || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
		if !retry || attempt == len(reportRetries) {
			if err != nil {
				return 0, nil, fmt.Errorf("could not reach Quai: %w", err)
			}
			return status, answer, nil
		}
		select {
		case <-ctx.Done():
			return 0, nil, ctx.Err()
		case <-time.After(reportRetries[attempt]):
		}
	}
}

// refusal says why Quai refused a report, from its problem details (RFC 9457).
func refusal(status int, answer []byte) string {
	var p struct {
		Detail string `json:"detail"`
		Errors []struct {
			Message, Location string
		} `json:"errors"`
	}
	msg := fmt.Sprintf("%d %s", status, http.StatusText(status))
	if json.Unmarshal(answer, &p) != nil {
		return msg
	}
	if p.Detail != "" {
		msg += ": " + p.Detail
	}
	for _, e := range p.Errors {
		msg += "; " + strings.TrimPrefix(e.Location+": ", ": ") + e.Message
	}
	return msg
}
