package definitions

import (
	"testing"
	"time"
)

func TestParseExpiry(t *testing.T) {
	for in, want := range map[string]time.Duration{"1m": time.Minute, "90m": 90 * time.Minute, "36h": 36 * time.Hour, "7d": 7 * 24 * time.Hour, "365d": MaxExpiry} {
		if got, err := ParseExpiry(in); err != nil || got != want {
			t.Errorf("ParseExpiry(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0d", "0m", "1w", "7", "d", "-1d", "1.5d", "366d", "8761h", "99999999999999999999d", " 7d", "7d\n"} {
		if got, err := ParseExpiry(in); err == nil {
			t.Errorf("ParseExpiry(%q) = %v, want an error", in, got)
		}
	}
}

func TestExpiry(t *testing.T) {
	if w, d := Expiry(Approval{}, Approval{Expiry: "7d"}, Approval{Expiry: "36h"}, Approval{Expiry: "bad"}); w != "36h" || d != 36*time.Hour {
		t.Errorf("Expiry = %q, %v; want the shortest, 36h", w, d)
	}
	if w, d := Expiry(Approval{}, Approval{}); w != "" || d != 0 {
		t.Errorf("Expiry without one = %q, %v", w, d)
	}
}
