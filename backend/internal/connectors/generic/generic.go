// Package generic is the connector for a Git server Quai knows nothing else about
// (docs/design/12-git-connectors.md#bitbucket-gitea-generic): Git transport only, so direct
// delivery only and no enforcement. It authenticates with an SSH deploy key, the secret that
// spec.credentials.ref names, against the host keys of spec.credentials.knownHosts.
package generic

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/damhau/quai/backend/internal/connectors"
	"github.com/damhau/quai/backend/internal/gitx"
)

// Connector is the generic connector.
type Connector struct {
	platform string
	cfg      connectors.Config
}

// New returns the generic connector.
func New(cfg connectors.Config) (connectors.Connector, error) {
	return &Connector{platform: "generic", cfg: cfg}, nil
}

// Fallback returns a connector with Git transport only for a platform whose own connector is
// not built yet (Registry.Fallback); it reports the platform's name.
func Fallback(platform string, cfg connectors.Config) (connectors.Connector, error) {
	return &Connector{platform: platform, cfg: cfg}, nil
}

func (c *Connector) Info() connectors.Info { return connectors.Info{Platform: c.platform} }

// GitAuth returns the deploy key, for an SSH remote; nothing when the connection names no
// credentials (a public repository, or one whose URL carries them).
func (c *Connector) GitAuth(ctx context.Context) (gitx.Credentials, error) {
	key, ok, err := c.cfg.Credentials(ctx)
	if err != nil || !ok {
		return gitx.Credentials{}, err
	}
	if !SSH(c.cfg.URL) {
		return gitx.Credentials{}, fmt.Errorf("the %s connector authenticates with an SSH deploy key: use an SSH URL (ssh://… or user@host:path)", c.platform)
	}
	if strings.TrimSpace(c.cfg.KnownHosts) == "" {
		return gitx.Credentials{}, errors.New("an SSH deploy key needs the server's host keys: set spec.credentials.knownHosts")
	}
	return gitx.Credentials{SSHKey: []byte(key.Reveal()), KnownHosts: []byte(c.cfg.KnownHosts)}, nil
}

// SSH reports whether remote is an SSH URL: ssh://… or the scp-like user@host:path.
func SSH(remote string) bool {
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" {
		return u.Scheme == "ssh" || u.Scheme == "git+ssh"
	}
	at, colon := strings.Index(remote, "@"), strings.Index(remote, ":")
	return at > 0 && colon > at+1 && !strings.Contains(remote[:colon], "/")
}

func (c *Connector) PullRequests() connectors.PullRequests { return nil }
func (c *Connector) Statuses() connectors.Statuses         { return nil }
func (c *Connector) Policies() connectors.Policies         { return nil }
func (c *Connector) Owners() connectors.Owners             { return nil }
func (c *Connector) Webhooks() connectors.Webhooks         { return nil }
func (c *Connector) Pipelines() connectors.Pipelines       { return nil }
func (c *Connector) Identities() connectors.Identities     { return nil }
