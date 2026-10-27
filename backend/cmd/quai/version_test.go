package main

import (
	"bytes"
	"runtime/debug"
	"strings"
	"testing"
)

func TestFormatVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		info    *debug.BuildInfo
		want    string
	}{
		{
			name:    "no build information",
			version: "dev",
			want:    "quai dev",
		},
		{
			name:    "go version only",
			version: "v1.2.3",
			info:    &debug.BuildInfo{GoVersion: "go1.27.3"},
			want:    "quai v1.2.3 (go1.27.3)",
		},
		{
			name:    "clean checkout",
			version: "v1.2.3",
			info: &debug.BuildInfo{GoVersion: "go1.27.3", Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "faa392712345678901234567890123456789abcd"},
				{Key: "vcs.modified", Value: "false"},
			}},
			want: "quai v1.2.3 (commit faa392712345, go1.27.3)",
		},
		{
			name:    "modified checkout",
			version: "dev",
			info: &debug.BuildInfo{GoVersion: "go1.27.3", Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: "faa3927"},
				{Key: "vcs.modified", Value: "true"},
			}},
			want: "quai dev (commit faa3927, modified, go1.27.3)",
		},
		{
			name:    "empty build information",
			version: "dev",
			info:    &debug.BuildInfo{},
			want:    "quai dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatVersion(tt.version, tt.info); got != tt.want {
				t.Errorf("formatVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestVersionCommand(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := out.String(); !strings.HasPrefix(got, "quai dev") || !strings.HasSuffix(got, "\n") {
		t.Errorf("version output = %q, want a line starting with %q", got, "quai dev")
	}
}

func TestVersionCommandRejectsArguments(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"version", "extra"})
	if err := root.Execute(); err == nil {
		t.Fatal("Execute() error = nil, want an error for an unexpected argument")
	}
}

func TestUnknownCommand(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{"does-not-exist"})
	if err := root.Execute(); err == nil {
		t.Fatal("Execute() error = nil, want an error for an unknown command")
	}
}

func TestOpenAPICommand(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"openapi"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"openapi": "3.1`, `"/api/v1/requests/type"`, `"/api/v1/catalog"`, `"/api/v1/me"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the document lacks %s", want)
		}
	}
}
