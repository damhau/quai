package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/engine/lint"
)

func newLintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint [directory]",
		Short: "Check a repository's Quai definitions and the files they describe",
		Long: "Checks that every definition under .quai/types and blueprints loads, that every file a\n" +
			"type locates parses and conforms to the type's schema, and that writing a file back\n" +
			"from what Quai reads changes no byte. Exits with status 1 when there are problems.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			info, err := os.Stat(dir)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", dir)
			}
			r := lint.Run(os.DirFS(dir))
			out := cmd.OutOrStdout()
			for _, n := range r.Notes {
				fmt.Fprintln(out, "note:", n)
			}
			for _, p := range r.Problems {
				fmt.Fprintln(out, p)
			}
			fmt.Fprintf(out, "%d types, %d blueprints, %d resources checked: %s\n",
				r.Types, r.Blueprints, r.Resources, plural(len(r.Problems), "problem"))
			if len(r.Problems) > 0 {
				return fmt.Errorf("%s in %s", plural(len(r.Problems), "problem"), dir)
			}
			return nil
		},
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if n == 0 {
		return "no " + word + "s"
	}
	return fmt.Sprintf("%d %ss", n, word)
}
