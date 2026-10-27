package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/store"
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
			return work(ctx, cfg, log, s)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "configuration file (YAML)")
	return cmd
}

// work runs the job queue until ctx ends, then lets jobs in flight finish within the shutdown
// timeout before cancelling them; cancelled jobs run again later.
func work(ctx context.Context, cfg *config.Config, log *slog.Logger, s *store.Store) error {
	c, err := jobs.NewWorker(s.Pool, jobs.Options{Logger: log})
	if err != nil {
		return err
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
	log.Info("stopped")
	return nil
}
