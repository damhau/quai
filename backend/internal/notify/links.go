package notify

import (
	"context"
	"fmt"
	"strings"

	"github.com/damhau/quai/backend/internal/chatlink"
	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/secrets"
)

// Links returns the signer of the links of approval cards in chat, with the keys of
// notifications.links resolved at each use (a rotated secret is used without a restart); nil
// when it is not configured.
func Links(cfg *config.ChatLinks, sec *secrets.Store) *chatlink.Signer {
	if cfg == nil {
		return nil
	}
	key := func(ctx context.Context, ref, name string) ([]byte, error) {
		s, err := sec.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err) // names the reference, never the value
		}
		return []byte(strings.TrimSpace(s.Reveal())), nil
	}
	return chatlink.New(func(ctx context.Context) ([]byte, [][]byte, error) {
		current, err := key(ctx, cfg.Key.Ref, "notifications.links.key")
		if err != nil {
			return nil, nil, err
		}
		var previous [][]byte
		for i, p := range cfg.PreviousKeys {
			k, err := key(ctx, p.Ref, fmt.Sprintf("notifications.links.previousKeys[%d]", i))
			if err != nil {
				return nil, nil, err
			}
			previous = append(previous, k)
		}
		return current, previous, nil
	})
}
