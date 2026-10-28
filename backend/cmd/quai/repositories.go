package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
	"github.com/damhau/quai/backend/internal/store"
)

// secretStore resolves secret references from Quai's environment and from Kubernetes Secrets.
func secretStore(cfg *config.Config) *secrets.Store {
	return secrets.New(map[string]secrets.Backend{
		"env": secrets.Env{Lookup: os.LookupEnv},
		"k8s": secrets.NewKubernetes(secrets.KubeConfig(cfg.Secrets.Kubernetes.Kubeconfig)),
	})
}

// openRepositories connects the repositories. With a database, the Repository documents of the
// configuration are reconciled into it first, and the registry follows the database, reloading
// what changes; without one, it holds the configuration's documents. It returns nil when there is
// neither.
func openRepositories(ctx context.Context, cfg *config.Config, s *store.Store, log *slog.Logger) (*repositories.Registry, error) {
	docs, err := repositories.LoadFiles(cfg.Repositories.Path)
	if err != nil {
		return nil, err
	}
	if s == nil {
		if len(docs) == 0 {
			return nil, nil
		}
		return repositories.Open(ctx, cfg, repositories.Static(docs), secretStore(cfg), log)
	}
	if err := repositories.Reconcile(ctx, s, docs); err != nil {
		return nil, err
	}
	reg, err := repositories.Open(ctx, cfg, repositories.FromStore(s), secretStore(cfg), log)
	if err != nil {
		return nil, err
	}
	go reg.Watch(ctx, repositories.ReloadInterval)
	return reg, nil
}
