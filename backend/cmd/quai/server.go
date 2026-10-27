package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/api"
	"github.com/damhau/quai/backend/internal/auth"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/jobs"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/telemetry"
)

func newServerCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the Quai server",
		Long: "Runs the HTTP server: the API under /api/v1 and /healthz. The configuration comes from\n" +
			"--config (or QUAI_CONFIG), overridden by QUAI_* environment variables.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath, os.LookupEnv)
			if err != nil {
				return err
			}
			log := cfg.Logger(cmd.ErrOrStderr())
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			shutdown, err := telemetry.Setup(ctx, cfg.Telemetry, version)
			if err != nil {
				return err
			}
			defer func() {
				if err := shutdown(context.WithoutCancel(ctx)); err != nil {
					log.Error("telemetry shutdown", slog.Any("error", err))
				}
			}()
			var s *store.Store
			if cfg.Database.URL != "" {
				if s, err = store.Open(ctx, cfg.Database.URL); err != nil {
					return err
				}
				defer s.Close()
			} else {
				log.Warn("no database configured: nobody can sign in")
			}
			ln, err := net.Listen("tcp", cfg.Server.Addr)
			if err != nil {
				return err
			}
			return serve(ctx, cfg, log, ln, s)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "configuration file (YAML)")
	return cmd
}

// serve runs the HTTP server on ln until ctx ends, then lets requests in flight finish within
// the shutdown timeout. Without a store, nobody can sign in.
func serve(ctx context.Context, cfg *config.Config, log *slog.Logger, ln net.Listener, s *store.Store) error {
	opts := api.Options{Version: version, Env: cfg.Env, Logger: log}
	if s != nil {
		opts.Store, opts.Auth = s, auth.New(cfg, s, log)
	}
	if len(cfg.Repositories) > 0 {
		reg, err := repositories.Open(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer reg.Close()
		opts.Repositories = reg
		if s != nil {
			ins, err := jobs.NewInserter(s.Pool, log)
			if err != nil {
				return err
			}
			opts.Requests = requests.New(s, reg, ins, log)
		}
	}
	srv := &http.Server{
		Handler:           api.New(opts),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	log.Info("listening", slog.String("addr", ln.Addr().String()), slog.String("env", cfg.Env), slog.String("version", version))
	select {
	case err := <-errc:
		return fmt.Errorf("server: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Server.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("server shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: %w", err)
	}
	log.Info("stopped")
	return nil
}
