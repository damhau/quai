package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/damhau/quai/backend/internal/scim"
)

// newSCIMTokenCmd makes a SCIM token for the identity provider's provisioning
// (docs/operations/deployment-and-operations.md#provisioning-with-scim): the token, shown once,
// goes to the identity provider; its SHA-256 goes to the secret auth.scim.tokens references. Quai
// keeps only the hash, and makes no token itself: whoever runs this command holds it.
func newSCIMTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scim-token",
		Short: "Make a SCIM token for the identity provider's provisioning, and its SHA-256 for auth.scim.tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			token, sum, err := newSCIMToken(rand.Reader)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "token: %s\nsha256: %s\n", token, sum); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.ErrOrStderr(), "Give the token to the identity provider (Secret Token) and put the sha256 in the secret "+
				"that auth.scim.tokens references. The token is not shown again, and Quai does not keep it.")
			return err
		},
	}
}

// newSCIMToken returns a token, scim.TokenPrefix and 32 random bytes in hex, and its SHA-256 in hex.
func newSCIMToken(random io.Reader) (token, sum string, err error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(random, b); err != nil {
		return "", "", fmt.Errorf("scim-token: %w", err)
	}
	token = scim.TokenPrefix + hex.EncodeToString(b)
	h := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(h[:]), nil
}
