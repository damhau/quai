// Package pipelinestest gives tests a platform's pipelines without a platform: Connector wraps
// any connector with Pipelines, which record what is triggered and answer with the runs they
// queued, their pages under https://ci.example.com, and with the build validation runs a test
// gives a pull request.
package pipelinestest

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/damhau/quai/backend/internal/connectors"
)

// Pipelines are fake pipelines.
type Pipelines struct {
	mu        sync.Mutex
	runs      []connectors.Run
	validated map[string][]string // a pull request's build validation runs, by ID
}

// Trigger queues a run.
func (p *Pipelines) Trigger(_ context.Context, pipeline, branch string, parameters map[string]string) (connectors.Run, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := strconv.Itoa(len(p.runs) + 1)
	run := connectors.Run{Pipeline: pipeline, ID: id, State: "queued", Branch: "refs/heads/" + strings.TrimPrefix(branch, "refs/heads/"),
		URL: "https://ci.example.com/_build/results?buildId=" + id, Parameters: maps.Clone(parameters)}
	p.runs = append(p.runs, run)
	return run, nil
}

// Triggered returns the runs queued so far.
func (p *Pipelines) Triggered() []connectors.Run {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]connectors.Run(nil), p.runs...)
}

// Finish completes a run queued or validating, at commit, with result (succeeded, failed, …).
func (p *Pipelines) Finish(id, result, commit string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.runs {
		if p.runs[i].ID == id {
			p.runs[i].State, p.runs[i].Result, p.runs[i].Commit = "completed", result, commit
		}
	}
}

// Validate starts a run of pipeline as the build validation of a pull request, at commit.
func (p *Pipelines) Validate(pipeline, pullRequest, commit string) connectors.Run {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := strconv.Itoa(len(p.runs) + 1)
	run := connectors.Run{Pipeline: pipeline, ID: id, State: "running", Commit: commit, Branch: "refs/pull/" + pullRequest + "/merge",
		PullRequest: pullRequest, URL: "https://ci.example.com/_build/results?buildId=" + id}
	p.runs = append(p.runs, run)
	if p.validated == nil {
		p.validated = map[string][]string{}
	}
	p.validated[pullRequest] = append(p.validated[pullRequest], id)
	return run
}

// Get returns a run queued.
func (p *Pipelines) Get(_ context.Context, id string) (connectors.Run, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return connectors.Run{}, fmt.Errorf("run %s: %w", id, connectors.ErrNotFound)
}

// PullRequestRuns returns the build validation runs of a pull request.
func (p *Pipelines) PullRequestRuns(_ context.Context, pullRequest string) ([]connectors.Run, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []connectors.Run
	for _, id := range p.validated[pullRequest] {
		for _, r := range p.runs {
			if r.ID == id {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

// Runs returns the latest runs of pipeline on branch, newest first.
func (p *Pipelines) Runs(_ context.Context, pipeline, branch string, limit int) ([]connectors.Run, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []connectors.Run
	for _, r := range slices.Backward(p.runs) {
		if r.Pipeline == pipeline && r.Branch == "refs/heads/"+strings.TrimPrefix(branch, "refs/heads/") && len(out) < limit {
			out = append(out, r)
		}
	}
	return out, nil
}

// Artifact has none.
func (p *Pipelines) Artifact(context.Context, string, string, string) ([]byte, error) {
	return nil, connectors.ErrNotFound
}

// Check passes.
func (p *Pipelines) Check(context.Context, string) error { return nil }

// Connector is a connector with the fake pipelines.
type Connector struct {
	connectors.Connector
	P *Pipelines
}

// Pipelines returns the fake pipelines.
func (c Connector) Pipelines() connectors.Pipelines { return c.P }

// Factory makes connectors of factory with p.
func Factory(factory connectors.Factory, p *Pipelines) connectors.Factory {
	return func(cfg connectors.Config) (connectors.Connector, error) {
		c, err := factory(cfg)
		if err != nil {
			return nil, err
		}
		return Connector{Connector: c, P: p}, nil
	}
}
