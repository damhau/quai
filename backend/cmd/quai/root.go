package main

import "github.com/spf13/cobra"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "quai",
		Short:         "Governed self-service in front of infrastructure as code",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersionCmd(), newLintCmd(), newServerCmd(), newMigrateCmd(), newWorkerCmd(), newOpenAPICmd())
	return root
}
