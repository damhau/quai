package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store"
)

func newMigrateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply the database migrations",
		Long: "Applies the migrations embedded in this binary that the database does not have yet, connected as the\n" +
			"schema's owner (database.migrateURL, or database.url), then grants the application role database.url\n" +
			"connects as what the server and the worker need: the audit log to read and append only.\n" +
			"Run it before starting a new version (the Helm chart runs it as a pre-upgrade hook).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath, os.LookupEnv)
			if err != nil {
				return err
			}
			if cfg.Database.URL == "" {
				return errors.New("no database: set database.url or QUAI_DATABASE_URL")
			}
			owner := cfg.Database.MigrateURL
			if owner == "" {
				owner = cfg.Database.URL
			}
			s, err := store.Open(cmd.Context(), owner)
			if err != nil {
				return err
			}
			defer s.Close()
			n, err := store.Migrate(cmd.Context(), s.Pool)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d migrations applied\n", n)
			return grantApplication(cmd.Context(), s, cfg.Database.URL, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "configuration file (YAML)")
	return cmd
}

// grantApplication grants the role of database.url what the server and the worker need, unless
// it is the owner's own: then it warns that the audit log is not protected (open question 19).
func grantApplication(ctx context.Context, s *store.Store, appURL string, out, warn io.Writer) error {
	app, err := pgx.ParseConfig(appURL)
	if err != nil {
		return fmt.Errorf("database.url: %w", err)
	}
	var owner string
	if err := s.Pool.QueryRow(ctx, "SELECT current_user").Scan(&owner); err != nil {
		return err
	}
	if app.User == owner {
		fmt.Fprintf(warn, "warning: the server and the worker connect as %s, the schema's owner: the audit log is not protected. "+
			"Set database.migrateURL to the owner and database.url to an application role.\n", owner)
		return nil
	}
	if err := store.Grant(ctx, s.Pool, app.User); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s may read and append the audit log, and not change it\n", app.User)
	return nil
}
