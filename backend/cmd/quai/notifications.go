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

// notifications returns the notifications service of the configured channels (email, Microsoft
// Teams, Slack), hooked to the store's events and telling the requests service whom it reaches and how
// to check the links of approval cards in chat; nil when no channel is configured. The caller
// gives it its job client.
func notifications(cfg *config.Config, s *store.Store, reg *repositories.Registry, svc *requests.Service, network *netx.Network,
	log *slog.Logger) (*notify.Service, error) {
	n := cfg.Notifications
	if n.Email == nil && n.Teams == nil && n.Slack == nil {
		return nil, nil
	}
	sec := secretStore(cfg)
	var channels []notify.Channel
	if n.Email != nil {
		email, err := notify.NewEmail(*n.Email, network, sec, cfg.Server.PublicURL)
		if err != nil {
			return nil, err
		}
		channels = append(channels, email)
	}
	if n.Teams != nil {
		teams, err := notify.NewTeams(*n.Teams, network, sec)
		if err != nil {
			return nil, err
		}
		channels = append(channels, teams)
	}
	if n.Slack != nil {
		slack, err := notify.NewSlack(*n.Slack, network, sec)
		if err != nil {
			return nil, err
		}
		channels = append(channels, slack)
	}
	links := notify.Links(n.Links, sec)
	ns := notify.New(notify.Options{Store: s, Requests: svc, Repositories: reg, Directory: auth.NewDirectory(cfg.Auth),
		Channels: channels, Alerts: n.Alerts, PublicURL: cfg.Server.PublicURL, Log: log, Settings: notify.NewSettings(n), Links: links})
	s.UseEvents(ns.EventRecorded)
	svc.UseReach(ns.Reach)
	svc.UseLinks(links)
	return ns, nil
}
