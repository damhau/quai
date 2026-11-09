package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/riverqueue/river"
	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/audit/keeper"
	"github.com/damhau/quai/backend/internal/audit/siem"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/gitx"
	"github.com/damhau/quai/backend/internal/ingest"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/netx"
	"github.com/damhau/quai/backend/internal/notify"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/telemetry"
)

func newWorkerCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Run background jobs",
		Long: "Works the job queue in PostgreSQL and runs the periodic jobs. Run several for\n" +
			"availability: jobs are shared, and a job left by a worker that dies is run again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath, os.LookupEnv)
			if err != nil {
				return err
			}
			if cfg.Database.URL == "" {
				return errors.New("no database: set database.url or QUAI_DATABASE_URL")
			}
			log := cfg.Logger(cmd.ErrOrStderr())
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			s, err := store.Open(ctx, cfg.Database.URL)
			if err != nil {
				return err
			}
			defer s.Close()
			network, err := netx.New(cfg.Network.CAFile)
			if err != nil {
				return err
			}
			shutdown, err := telemetry.Setup(ctx, cfg.Telemetry, version, network)
			if err != nil {
				return err
			}
			defer func() {
				if err := shutdown(context.WithoutCancel(ctx)); err != nil {
					log.Error("telemetry shutdown", slog.Any("error", err))
				}
			}()
			return work(ctx, cfg, log, s, network)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "configuration file (YAML)")
	return cmd
}

// work runs the job queue until ctx ends, then lets jobs in flight finish within the shutdown
// timeout before cancelling them, and signs a last audit checkpoint; cancelled jobs run again
// later. It exports the audit log to the SIEMs audit.export names. Outgoing connections go
// through network (nil: the defaults).
func work(ctx context.Context, cfg *config.Config, log *slog.Logger, s *store.Store, network *netx.Network) error {
	opts := jobs.Options{Logger: log}
	var svc *requests.Service
	var hooks *ingest.Service
	var n *notify.Service
	reg, err := openRepositories(ctx, cfg, s, network, log)
	if err != nil {
		return err
	}
	sec := secretStore(cfg)
	keep := keeper.New(keeper.Options{Store: s, Config: cfg.Audit, Secrets: sec, Repositories: reg,
		Committer: gitx.Signature{Name: cfg.Git.Committer.Name, Email: cfg.Git.Committer.Email}, Log: log})
	if !keep.Signs() {
		log.Warn("audit.checkpoints.signingKey.ref is not set: no audit checkpoint is signed, so the audit log is not pruned " +
			"and its verification cannot tell a rewritten chain (docs/operations/deployment-and-operations.md#the-checkpoint-key)")
	}
	workers := []func(*river.Workers){keep.Workers}
	opts.Periodic = append(opts.Periodic, keep.Periodic()...)
	if len(cfg.Audit.Export) > 0 {
		exp, err := siem.New(siem.Options{Store: s, Destinations: cfg.Audit.Export, Secrets: sec, Network: network, Log: log,
			Service: cfg.Telemetry.ServiceName, Version: version})
		if err != nil {
			return err
		}
		workers = append(workers, exp.Workers)
		opts.Periodic = append(opts.Periodic, exp.Periodic()...)
		telemetry.ObserveAuditExport(exp.Lag)
	}
	if reg != nil {
		defer reg.Close()
		svc = requests.New(s, reg, nil, log)
		svc.UsePublicURL(cfg.Server.PublicURL)
		svc.UseDirectory(auth.NewDirectory(cfg.Auth))
		svc.UseStepUp(auth.NewStepUp(cfg.Auth, s))
		hooks = ingest.New(s, reg, log)
		hooks.UseHandler(svc)
		workers = append(workers, svc.Workers, hooks.Workers)
		if n, err = notifications(cfg, s, reg, svc, network, log); err != nil {
			return err
		}
		if n != nil {
			workers = append(workers, n.Workers)
			opts.Periodic = append(opts.Periodic, notify.Periodic())
		}
		opts.Periodic = append(opts.Periodic, ingest.Periodic())
	}
	opts.Workers = func(w *river.Workers) {
		for _, add := range workers {
			add(w)
		}
	}
	c, err := jobs.NewWorker(s.Pool, opts)
	if err != nil {
		return err
	}
	if svc != nil {
		svc.UseJobs(c)
		hooks.UseJobs(c)
		if n != nil {
			n.UseJobs(c)
		}
	}
	if err := c.Start(ctx); err != nil {
		return err
	}
	log.Info("working", slog.String("version", version))
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := c.Stop(sctx); err != nil {
		log.Warn("jobs still running at shutdown; cancelling them", slog.Any("error", err))
		if err := c.StopAndCancel(context.WithoutCancel(ctx)); err != nil {
			return err
		}
	}
	if err := keep.Shutdown(sctx); err != nil {
		log.Error("the audit checkpoint at shutdown", slog.Any("error", err))
	}
	log.Info("stopped")
	return nil
}
