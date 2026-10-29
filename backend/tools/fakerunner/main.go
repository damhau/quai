// Command fakerunner is the runner of the end-to-end scenarios (docs/engineering/testing.md): it
// listens to Gitea's webhooks for a repository and reports to Quai with `quai report`, as the
// repository's pipelines would. A pull request opened or updated is a plan of each stack it
// changes, a push to main an apply of each stack it changes; plans carry a recorded plan
// (FAKERUNNER_PLAN). A request's reason picks what happens: "[costly]" makes its plan cost
// 900 CHF a month (40 otherwise), "[fail-apply]" fails its apply. Each run has a page here.
//
// Environment: QUAI_URL, QUAI_REPOSITORY and QUAI_RUN_TOKEN_FILE for quai report; GITEA_URL
// and GITEA_TOKEN_FILE to read a pull request's files; FAKERUNNER_URL, its own address in the
// links it reports (http://fake-runner:8090 by default).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type runner struct {
	gitea, giteaToken, self, plan string
	log                           *slog.Logger
	boot                          string     // in each run's page: a run is its page, and pages must not repeat across restarts
	mu                            sync.Mutex // one hook at a time, in order
	runs                          []string   // what each run did, by its number
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	token, err := os.ReadFile(env("GITEA_TOKEN_FILE", "/run/quai/git-token"))
	if err != nil {
		log.Error("the Gitea token", slog.Any("error", err))
		os.Exit(1)
	}
	r := &runner{gitea: env("GITEA_URL", "http://gitea:3000"), giteaToken: strings.TrimSpace(string(token)), log: log, boot: strconv.FormatInt(time.Now().UnixMilli(), 36),
		self: env("FAKERUNNER_URL", "http://fake-runner:8090"), plan: env("FAKERUNNER_PLAN", "/etc/fakerunner/plan.json")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /hooks", r.hook)
	mux.HandleFunc("GET /runs/{boot}/{n}", r.page)
	srv := &http.Server{Addr: env("FAKERUNNER_LISTEN", ":8090"), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Info("listening", slog.String("addr", srv.Addr))
	if err := srv.ListenAndServe(); err != nil {
		log.Error("serve", slog.Any("error", err))
		os.Exit(1)
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// hook takes a Gitea webhook and reports what it means, after answering.
func (r *runner) hook(w http.ResponseWriter, req *http.Request) {
	var e struct {
		Action      string `json:"action"`
		Ref         string `json:"ref"`
		After       string `json:"after"`
		PullRequest struct {
			Number int    `json:"number"`
			Body   string `json:"body"`
			Head   struct {
				Sha string `json:"sha"`
			} `json:"head"`
		} `json:"pull_request"`
		Commits []struct {
			Message  string   `json:"message"`
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<20)).Decode(&e); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	event := req.Header.Get("X-Gitea-Event")
	go func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		switch {
		case event == "pull_request" && slices.Contains([]string{"opened", "synchronized", "reopened"}, e.Action):
			pr := e.PullRequest
			stacks, err := r.pullRequestStacks(ctx, e.Repository.FullName, pr.Number)
			if err != nil {
				r.log.Error("the pull request's files", slog.Int("pull_request", pr.Number), slog.Any("error", err))
				return
			}
			cost := "40"
			if strings.Contains(pr.Body, "[costly]") {
				cost = "900"
			}
			for _, stack := range stacks {
				r.report(ctx, stack, "plan", pr.Head.Sha, strconv.Itoa(pr.Number), "succeeded", "--plan", r.plan, "--cost-monthly-delta", cost, "--cost-currency", "CHF")
			}
		case event == "push" && e.Ref == "refs/heads/main":
			var paths []string
			status := "succeeded"
			for _, c := range e.Commits {
				paths = append(append(append(paths, c.Added...), c.Modified...), c.Removed...)
				if strings.Contains(c.Message, "[fail-apply]") {
					status = "failed"
				}
			}
			for _, stack := range stacksOf(paths) {
				r.report(ctx, stack, "apply", e.After, "", status)
			}
		}
	}()
}

// report runs quai report for a run: running, then its result.
func (r *runner) report(ctx context.Context, stack, kind, commit, pullRequest, status string, extra ...string) {
	r.runs = append(r.runs, fmt.Sprintf("%s of %s at %s: %s", kind, stack, commit, status))
	page := fmt.Sprintf("%s/runs/%s/%d", r.self, r.boot, len(r.runs))
	for _, s := range []string{"running", status} {
		args := []string{"report", "--stack", stack, "--kind", kind, "--status", s, "--commit", commit, "--url", page}
		if pullRequest != "" {
			args = append(args, "--pull-request", pullRequest)
		}
		if s != "running" {
			args = append(args, extra...)
		}
		out, err := exec.CommandContext(ctx, "quai", args...).CombinedOutput() // #nosec G204 -- quai, with the arguments above
		r.log.Info("quai report", slog.String("kind", kind), slog.String("stack", stack), slog.String("status", s), slog.String("output", strings.TrimSpace(string(out))), slog.Any("error", err))
	}
}

// pullRequestStacks returns the stacks a pull request's files are in, from Gitea.
func (r *runner) pullRequestStacks(ctx context.Context, repo string, number int) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/repos/%s/pulls/%d/files", r.gitea, repo, number), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+r.giteaToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gitea: %s", resp.Status)
	}
	var files []struct {
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&files); err != nil {
		return nil, err
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Filename)
	}
	return stacksOf(paths), nil
}

// stacksOf returns the stacks of paths: envs/<env> for what is under it.
func stacksOf(paths []string) []string {
	var out []string
	for _, p := range paths {
		parts := strings.SplitN(p, "/", 3)
		if len(parts) == 3 && parts[0] == "envs" && !slices.Contains(out, "envs/"+parts[1]) {
			out = append(out, "envs/"+parts[1])
		}
	}
	slices.Sort(out)
	return out
}

// page shows what a run did.
func (r *runner) page(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := strconv.Atoi(req.PathValue("n"))
	if err != nil || req.PathValue("boot") != r.boot || n < 1 || n > len(r.runs) {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, "<!doctype html><title>Run %d</title><p>Run %d (the end-to-end scenarios' fake runner): %s</p>\n", n, n, html.EscapeString(r.runs[n-1]))
}
