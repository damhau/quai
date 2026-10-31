package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/engine/lint"
)

func newLintCmd() *cobra.Command {
	var planFile, stack string
	cmd := &cobra.Command{
		Use:   "lint [directory]",
		Short: "Check a repository's Quai definitions and the files they describe",
		Long: "Checks that every definition under .quai/types and blueprints loads, that every file a\n" +
			"type locates parses and conforms to the type's schema, and that writing a file back\n" +
			"from what Quai reads changes no byte. With --plan and --stack, also checks that every\n" +
			"Terraform resource the plan changes belongs to exactly one resource. Exits with status 1\n" +
			"when there are problems.",
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
			if (planFile == "") != (stack == "") {
				return fmt.Errorf("--plan and --stack go together: the plan file and the stack it plans")
			}
			r := lint.Run(os.DirFS(dir))
			if planFile != "" {
				b, err := os.ReadFile(planFile) // #nosec G304 -- the file the user names
				if err != nil {
					return err
				}
				r.Problems = append(r.Problems, lint.Plan(os.DirFS(dir), b, planFile, stack)...)
			}
			out := cmd.OutOrStdout()
			for _, n := range r.Notes {
				fmt.Fprintln(out, "note:", n)
			}
			for _, p := range r.Problems {
				fmt.Fprintln(out, p)
			}
			fmt.Fprintf(out, "%d types, %d blueprints, %d resources, %d golden examples checked: %s\n",
				r.Types, r.Blueprints, r.Resources, r.Tests, plural(len(r.Problems), "problem"))
			if len(r.Problems) > 0 {
				return fmt.Errorf("%s in %s", plural(len(r.Problems), "problem"), dir)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&planFile, "plan", "", "a plan of the repository, from terraform show -json")
	cmd.Flags().StringVar(&stack, "stack", "", "the stack the plan is for, such as envs/prod")
	return cmd
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
