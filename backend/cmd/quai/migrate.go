package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store"
)

func newMigrateCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply the database migrations",
		Long: "Applies the migrations embedded in this binary that the database does not have yet.\n" +
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
			s, err := store.Open(cmd.Context(), cfg.Database.URL)
			if err != nil {
				return err
			}
			defer s.Close()
			n, err := store.Migrate(cmd.Context(), s.Pool)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d migrations applied\n", n)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "configuration file (YAML)")
	return cmd
}
