package generic

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/connectors/conformance"
	"github.com/damhau/quai/backend/internal/secrets"
)

func TestGitAuth(t *testing.T) {
	ctx := context.Background()
	key := func(context.Context) (secrets.Secret, bool, error) {
		return secrets.FromValue("PRIVATE KEY"), true, nil
	}
	const hosts = "git.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	for name, tc := range map[string]struct {
		cfg  connectors.Config
		want string // an error containing it; empty: the key
	}{
		"ssh URL":      {connectors.Config{URL: "ssh://git@git.example.com/infra.git", Credentials: key, KnownHosts: hosts}, ""},
		"scp-like URL": {connectors.Config{URL: "git@git.example.com:infra.git", Credentials: key, KnownHosts: hosts}, ""},
		"HTTPS URL":    {connectors.Config{URL: "https://git.example.com/infra.git", Credentials: key, KnownHosts: hosts}, "SSH URL"},
		"no host keys": {connectors.Config{URL: "ssh://git@git.example.com/infra.git", Credentials: key}, "knownHosts"},
		"unresolvable": {connectors.Config{URL: "ssh://git@git.example.com/infra.git", KnownHosts: hosts, Credentials: func(context.Context) (secrets.Secret, bool, error) {
			return secrets.Secret{}, false, secrets.ErrNotFound
		}}, "not found"},
	} {
		r := &connectors.Registry{Factories: map[string]connectors.Factory{"generic": New}}
		c, err := r.New("generic", tc.cfg)
		if err != nil {
			t.Fatal(err)
		}
		creds, err := c.GitAuth(ctx)
		switch {
		case tc.want == "" && (err != nil || string(creds.SSHKey) != "PRIVATE KEY" || string(creds.KnownHosts) != hosts):
			t.Errorf("%s: %+v, %v", name, creds, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want an error about %s", name, err, tc.want)
		}
	}
	for remote, want := range map[string]bool{"ssh://h/r": true, "git+ssh://u@h/r": true, "u@h:r.git": true, "https://u@h/r": false, "/srv/r.git": false, "./a@b:c": false} {
		if SSH(remote) != want {
			t.Errorf("SSH(%q) = %v", remote, !want)
		}
	}
}

// local is a harness over a bare repository on disk: Git transport only, as generic Git.
type local struct{}

func (local) Setup(t *testing.T, client *http.Client) *conformance.Repository {
	dir := t.TempDir()
	bare, work := filepath.Join(dir, "infra.git"), filepath.Join(dir, "work")
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", bare},
		{"clone", "--quiet", bare, work},
		{"-C", work, "commit", "--quiet", "--allow-empty", "-m", "initial"},
		{"-C", work, "push", "--quiet", "origin", "HEAD:main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=" + dir, "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	c, err := New(connectors.Config{URL: bare, HTTP: client, Credentials: func(context.Context) (secrets.Secret, bool, error) { return secrets.Secret{}, false, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return &conformance.Repository{Connector: c, URL: bare, DefaultBranch: "main"}
}

// TestConformance: generic Git pushes Quai's branches; every other step is skipped for want of
// the capability.
func TestConformance(t *testing.T) { conformance.Run(t, local{}) }
