package connectors

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMarker(t *testing.T) {
	desc := "Resize web01 for Black Friday.\n\n" + Marker("3b6f2a8c-0d4e-4f5a-9b1c-2d3e4f5a6b7c")
	if id, ok := RequestOf(desc); !ok || id != "3b6f2a8c-0d4e-4f5a-9b1c-2d3e4f5a6b7c" {
		t.Errorf("RequestOf = %q, %v", id, ok)
	}
	if _, ok := RequestOf("no marker <!-- quai:other=1 -->"); ok {
		t.Error("a description without a marker")
	}
	long := strings.Repeat("é", 3000) + "\n\n" + Marker("abc")
	got := Truncate(long, 4000)
	if len(got) > 4000 || !utf8.ValidString(got) || !strings.HasSuffix(got, Marker("abc")) || !strings.Contains(got, "…") {
		t.Errorf("Truncate: %d bytes, valid %v, ends %q", len(got), utf8.ValidString(got), got[len(got)-40:])
	}
	if Truncate("short", 4000) != "short" {
		t.Error("a short description was changed")
	}
}
