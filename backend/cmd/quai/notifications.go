package main

import (
	"log/slog"

	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/notify"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
)

// notifications returns the notifications service of the configured channels, hooked to the
// store's events and telling the requests service whom it reaches; nil when no channel is
// configured. The caller gives it its job client.
func notifications(cfg *config.Config, s *store.Store, reg *repositories.Registry, svc *requests.Service, network *netx.Network,
	log *slog.Logger) (*notify.Service, error) {
	if cfg.Notifications.Email == nil {
		return nil, nil
	}
	email, err := notify.NewEmail(*cfg.Notifications.Email, network, secretStore(cfg), cfg.Server.PublicURL)
	if err != nil {
		return nil, err
	}
	n := notify.New(notify.Options{Store: s, Requests: svc, Repositories: reg, Directory: auth.NewDirectory(cfg.Auth),
		Channels: []notify.Channel{email}, Alerts: cfg.Notifications.Alerts, PublicURL: cfg.Server.PublicURL, Log: log})
	s.UseEvents(n.EventRecorded)
	svc.UseReach(n.Reach)
	return n, nil
}
