package definitions

import (
	"errors"
	"strconv"
	"time"
)

// MaxExpiry bounds an approval's expiry: a year.
const MaxExpiry = 365 * 24 * time.Hour

// ParseExpiry reads an approval's expiry: a whole number of minutes (30m), hours (12h) or days
// (7d), at least a minute and at most MaxExpiry
// (docs/design/05-delivery-and-approvals.md#reminders-escalation-expiry).
func ParseExpiry(s string) (time.Duration, error) {
	m := durationRe.FindStringSubmatch(s)
	if m == nil {
		return 0, errors.New("must be a duration such as 30m, 12h or 7d")
	}
	unit := map[string]time.Duration{"m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || n < 1 || n > int64(MaxExpiry/unit) {
		return 0, errors.New("must be at least 1m and at most 365d")
	}
	return time.Duration(n) * unit, nil
}

// Expiry returns the shortest expiry among approvals, as written, and its duration; "" and 0 when
// none sets one. Expiries were checked when the definitions were loaded: one that does not parse
// is skipped.
func Expiry(approvals ...Approval) (string, time.Duration) {
	written, shortest := "", time.Duration(0)
	for _, a := range approvals {
		if a.Expiry == "" {
			continue
		}
		d, err := ParseExpiry(a.Expiry)
		if err != nil {
			continue
		}
		if shortest == 0 || d < shortest {
			written, shortest = a.Expiry, d
		}
	}
	return written, shortest
}
