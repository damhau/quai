package siem

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestASCIIJSON: the escaped JSON is ASCII and holds the same value.
func TestASCIIJSON(t *testing.T) {
	in, _ := json.Marshal(map[string]string{"s": "Zürich ☃ 🚀 \u2028 <&>", "plain": `a"b\c`})
	out := asciiJSON(in)
	for _, c := range out {
		if c > 126 {
			t.Fatalf("not ASCII: %s", out)
		}
	}
	var a, b map[string]string
	if err := json.Unmarshal(in, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &b); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if a["s"] != b["s"] || a["plain"] != b["plain"] {
		t.Errorf("%q, want %q", b, a)
	}
	// esc is a backslash followed by s.
	esc := func(s string) []byte { return append([]byte{92}, s...) }
	if !bytes.Contains(out, esc("u00fc")) || !bytes.Contains(out, append(esc("ud83d"), esc("ude80")...)) {
		t.Errorf("escapes: %s", out)
	}
}

// TestPrintable: header fields keep printable US-ASCII, within their length.
func TestPrintable(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"request.submitted", 32, "request.submitted"},
		{"a b\tc", 32, "abc"},
		{"zürich", 32, "zrich"},
		{"", 32, "-"},
		{" ", 32, "-"},
		{"abcdef", 4, "abcd"},
	} {
		if got := printable(c.in, c.max); got != c.want {
			t.Errorf("printable(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

// TestSeverity: what may need someone's attention, a break-glass among them, is a warning; its
// review's sign-offs and every other event a notice.
func TestSeverity(t *testing.T) {
	for action, want := range map[string]int{
		"audit.broken": severityWarning, "request.merged_without_approval": severityWarning, "request.merged_outside_window": severityWarning,
		"repository.enforcement_lost": severityWarning, "request.break_glass": severityWarning,
		"request.break_glass_signed_off": severityNotice, "request.break_glass_review_closed": severityNotice, "request.merged": severityNotice,
	} {
		if got := severity(action); got != want {
			t.Errorf("severity(%s) = %d, want %d", action, got, want)
		}
	}
}
