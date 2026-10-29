// Package pipelinestest gives tests a platform's pipelines without a platform: Connector wraps
// any connector with Pipelines, which record what is triggered and answer with the runs they
// queued, their pages under https://ci.example.com.
package pipelinestest

import (
	"context"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"

	"github.com/damhau/quai/backend/internal/connectors"
)

// Pipelines are fake pipelines.
type Pipelines struct {
	mu   sync.Mutex
	runs []connectors.Run
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

// PullRequestRuns returns none.
func (p *Pipelines) PullRequestRuns(context.Context, string) ([]connectors.Run, error) { return nil, nil }

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
