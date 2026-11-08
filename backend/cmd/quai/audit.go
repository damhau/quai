package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store"
)

func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Export and verify the audit log",
	}
	cmd.AddCommand(newAuditExportCmd(), newAuditVerifyCmd())
	return cmd
}

func newAuditExportCmd() *cobra.Command {
	var configPath, output string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write the audit log, to verify it offline",
		Long: "Writes the audit log in JSON lines, as it is at one moment: its signed checkpoints, then its\n" +
			"events in seq order, with personal data as pseudonyms. quai audit verify checks it with the\n" +
			"checkpoint key's public key. Connects as database.url.",
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
			w := cmd.OutOrStdout()
			var f *os.File
			if output != "" && output != "-" {
				if f, err = os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil { // #nosec G304 -- the file the user names
					return err
				}
				w = f
			}
			n, err := audit.Export(cmd.Context(), s.Pool, w)
			if f != nil {
				err = errors.Join(err, f.Close())
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%d audit events exported\n", n)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "configuration file (YAML)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "the file to write, which must not exist (default: standard output)")
	return cmd
}

func newAuditVerifyCmd() *cobra.Command {
	var keyFiles, anchorPaths []string
	cmd := &cobra.Command{
		Use:   "verify [export]",
		Short: "Verify an export of the audit log, offline",
		Long: "Verifies an export of the audit log (quai audit export, or GET /api/v1/audit/export; standard\n" +
			"input without a file, or -): each event's hash, each link to the event before, each checkpoint's\n" +
			"signature with the public keys --key gives, and that each checkpoint is its event's hash. With\n" +
			"--anchors, the checkpoints committed to the audit repository too: one beyond the export's last\n" +
			"event means events were lost. Names the first seq where the log fails, and exits with status 1.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keys := audit.Keys{}
			for _, f := range keyFiles {
				b, err := os.ReadFile(f) // #nosec G304 -- the file the user names
				if err != nil {
					return err
				}
				pubs, err := audit.ParsePublicKeys(b)
				if err != nil {
					return fmt.Errorf("--key %s: %w", f, err)
				}
				keys.Add(pubs...)
			}
			v := audit.NewVerifier(keys)
			anchors := 0
			for _, p := range anchorPaths {
				n, err := readAnchors(p, v)
				if err != nil {
					return fmt.Errorf("--anchors %s: %w", p, err)
				}
				anchors += n
			}
			in, name := cmd.InOrStdin(), "standard input"
			if len(args) == 1 && args[0] != "-" {
				f, err := os.Open(args[0]) // #nosec G304 -- the file the user names
				if err != nil {
					return err
				}
				defer func() { _ = f.Close() }()
				in, name = f, args[0]
			}
			if err := audit.ReadExport(in, v); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			res := v.Result()
			if res.Broken != nil {
				return res.Broken
			}
			if res.Events == 0 {
				return fmt.Errorf("%s holds no audit event", name)
			}
			out := cmd.OutOrStdout()
			from := "from seq 1"
			if res.Start > 0 {
				from = fmt.Sprintf("from the checkpoint at seq %d (pruned before)", res.Start)
			}
			fmt.Fprintf(out, "%d events verified %s to %d, %s matching their events", res.Events, from, res.Last, plural(res.Checkpoints, "checkpoint"))
			if len(anchorPaths) > 0 {
				fmt.Fprintf(out, ", with %s from the audit repository", plural(anchors, "checkpoint"))
			}
			fmt.Fprintln(out)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&keyFiles, "key", nil, "a PEM file of public keys checkpoints are signed with (openssl pkey -pubout); repeat it for the keys before a rotation")
	cmd.Flags().StringArrayVar(&anchorPaths, "anchors", nil, "a checkout of the audit repository, or a checkpoint file from it")
	return cmd
}

// readAnchors adds the checkpoints of the audit repository at path, a checkout or one of its
// files, and returns how many.
func readAnchors(path string, v *audit.Verifier) (int, error) {
	var files []string
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == ".git":
			return filepath.SkipDir
		case !d.IsDir() && (p == path || strings.HasSuffix(p, ".json")):
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- a file of the checkout the user names
		if err != nil {
			return 0, err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var c audit.Checkpoint
		if err := dec.Decode(&c); err != nil {
			return 0, fmt.Errorf("%s is not a checkpoint: %w", f, err)
		}
		if c.Type != audit.CheckpointType || dec.More() {
			return 0, fmt.Errorf("%s is not a checkpoint: it must hold one, as Quai commits them", f)
		}
		if err := v.Checkpoint(c, audit.FromRepository); err != nil {
			return 0, err
		}
	}
	return len(files), nil
}
