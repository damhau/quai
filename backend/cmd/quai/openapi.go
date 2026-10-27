package main

import (
	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/api"
)

func newOpenAPICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "openapi",
		Short: "Print the API's OpenAPI document",
		Long: "Prints the OpenAPI 3.1 document of every API operation, without a server. The frontend\n" +
			"commits it as frontend/openapi.json and generates its client from it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := api.Document("dev")
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(append(doc, '\n'))
			return err
		},
	}
}
