package main

import (
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// version is set at build time: go build -ldflags "-X main.version=v1.2.3".
var version = "dev"

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of quai",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info, _ := debug.ReadBuildInfo()
			_, err := fmt.Fprintln(cmd.OutOrStdout(), formatVersion(version, info))
			return err
		},
	}
}

// formatVersion renders "quai <version> (commit <rev>[, modified], <go version>)" from the build
// information, leaving out the parts that are not known.
func formatVersion(version string, info *debug.BuildInfo) string {
	if info == nil {
		return "quai " + version
	}
	var details []string
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		details = append(details, "commit "+revision)
		if modified == "true" {
			details = append(details, "modified")
		}
	}
	if info.GoVersion != "" {
		details = append(details, info.GoVersion)
	}
	if len(details) == 0 {
		return "quai " + version
	}
	return fmt.Sprintf("quai %s (%s)", version, strings.Join(details, ", "))
}
