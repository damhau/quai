package connectors

import (
	"regexp"
	"strings"
)

// Marker is the machine-readable line a pull request's description carries, so that webhook
// events map back to the request (docs/design/05-delivery-and-approvals.md#branch-commit-pull-request).
func Marker(request string) string { return "<!-- quai:request=" + request + " -->" }

var markerRe = regexp.MustCompile(`<!-- quai:request=([0-9A-Za-z-]+) -->`)

// RequestOf returns the request a pull request's description names in its marker.
func RequestOf(description string) (string, bool) {
	m := markerRe.FindStringSubmatch(description)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// Truncate shortens a description to max bytes for a platform that limits it, keeping its
// marker: the text is cut, never the marker.
func Truncate(description string, max int) string {
	if len(description) <= max {
		return description
	}
	marker := markerRe.FindString(description)
	const cut = "\n\n…\n\n"
	text := strings.Replace(description, marker, "", 1)
	keep := max - len(marker) - len(cut)
	if keep < 0 {
		return marker[:min(len(marker), max)]
	}
	// Cut on a rune boundary.
	for keep > 0 && keep < len(text) && !utf8RuneStart(text[keep]) {
		keep--
	}
	return strings.TrimRight(text[:keep], " \n") + cut + marker
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
