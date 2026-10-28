package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/azuredevops"
	"github.com/damhau/quai/backend/internal/connectors/generic"
	"github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/netx"
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

// connectorRegistry makes the connectors, their API calls going through network (nil: the
// defaults).
func connectorRegistry(network *netx.Network, log *slog.Logger) *connectors.Registry {
	var base http.RoundTripper = http.DefaultTransport
	if network != nil {
		base = network.Transport()
	}
	return &connectors.Registry{
		Factories: map[string]connectors.Factory{"generic": generic.New, "gitea": gitea.New, "azure-devops": azuredevops.New},
		Fallback:  generic.Fallback,
		HTTP:      connectors.NewHTTPClient(base, log),
		Log:       log,
	}
}

func repositoryOptions(cfg *config.Config, network *netx.Network, log *slog.Logger) repositories.Options {
	return repositories.Options{Secrets: secretStore(cfg), Network: network, Connectors: connectorRegistry(network, log), Log: log}
}

// openRepositories connects the repositories. With a database, the Repository documents of the
// configuration are reconciled into it first, and the registry follows the database, reloading
// what changes; without one, it holds the configuration's documents. It returns nil when there is
// neither.
func openRepositories(ctx context.Context, cfg *config.Config, s *store.Store, network *netx.Network, log *slog.Logger) (*repositories.Registry, error) {
	docs, err := repositories.LoadFiles(cfg.Repositories.Path)
	if err != nil {
		return nil, err
	}
	if s == nil {
		if len(docs) == 0 {
			return nil, nil
		}
		return repositories.Open(ctx, cfg, repositories.Static(docs), repositoryOptions(cfg, network, log))
	}
	if err := repositories.Reconcile(ctx, s, docs); err != nil {
		return nil, err
	}
	reg, err := repositories.Open(ctx, cfg, repositories.FromStore(s), repositoryOptions(cfg, network, log))
	if err != nil {
		return nil, err
	}
	go reg.Watch(ctx, repositories.ReloadInterval)
	return reg, nil
}
