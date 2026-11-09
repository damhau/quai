// Package window decides change windows and freezes
// (docs/design/05-delivery-and-approvals.md#change-windows-and-freezes): when an approved request
// may merge. A window applies to a request when its `when` holds (always without one); it then
// lets the request merge only in one of its allow entries (any time without one) and never in
// one of its deny entries (freezes). Every window that applies must let the request merge: Quai's
// configuration and the repository document each add constraints, so neither can lift the
// other's. At decides whether a time is in the windows, and when they next open or close, within
// Horizon.
//
// Times are wall-clock times in a named time zone: an allow entry holds whenever the zone's
// clock reads a time inside it, on one of its days. On the day the clock springs forward, an
// entry inside the skipped hour never holds; on the day it falls back, an entry in the repeated
// hour holds during both readings. A deny entry runs from the first instant the clock reads its
// start to the last instant it reads its end (a time in a skipped hour reads as the jump), so an
// ambiguous freeze is the longer one.
//
// The package is pure: the caller resolves time zones (Zones) and passes the time.
package window

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/damhau/quai/backend/internal/engine/expr"
)

// Horizon is how far ahead At looks for the windows to open or close: a request whose windows
// do not open within it waits, and none is said to open.
const Horizon = 366 * day

// Limits on what a source may declare, which bound the work of At.
const (
	MaxWindows = 32 // windows in one source (Quai's configuration, a repository document)
	MaxEntries = 32 // allow entries, and deny entries, in one window
)

const day = 24 * time.Hour

// ChangeWindow is a change window as written in Quai's configuration (changeWindows) or a
// repository document (spec.enforcement.changeWindows).
type ChangeWindow struct {
	Name string `json:"name" yaml:"name"`
	// When is a CEL condition on the request (params, request): the window applies only to the
	// requests for which it holds. Empty: to every request.
	When string `json:"when,omitempty" yaml:"when"`
	// Allow are the times the window lets requests merge: "Mon-Thu 08:00-16:00 Europe/Zurich".
	// None: any time but Deny.
	Allow []string `json:"allow,omitempty" yaml:"allow"`
	// Deny are freezes: "2026-12-18T00:00 to 2027-01-05T00:00 Europe/Zurich".
	Deny []string `json:"deny,omitempty" yaml:"deny"`
}

// Window is a compiled ChangeWindow.
type Window struct {
	Name  string
	When  *expr.Program // nil: the window applies to every request
	Allow []Allow
	Deny  []Deny
}

// Allow is an allow entry: on Days, from Start to End minutes after midnight in Zone.
type Allow struct {
	Text       string
	Days       [7]bool // by time.Weekday
	Start, End int     // 0 <= Start < End <= 24*60
	Zone       *time.Location
}

// Deny is a freeze: the instants from From (included) to To (excluded).
type Deny struct {
	Text     string
	From, To time.Time
	Zone     *time.Location
}

// Zones resolves the name of an IANA time zone (config.TimeZone): the caller's database.
type Zones func(name string) (*time.Location, error)

// The variables a window's when may not use: a window is about what changes and where, not who
// asks or what the files say.
var forbidden = []string{expr.VarInputs, expr.VarDoc, expr.VarPrevious, expr.VarAction, expr.VarUser}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// Compile checks and compiles specs, declared at the field at (changeWindows); the error lists
// every problem, each with its place ("changeWindows[1].allow[0]: ...").
func Compile(specs []ChangeWindow, at string, zones Zones) ([]*Window, error) {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(at+format, args...)) }
	if len(specs) > MaxWindows {
		add(": at most %d windows", MaxWindows)
		return nil, errors.Join(errs...)
	}
	env, err := expr.NewEnv()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]*Window, 0, len(specs))
	for i, s := range specs {
		w := &Window{Name: s.Name}
		switch {
		case !nameRe.MatchString(s.Name):
			add("[%d].name must be 1 to 63 letters, digits, '.', '_' or '-'", i)
		case seen[s.Name]:
			add("[%d].name: %s is declared twice", i, s.Name)
		}
		seen[s.Name] = true
		if len(s.Allow) == 0 && len(s.Deny) == 0 {
			add("[%d] (%s) needs allow or deny entries", i, s.Name)
		}
		if len(s.Allow) > MaxEntries || len(s.Deny) > MaxEntries {
			add("[%d] (%s): at most %d allow and %d deny entries", i, s.Name, MaxEntries, MaxEntries)
			continue
		}
		if s.When != "" {
			if w.When, err = env.CompileCondition(s.When); err != nil {
				add("[%d].when: %v", i, err)
			} else {
				for _, v := range forbidden {
					if w.When.Uses(v) {
						add("[%d].when: a change window's condition sees params and request only, not %s", i, v)
					}
				}
			}
		}
		for j, text := range s.Allow {
			a, err := ParseAllow(text, zones)
			if err != nil {
				add("[%d].allow[%d]: %v", i, j, err)
				continue
			}
			w.Allow = append(w.Allow, a)
		}
		for j, text := range s.Deny {
			d, err := ParseDeny(text, zones)
			if err != nil {
				add("[%d].deny[%d]: %v", i, j, err)
				continue
			}
			w.Deny = append(w.Deny, d)
		}
		out = append(out, w)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

var weekdays = map[string]time.Weekday{
	"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday,
	"Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday,
}

// ParseAllow parses an allow entry: days (Mon, Mon-Fri, Mon,Wed,Fri-Sun; a range may wrap,
// Fri-Mon), hours (HH:MM-HH:MM, the end after the start, 24:00 for midnight at the end) and a
// time zone, separated by spaces: "Mon-Thu 08:00-16:00 Europe/Zurich". An entry does not cross
// midnight: write two.
func ParseAllow(text string, zones Zones) (Allow, error) {
	a := Allow{Text: text}
	f := strings.Fields(text)
	if len(f) != 3 {
		return a, fmt.Errorf("%q: write days, hours and a time zone, such as \"Mon-Fri 08:00-16:00 Europe/Zurich\"", text)
	}
	for item := range strings.SplitSeq(f[0], ",") {
		from, to, isRange := strings.Cut(item, "-")
		d1, ok1 := weekdays[from]
		d2, ok2 := weekdays[to]
		if !ok1 || (isRange && !ok2) {
			return a, fmt.Errorf("%q: days are Mon, Tue, Wed, Thu, Fri, Sat and Sun, as a list (Mon,Wed) or ranges (Mon-Fri)", text)
		}
		if !isRange {
			a.Days[d1] = true
			continue
		}
		if d1 == d2 {
			return a, fmt.Errorf("%q: the range %s runs from a day to itself; write the day alone, or Mon-Sun for every day", text, item)
		}
		for d := d1; ; d = (d + 1) % 7 {
			a.Days[d] = true
			if d == d2 {
				break
			}
		}
	}
	from, to, ok := strings.Cut(f[1], "-")
	start, err1 := minutes(from, false)
	end, err2 := minutes(to, true)
	if !ok || err1 != nil || err2 != nil {
		return a, fmt.Errorf("%q: hours are HH:MM-HH:MM, such as 08:00-16:00 (24:00 ends at midnight)", text)
	}
	if start >= end {
		return a, fmt.Errorf("%q: the hours end before they start; an entry does not cross midnight, write two", text)
	}
	a.Start, a.End = start, end
	loc, err := zones(f[2])
	if err != nil {
		return a, fmt.Errorf("%q: %w", text, err)
	}
	a.Zone = loc
	return a, nil
}

// minutes reads HH:MM as minutes after midnight; 24:00 only as an end.
func minutes(s string, end bool) (int, error) {
	if end && s == "24:00" {
		return 24 * 60, nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, errors.New("not HH:MM")
	}
	return t.Hour()*60 + t.Minute(), nil
}

// ParseDeny parses a freeze: a start, "to", an end (each YYYY-MM-DDTHH:MM, or a date for its
// midnight) and a time zone: "2026-12-18T00:00 to 2027-01-05T00:00 Europe/Zurich".
func ParseDeny(text string, zones Zones) (Deny, error) {
	d := Deny{Text: text}
	f := strings.Fields(text)
	if len(f) != 4 || f[1] != "to" {
		return d, fmt.Errorf("%q: write a start, to, an end and a time zone, such as \"2026-12-18T00:00 to 2027-01-05T00:00 Europe/Zurich\"", text)
	}
	from, err1 := wallTime(f[0])
	to, err2 := wallTime(f[2])
	if err1 != nil || err2 != nil {
		return d, fmt.Errorf("%q: times are YYYY-MM-DDTHH:MM, or a date for its midnight", text)
	}
	if !from.Before(to) {
		return d, fmt.Errorf("%q: the freeze ends before it starts", text)
	}
	loc, err := zones(f[3])
	if err != nil {
		return d, fmt.Errorf("%q: %w", text, err)
	}
	d.Zone = loc
	// The hull of the instants the clock reads a time from the start to the end: an ambiguous
	// start is its first reading, an ambiguous end its last.
	if spans := wall(from, to, loc); len(spans) > 0 {
		d.From, d.To = spans[0].from, spans[len(spans)-1].to
	}
	return d, nil
}

// wallTime reads a wall-clock time, as UTC fields.
func wallTime(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02T15:04", s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

// Applies reports whether w applies to a request: its when holds for one of subjects (the
// request's resources); without when, or without subjects to test, it applies. An evaluation
// error is returned, and the caller holds the request: an error never lets it merge.
func (w *Window) Applies(subjects []expr.Vars) (bool, error) {
	if w.When == nil || len(subjects) == 0 {
		return true, nil
	}
	applies := false
	for _, v := range subjects {
		ok, err := w.When.Test(v)
		if err != nil {
			return false, fmt.Errorf("change window %s: %w", w.Name, err)
		}
		applies = applies || ok
	}
	return applies, nil
}

// Open reports whether w lets a request merge at t.
func (w *Window) Open(t time.Time) bool {
	for _, d := range w.Deny {
		if !t.Before(d.From) && t.Before(d.To) {
			return false
		}
	}
	if len(w.Allow) == 0 {
		return true
	}
	for _, a := range w.Allow {
		if a.covers(t) {
			return true
		}
	}
	return false
}

// Zone is the time zone of w's first entry: the one its times are shown in.
func (w *Window) Zone() *time.Location {
	if len(w.Allow) > 0 {
		return w.Allow[0].Zone
	}
	if len(w.Deny) > 0 {
		return w.Deny[0].Zone
	}
	return time.UTC
}

func (a Allow) covers(t time.Time) bool {
	l := t.In(a.Zone)
	m := l.Hour()*60 + l.Minute()
	return a.Days[l.Weekday()] && m >= a.Start && m < a.End
}

// State is what the windows that apply to a request say at a time.
type State struct {
	// Open: the request may merge now.
	Open bool
	// Closes, when open, is when the windows next close; zero if they stay open beyond Horizon
	// (or no window applies).
	Closes time.Time
	// Opens, when closed, is when the windows next open; zero if none opens within Horizon.
	Opens time.Time
	// Holding, when closed, are the windows closed now; when open, those that close at Closes.
	Holding []*Window
}

// Zone is the time zone a person is shown the state's times in: the first holding window's.
func (s State) Zone() *time.Location {
	if len(s.Holding) == 0 {
		return time.UTC
	}
	return s.Holding[0].Zone()
}

// At decides the windows ws, which all apply, at t. No window: always open.
func At(ws []*Window, t time.Time) State {
	var st State
	if len(ws) == 0 {
		return State{Open: true}
	}
	// Short stretches first: the answer is usually within days.
	for _, ahead := range []time.Duration{8 * day, 32 * day, Horizon} {
		hi := t.Add(ahead)
		open := spans(ws, t, hi)
		if len(open) > 0 && open[0].from.Equal(t) {
			st.Open = true
			if open[0].to.Before(hi) {
				st.Closes = open[0].to
				break
			}
			continue
		}
		if len(open) > 0 {
			st.Opens = open[0].from
			break
		}
	}
	at := t
	if st.Open {
		at = st.Closes
	}
	if !at.IsZero() {
		for _, w := range ws {
			if !w.Open(at) {
				st.Holding = append(st.Holding, w)
			}
		}
	}
	return st
}

// When formats t for a person, in zone: "Mon 08:00 Europe/Zurich" when it is less than six days
// after now, "Tue 5 Jan 2027 08:00 Europe/Zurich" otherwise.
func When(t, now time.Time, zone *time.Location) string {
	l := t.In(zone)
	if t.Sub(now) < 6*day {
		return l.Format("Mon 15:04") + " " + zone.String()
	}
	return l.Format("Mon 2 Jan 2006 15:04") + " " + zone.String()
}

// span is the instants from from (included) to to (excluded).
type span struct{ from, to time.Time }

// spans returns the instants of [lo, hi) at which every window of ws is open.
func spans(ws []*Window, lo, hi time.Time) []span {
	out := []span{{lo, hi}}
	for _, w := range ws {
		if out = intersect(out, w.spans(lo, hi)); len(out) == 0 {
			break
		}
	}
	return out
}

// spans returns the instants of [lo, hi) at which w is open.
func (w *Window) spans(lo, hi time.Time) []span {
	allowed := []span{{lo, hi}}
	if len(w.Allow) > 0 {
		allowed = nil
		for _, a := range w.Allow {
			allowed = append(allowed, a.spans(lo, hi)...)
		}
		allowed = normalize(allowed)
	}
	var denied []span
	for _, d := range w.Deny {
		if s := clip(span{d.From, d.To}, lo, hi); s.from.Before(s.to) {
			denied = append(denied, s)
		}
	}
	return subtract(allowed, normalize(denied))
}

// spans returns the instants of [lo, hi) at which a holds.
func (a Allow) spans(lo, hi time.Time) []span {
	first, last := lo.In(a.Zone), hi.In(a.Zone)
	// A day either side: around a jump back at midnight, a day's readings come after the next's.
	d := time.Date(first.Year(), first.Month(), first.Day()-1, 0, 0, 0, 0, time.UTC)
	end := time.Date(last.Year(), last.Month(), last.Day()+1, 0, 0, 0, 0, time.UTC)
	var out []span
	for ; !d.After(end); d = d.AddDate(0, 0, 1) {
		if !a.Days[d.Weekday()] {
			continue
		}
		for _, s := range wall(d.Add(time.Duration(a.Start)*time.Minute), d.Add(time.Duration(a.End)*time.Minute), a.Zone) {
			if s = clip(s, lo, hi); s.from.Before(s.to) {
				out = append(out, s)
			}
		}
	}
	return out
}

// wall returns the instants at which loc's clock reads a time from from (included) to to
// (excluded), wall-clock times given as UTC fields: in each period of constant offset, the
// readings from-offset to to-offset that fall in it.
func wall(from, to time.Time, loc *time.Location) []span {
	var out []span
	// Offsets are within a day of UTC: the periods from a day before to a day after hold every
	// reading.
	end := to.Add(27 * time.Hour)
	for t := from.Add(-27 * time.Hour); t.Before(end); {
		l := t.In(loc)
		start, next := l.ZoneBounds()
		_, offset := l.Zone()
		off := time.Duration(offset) * time.Second
		s := span{from.Add(-off), to.Add(-off)}
		if !start.IsZero() && s.from.Before(start) {
			s.from = start
		}
		if !next.IsZero() && s.to.After(next) {
			s.to = next
		}
		if s.from.Before(s.to) {
			out = append(out, s)
		}
		if next.IsZero() || !next.After(t) {
			break
		}
		t = next
	}
	return normalize(out)
}

func clip(s span, lo, hi time.Time) span {
	if s.from.Before(lo) {
		s.from = lo
	}
	if s.to.After(hi) {
		s.to = hi
	}
	return s
}

// normalize sorts spans and merges those that overlap or touch; it drops empty ones.
func normalize(in []span) []span {
	in = slices.DeleteFunc(in, func(s span) bool { return !s.from.Before(s.to) })
	slices.SortFunc(in, func(a, b span) int { return a.from.Compare(b.from) })
	var out []span
	for _, s := range in {
		if n := len(out); n > 0 && !s.from.After(out[n-1].to) {
			if s.to.After(out[n-1].to) {
				out[n-1].to = s.to
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// intersect returns the instants in both a and b, each normalized.
func intersect(a, b []span) []span {
	var out []span
	for i, j := 0, 0; i < len(a) && j < len(b); {
		from, to := a[i].from, a[i].to
		if b[j].from.After(from) {
			from = b[j].from
		}
		if b[j].to.Before(to) {
			to = b[j].to
		}
		if from.Before(to) {
			out = append(out, span{from, to})
		}
		if a[i].to.Before(b[j].to) {
			i++
		} else {
			j++
		}
	}
	return out
}

// subtract returns the instants of a not in b, each normalized.
func subtract(a, b []span) []span {
	var out []span
	j := 0
	for _, s := range a {
		for j < len(b) && !b[j].to.After(s.from) {
			j++
		}
		cur := s.from
		for k := j; k < len(b) && b[k].from.Before(s.to); k++ {
			if b[k].from.After(cur) {
				out = append(out, span{cur, b[k].from})
			}
			if b[k].to.After(cur) {
				cur = b[k].to
			}
		}
		if cur.Before(s.to) {
			out = append(out, span{cur, s.to})
		}
	}
	return out
}
