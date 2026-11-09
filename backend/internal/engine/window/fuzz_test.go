package window

import (
	"testing"
	"time"
)

// FuzzParse: the entry parsers never panic, and what they accept is sound: an allow entry has a
// day and hours that end after they start, a freeze ends after it starts (or is empty, inside
// a skipped hour), and At decides them.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"Mon-Thu 08:00-16:00 Europe/Zurich", "Fri-Mon 22:00-24:00 UTC", "Mon,Wed,Sat-Sun 00:00-00:01 UTC",
		"2026-12-18T00:00 to 2027-01-05T00:00 Europe/Zurich", "2026-03-29T02:30 to 2026-03-29T02:45 Europe/Zurich",
		"2026-10-25 to 2026-10-26 America/New_York", "Mon-Mon 08:00-08:00 X", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
		if a, err := ParseAllow(text, zones); err == nil {
			if a.Start < 0 || a.Start >= a.End || a.End > 24*60 || a.Days == [7]bool{} || a.Zone == nil {
				t.Fatalf("%q: %+v", text, a)
			}
			At([]*Window{{Name: "w", Allow: []Allow{a}}}, at)
		}
		if d, err := ParseDeny(text, zones); err == nil {
			if d.To.Before(d.From) || d.Zone == nil {
				t.Fatalf("%q: %+v", text, d)
			}
			At([]*Window{{Name: "w", Deny: []Deny{d}}}, at)
		}
	})
}
