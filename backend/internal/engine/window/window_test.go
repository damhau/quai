package window

import (
	"math/rand/v2"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zones of the tests, whatever the machine has

	"github.com/damhau/quai/backend/internal/engine/expr"
)

func zones(name string) (*time.Location, error) { return time.LoadLocation(name) }

func compile(t *testing.T, specs ...ChangeWindow) []*Window {
	t.Helper()
	ws, err := Compile(specs, "changeWindows", zones)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func utc(s string) time.Time {
	t, err := time.Parse("2006-01-02T15:04Z", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseAllow(t *testing.T) {
	t.Parallel()
	all := [7]bool{true, true, true, true, true, true, true}
	for text, want := range map[string]Allow{
		"Mon-Thu 08:00-16:00 Europe/Zurich": {Days: [7]bool{1: true, 2: true, 3: true, 4: true}, Start: 8 * 60, End: 16 * 60},
		"Fri-Mon 22:00-24:00 UTC":           {Days: [7]bool{0: true, 1: true, 5: true, 6: true}, Start: 22 * 60, End: 24 * 60},
		"Mon,Wed,Sat-Sun 00:00-00:01 UTC":   {Days: [7]bool{0: true, 1: true, 3: true, 6: true}, Start: 0, End: 1},
		"Mon-Sun 00:00-24:00 UTC":           {Days: all, Start: 0, End: 24 * 60},
		"Sun-Sat 09:30-10:45 UTC":           {Days: all, Start: 9*60 + 30, End: 10*60 + 45},
	} {
		got, err := ParseAllow(text, zones)
		if err != nil {
			t.Errorf("%s: %v", text, err)
			continue
		}
		if got.Days != want.Days || got.Start != want.Start || got.End != want.End || got.Zone == nil || got.Text != text {
			t.Errorf("%s = %+v, want %+v", text, got, want)
		}
	}
	for text, want := range map[string]string{
		"Mon-Fri 08:00-16:00":                 "days, hours and a time zone",
		"Mon-Fri  08:00-16:00 UTC extra":      "days, hours and a time zone",
		"Monday 08:00-16:00 UTC":              "days are Mon",
		"mon 08:00-16:00 UTC":                 "days are Mon",
		"Mon- 08:00-16:00 UTC":                "days are Mon",
		"Mon,,Tue 08:00-16:00 UTC":            "days are Mon",
		"Mon-Mon 08:00-16:00 UTC":             "from a day to itself",
		"Mon 8:00-16:00 UTC":                  "hours are HH:MM-HH:MM",
		"Mon 08:00-16:60 UTC":                 "hours are HH:MM-HH:MM",
		"Mon 24:00-24:00 UTC":                 "hours are HH:MM-HH:MM",
		"Mon 08:00 UTC":                       "hours are HH:MM-HH:MM",
		"Mon 16:00-08:00 UTC":                 "does not cross midnight",
		"Mon 08:00-08:00 UTC":                 "does not cross midnight",
		"Mon 08:00-16:00 Europe/Atlantis":     "Europe/Atlantis",
		"Mon 08:00-16:00 ../../etc/localtime": "localtime",
	} {
		if _, err := ParseAllow(text, zones); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want one saying %q", text, err, want)
		}
	}
}

func TestParseDeny(t *testing.T) {
	t.Parallel()
	for text, want := range map[string][2]time.Time{
		// Winter (+01:00) to winter.
		"2026-12-18T00:00 to 2027-01-05T00:00 Europe/Zurich": {utc("2026-12-17T23:00Z"), utc("2027-01-04T23:00Z")},
		// A date is its midnight; winter to summer.
		"2026-03-01 to 2026-04-01 Europe/Zurich": {utc("2026-02-28T23:00Z"), utc("2026-03-31T22:00Z")},
		// A start in the hour the clock skips (02:00-03:00 on 29 March 2026) is the jump.
		"2026-03-29T02:30 to 2026-03-29T05:00 Europe/Zurich": {utc("2026-03-29T01:00Z"), utc("2026-03-29T03:00Z")},
		// In the hour read twice (02:00-03:00 on 25 October 2026): from the first reading of the
		// start to the last of the end.
		"2026-10-25T02:30 to 2026-10-25T02:45 Europe/Zurich": {utc("2026-10-25T00:30Z"), utc("2026-10-25T01:45Z")},
		"2026-10-25T02:30 to 2026-10-25T04:00 Europe/Zurich": {utc("2026-10-25T00:30Z"), utc("2026-10-25T03:00Z")},
		"2026-10-24T12:00 to 2026-10-25T02:30 Europe/Zurich": {utc("2026-10-24T10:00Z"), utc("2026-10-25T01:30Z")},
	} {
		d, err := ParseDeny(text, zones)
		if err != nil {
			t.Errorf("%s: %v", text, err)
			continue
		}
		if !d.From.Equal(want[0]) || !d.To.Equal(want[1]) {
			t.Errorf("%s = %s to %s, want %s to %s", text, d.From.UTC(), d.To.UTC(), want[0], want[1])
		}
	}
	for text, want := range map[string]string{
		"2026-12-18T00:00 2027-01-05T00:00 Europe/Zurich":    "a start, to, an end",
		"2026-12-18T00:00 to 2027-01-05T00:00":               "a start, to, an end",
		"2026-12-18 00:00 to 2027-01-05T00:00 Europe/Zurich": "a start, to, an end",
		"18.12.2026 to 2027-01-05 Europe/Zurich":             "YYYY-MM-DDTHH:MM",
		"2026-12-18T00:00:00 to 2027-01-05 Europe/Zurich":    "YYYY-MM-DDTHH:MM",
		"2027-01-05 to 2026-12-18 Europe/Zurich":             "ends before it starts",
		"2027-01-05 to 2027-01-05 Europe/Zurich":             "ends before it starts",
		"2026-12-18 to 2027-01-05 Local":                     "Local",
	} {
		zones := func(name string) (*time.Location, error) {
			if name == "Local" {
				return nil, errorString("\"Local\" is not an IANA time zone name")
			}
			return time.LoadLocation(name)
		}
		if _, err := ParseDeny(text, zones); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want one saying %q", text, err, want)
		}
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestCompile(t *testing.T) {
	t.Parallel()
	ws := compile(t,
		ChangeWindow{Name: "prod-business-hours", When: "has(params.env) && params.env == 'prod'", Allow: []string{"Mon-Thu 08:00-16:00 Europe/Zurich"}},
		ChangeWindow{Name: "year-end-freeze", Deny: []string{"2026-12-18T00:00 to 2027-01-05T00:00 Europe/Zurich"}},
	)
	if len(ws) != 2 || ws[0].When == nil || ws[1].When != nil || len(ws[0].Allow) != 1 || len(ws[1].Deny) != 1 {
		t.Fatalf("compiled %+v", ws)
	}
	many := make([]string, MaxEntries+1)
	for i := range many {
		many[i] = "Mon 08:00-09:00 UTC"
	}
	for name, tt := range map[string]struct {
		specs []ChangeWindow
		want  []string
	}{
		"no name":       {[]ChangeWindow{{Allow: []string{"Mon 08:00-09:00 UTC"}}}, []string{"[0].name must be"}},
		"bad name":      {[]ChangeWindow{{Name: "a b", Allow: []string{"Mon 08:00-09:00 UTC"}}}, []string{"[0].name must be"}},
		"twice":         {[]ChangeWindow{{Name: "a", Allow: []string{"Mon 08:00-09:00 UTC"}}, {Name: "a", Deny: []string{"2026-01-01 to 2026-01-02 UTC"}}}, []string{"[1].name: a is declared twice"}},
		"empty":         {[]ChangeWindow{{Name: "a", When: "true"}}, []string{"[0] (a) needs allow or deny entries"}},
		"bad when":      {[]ChangeWindow{{Name: "a", When: "params.env ==", Allow: []string{"Mon 08:00-09:00 UTC"}}}, []string{"[0].when:"}},
		"not boolean":   {[]ChangeWindow{{Name: "a", When: "'prod'", Allow: []string{"Mon 08:00-09:00 UTC"}}}, []string{"[0].when:"}},
		"user":          {[]ChangeWindow{{Name: "a", When: "user.name == 'bob'", Allow: []string{"Mon 08:00-09:00 UTC"}}}, []string{"not user"}},
		"inputs":        {[]ChangeWindow{{Name: "a", When: "inputs.cpu > 8", Allow: []string{"Mon 08:00-09:00 UTC"}}}, []string{"not inputs"}},
		"doc":           {[]ChangeWindow{{Name: "a", When: "has(doc.x) || has(previous.x) || has(action.x)", Allow: []string{"Mon 08:00-09:00 UTC"}}}, []string{"not doc", "not previous", "not action"}},
		"plan":          {[]ChangeWindow{{Name: "a", When: "plan.destroy > 0", Allow: []string{"Mon 08:00-09:00 UTC"}}}, []string{"[0].when:", "plan"}},
		"entries":       {[]ChangeWindow{{Name: "a", Allow: []string{"Mon 08:00-09:00"}, Deny: []string{"2026-01-02 to 2026-01-01 UTC"}}}, []string{"[0].allow[0]:", "[0].deny[0]:"}},
		"too many":      {[]ChangeWindow{{Name: "a", Allow: many}}, []string{"at most 32 allow"}},
		"several parts": {[]ChangeWindow{{Name: "a", Allow: []string{"x"}}, {Name: "b", Deny: []string{"y"}}}, []string{"[0].allow[0]:", "[1].deny[0]:"}},
	} {
		_, err := Compile(tt.specs, "w", zones)
		if err == nil {
			t.Errorf("%s: compiled", name)
			continue
		}
		for _, w := range tt.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: %v, want %q", name, err, w)
			}
		}
	}
	tooMany := make([]ChangeWindow, MaxWindows+1)
	if _, err := Compile(tooMany, "w", zones); err == nil || !strings.Contains(err.Error(), "w: at most 32 windows") {
		t.Errorf("too many windows: %v", err)
	}
}

// state is At's answer in UTC, to compare.
type state struct {
	open          bool
	opens, closes string
	holding       string
}

func stateOf(s State) state {
	out := state{open: s.Open}
	if !s.Opens.IsZero() {
		out.opens = s.Opens.UTC().Format("2006-01-02T15:04Z")
	}
	if !s.Closes.IsZero() {
		out.closes = s.Closes.UTC().Format("2006-01-02T15:04Z")
	}
	var names []string
	for _, w := range s.Holding {
		names = append(names, w.Name)
	}
	out.holding = strings.Join(names, ",")
	return out
}

func TestAt(t *testing.T) {
	t.Parallel()
	hours := ChangeWindow{Name: "business-hours", Allow: []string{"Mon-Thu 08:00-16:00 Europe/Zurich"}}
	freeze := ChangeWindow{Name: "year-end-freeze", Deny: []string{"2026-12-18T00:00 to 2027-01-05T00:00 Europe/Zurich"}}
	for name, tt := range map[string]struct {
		specs []ChangeWindow
		at    string
		want  state
	}{
		"none: always open": {nil, "2026-11-09T10:00Z", state{open: true}},
		// Monday 9 November 2026, 11:00 in Zurich (+01:00).
		"in the hours": {[]ChangeWindow{hours}, "2026-11-09T10:00Z", state{open: true, closes: "2026-11-09T15:00Z", holding: "business-hours"}},
		"at the start": {[]ChangeWindow{hours}, "2026-11-09T07:00Z", state{open: true, closes: "2026-11-09T15:00Z", holding: "business-hours"}},
		"at the end":   {[]ChangeWindow{hours}, "2026-11-09T15:00Z", state{opens: "2026-11-10T07:00Z", holding: "business-hours"}},
		"before":       {[]ChangeWindow{hours}, "2026-11-09T06:59Z", state{opens: "2026-11-09T07:00Z", holding: "business-hours"}},
		// Thursday evening: the next opening is Monday, across the weekend.
		"after thursday": {[]ChangeWindow{hours}, "2026-11-12T16:00Z", state{opens: "2026-11-16T07:00Z", holding: "business-hours"}},
		"on friday":      {[]ChangeWindow{hours}, "2026-11-13T09:00Z", state{opens: "2026-11-16T07:00Z", holding: "business-hours"}},
		// In summer (+02:00).
		"in summer": {[]ChangeWindow{hours}, "2026-07-02T05:00Z", state{opens: "2026-07-02T06:00Z", holding: "business-hours"}},
		// The freeze, in a window of its own, closes the hours it covers.
		"in the freeze": {[]ChangeWindow{hours, freeze}, "2026-12-21T09:00Z", state{opens: "2027-01-05T07:00Z", holding: "year-end-freeze"}},
		"before the freeze": {[]ChangeWindow{hours, freeze}, "2026-12-17T09:00Z",
			state{open: true, closes: "2026-12-17T15:00Z", holding: "business-hours"}},
		// Closed by both, the request waits for both.
		"closed by both": {[]ChangeWindow{hours, freeze}, "2026-12-19T09:00Z", state{opens: "2027-01-05T07:00Z", holding: "business-hours,year-end-freeze"}},
		// A freeze cut into the hours: deny wins over allow, in one window.
		"freeze in a window": {[]ChangeWindow{{Name: "w", Allow: []string{"Mon-Fri 08:00-18:00 UTC"}, Deny: []string{"2026-11-09T12:00 to 2026-11-09T14:00 UTC"}}},
			"2026-11-09T11:00Z", state{open: true, closes: "2026-11-09T12:00Z", holding: "w"}},
		"in that freeze": {[]ChangeWindow{{Name: "w", Allow: []string{"Mon-Fri 08:00-18:00 UTC"}, Deny: []string{"2026-11-09T12:00 to 2026-11-09T14:00 UTC"}}},
			"2026-11-09T12:00Z", state{opens: "2026-11-09T14:00Z", holding: "w"}},
		// Allow entries of one window add up; overlapping ones join.
		"overlapping allows": {[]ChangeWindow{{Name: "w", Allow: []string{"Mon 08:00-12:00 UTC", "Mon 11:00-14:00 UTC", "Mon 14:00-15:00 UTC"}}},
			"2026-11-09T09:00Z", state{open: true, closes: "2026-11-09T15:00Z", holding: "w"}},
		// Windows intersect.
		"two windows": {[]ChangeWindow{{Name: "a", Allow: []string{"Mon-Fri 08:00-18:00 UTC"}}, {Name: "b", Allow: []string{"Mon-Fri 12:00-20:00 UTC"}}},
			"2026-11-09T09:00Z", state{opens: "2026-11-09T12:00Z", holding: "b"}},
		"two windows open": {[]ChangeWindow{{Name: "a", Allow: []string{"Mon-Fri 08:00-18:00 UTC"}}, {Name: "b", Allow: []string{"Mon-Fri 12:00-20:00 UTC"}}},
			"2026-11-09T13:00Z", state{open: true, closes: "2026-11-09T18:00Z", holding: "a"}},
		// Zones: 08:00-12:00 in Zurich and in New York never meet.
		"never meet": {[]ChangeWindow{{Name: "zurich", Allow: []string{"Mon 08:00-12:00 Europe/Zurich"}}, {Name: "new-york", Allow: []string{"Mon 08:00-12:00 America/New_York"}}},
			"2026-11-09T08:00Z", state{holding: "new-york"}},
		// But 08:00-16:00 in Zurich and 07:00-12:00 in New York do, from 12:00 to 15:00 UTC.
		"meet": {[]ChangeWindow{{Name: "zurich", Allow: []string{"Mon 08:00-16:00 Europe/Zurich"}}, {Name: "new-york", Allow: []string{"Mon 07:00-12:00 America/New_York"}}},
			"2026-11-09T08:00Z", state{opens: "2026-11-09T12:00Z", holding: "new-york"}},
		"a zone east": {[]ChangeWindow{{Name: "tokyo", Allow: []string{"Tue 09:00-10:00 Asia/Tokyo"}}},
			"2026-11-10T00:30Z", state{open: true, closes: "2026-11-10T01:00Z", holding: "tokyo"}},
		// A freeze for two years: nothing opens within a year.
		"none opens": {[]ChangeWindow{hours, {Name: "long", Deny: []string{"2026-11-01 to 2028-11-01 UTC"}}}, "2026-11-09T10:00Z", state{holding: "long"}},
		// A freeze that is over: open beyond the horizon.
		"freeze over": {[]ChangeWindow{freeze}, "2027-02-01T10:00Z", state{open: true}},
		// A freeze that starts in more than a year: open, and it closes then... beyond the horizon.
		"freeze far": {[]ChangeWindow{{Name: "far", Deny: []string{"2028-01-01 to 2028-02-01 UTC"}}}, "2026-11-09T10:00Z", state{open: true}},
		// Within the horizon, found past the first stretches.
		"freeze in months": {[]ChangeWindow{{Name: "f", Deny: []string{"2026-11-01 to 2027-03-01 UTC"}}}, "2026-11-09T10:00Z", state{opens: "2027-03-01T00:00Z", holding: "f"}},
		"closes in months": {[]ChangeWindow{{Name: "f", Deny: []string{"2027-03-01 to 2027-04-01 UTC"}}}, "2026-11-09T10:00Z", state{open: true, closes: "2027-03-01T00:00Z", holding: "f"}},
		// Midnight: 22:00-24:00 then 00:00-02:00 the next day join.
		"over midnight": {[]ChangeWindow{{Name: "night", Allow: []string{"Mon 22:00-24:00 UTC", "Tue 00:00-02:00 UTC"}}}, "2026-11-09T23:00Z",
			state{open: true, closes: "2026-11-10T02:00Z", holding: "night"}},
	} {
		got := stateOf(At(compile(t, tt.specs...), utc(tt.at)))
		if got != tt.want {
			t.Errorf("%s: %+v, want %+v", name, got, tt.want)
		}
	}
}

// TestAtDST: wall-clock entries on the days the clock jumps (Europe/Zurich: forward from 02:00 to
// 03:00 on 29 March 2026, back from 03:00 to 02:00 on 25 October 2026).
func TestAtDST(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		allow, deny string
		at          string
		want        state
	}{
		// The skipped hour never holds: the next one is a week later, at 02:00 summer time.
		"skipped hour": {allow: "Sun 02:00-03:00 Europe/Zurich", at: "2026-03-29T00:30Z", want: state{opens: "2026-04-05T00:00Z", holding: "w"}},
		// An entry around it is an hour shorter: 01:00 winter time to 04:00 summer time.
		"around the skipped hour":    {allow: "Sun 01:00-04:00 Europe/Zurich", at: "2026-03-29T00:00Z", want: state{open: true, closes: "2026-03-29T02:00Z", holding: "w"}},
		"after the skipped hour":     {allow: "Sun 03:00-04:00 Europe/Zurich", at: "2026-03-29T00:30Z", want: state{opens: "2026-03-29T01:00Z", holding: "w"}},
		"start in the skipped hour":  {allow: "Sun 02:30-04:00 Europe/Zurich", at: "2026-03-29T00:30Z", want: state{opens: "2026-03-29T01:00Z", holding: "w"}},
		"end in the skipped hour":    {allow: "Sun 01:00-02:30 Europe/Zurich", at: "2026-03-29T00:30Z", want: state{open: true, closes: "2026-03-29T01:00Z", holding: "w"}},
		"the day after spring":       {allow: "Mon 02:00-03:00 Europe/Zurich", at: "2026-03-29T12:00Z", want: state{opens: "2026-03-30T00:00Z", holding: "w"}},
		"repeated hour, both passes": {allow: "Sun 02:00-03:00 Europe/Zurich", at: "2026-10-25T00:00Z", want: state{open: true, closes: "2026-10-25T02:00Z", holding: "w"}},
		// Half of the repeated hour: read twice, so open twice.
		"repeated half hour": {allow: "Sun 02:00-02:30 Europe/Zurich", at: "2026-10-25T00:00Z", want: state{open: true, closes: "2026-10-25T00:30Z", holding: "w"}},
		"between the passes": {allow: "Sun 02:00-02:30 Europe/Zurich", at: "2026-10-25T00:45Z", want: state{opens: "2026-10-25T01:00Z", holding: "w"}},
		"second pass":        {allow: "Sun 02:00-02:30 Europe/Zurich", at: "2026-10-25T01:10Z", want: state{open: true, closes: "2026-10-25T01:30Z", holding: "w"}},
		// A freeze in the repeated hour covers it from the first reading of its start to the last
		// reading of its end.
		"freeze in the repeated hour": {deny: "2026-10-25T02:30 to 2026-10-25T02:45 Europe/Zurich", at: "2026-10-25T00:50Z",
			want: state{opens: "2026-10-25T01:45Z", holding: "w"}},
		// A freeze across the change ends at its wall-clock end, in summer time.
		"freeze across spring": {deny: "2026-03-28T12:00 to 2026-03-29T12:00 Europe/Zurich", at: "2026-03-29T09:00Z",
			want: state{opens: "2026-03-29T10:00Z", holding: "w"}},
		// New York jumps on other days (8 March and 1 November 2026), at 02:00 local.
		"new york spring": {allow: "Sun 02:00-03:00 America/New_York", at: "2026-03-08T06:00Z", want: state{opens: "2026-03-15T06:00Z", holding: "w"}},
		"new york fall":   {allow: "Sun 01:00-02:00 America/New_York", at: "2026-11-01T05:00Z", want: state{open: true, closes: "2026-11-01T07:00Z", holding: "w"}},
		// Sydney goes forward in October and back in April.
		"sydney": {allow: "Sun 02:00-03:00 Australia/Sydney", at: "2026-10-03T14:00Z", want: state{opens: "2026-10-10T15:00Z", holding: "w"}},
	} {
		s := ChangeWindow{Name: "w"}
		if tt.allow != "" {
			s.Allow = []string{tt.allow}
		}
		if tt.deny != "" {
			s.Deny = []string{tt.deny}
		}
		if got := stateOf(At(compile(t, s), utc(tt.at))); got != tt.want {
			t.Errorf("%s: %+v, want %+v", name, got, tt.want)
		}
	}
}

// TestAtAgrees: At's answer agrees with each window's own Open, at random times of two years,
// across time zones and their changes: open when every window is, opening and closing when they
// say, and not before.
func TestAtAgrees(t *testing.T) {
	t.Parallel()
	ws := compile(t,
		ChangeWindow{Name: "hours", Allow: []string{"Mon-Thu 08:00-16:00 Europe/Zurich", "Fri 08:00-12:00 Europe/Zurich", "Sun 01:30-03:30 Europe/Zurich"}},
		ChangeWindow{Name: "ny", Allow: []string{"Mon-Fri 00:00-11:00 America/New_York", "Sat-Sun 01:00-03:00 America/New_York"}},
		ChangeWindow{Name: "freezes", Deny: []string{"2026-12-18 to 2027-01-05 Europe/Zurich", "2027-03-28T02:30 to 2027-03-28T05:00 Europe/Zurich",
			"2027-10-31T02:15 to 2027-10-31T02:45 Europe/Zurich"}},
	)
	open := func(at time.Time) bool {
		for _, w := range ws {
			if !w.Open(at) {
				return false
			}
		}
		return true
	}
	rng := rand.New(rand.NewPCG(1, 2))
	start := utc("2026-01-01T00:00Z")
	for range 400 {
		at := start.Add(time.Duration(rng.Int64N(int64(2*365*day))) / time.Minute * time.Minute)
		s := At(ws, at)
		if s.Open != open(at) {
			t.Fatalf("at %s: At says open %v, the windows %v", at, s.Open, open(at))
		}
		next := s.Opens
		if s.Open {
			next = s.Closes
		}
		if next.IsZero() {
			t.Fatalf("at %s: %+v", at, s)
		}
		if open(next) == s.Open || open(next.Add(-time.Minute)) != s.Open || len(s.Holding) == 0 {
			t.Fatalf("at %s: %+v, but open at it %v and a minute before %v", at, s, open(next), open(next.Add(-time.Minute)))
		}
		// Nothing changes in between, minute by minute (sampled).
		for m := at; m.Before(next); m = m.Add(time.Duration(1+rng.IntN(97)) * time.Minute) {
			if open(m) != s.Open {
				t.Fatalf("at %s: %+v, but at %s open is %v", at, s, m, open(m))
			}
		}
	}
}

// TestAtBounded: windows that never open are decided quickly, as none.
func TestAtBounded(t *testing.T) {
	t.Parallel()
	var specs []ChangeWindow
	for i := range MaxWindows {
		s := ChangeWindow{Name: "w" + string(rune('a'+i%26)) + string(rune('a'+i/26))}
		for j := range MaxEntries {
			// Every window's entries are on other hours than the others': they never meet.
			s.Allow = append(s.Allow, "Mon-Sun "+hhmm(i*40+j)+"-"+hhmm(i*40+j+1)+" Europe/Zurich")
		}
		specs = append(specs, s)
	}
	ws := compile(t, specs...)
	begin := time.Now()
	s := At(ws, utc("2026-11-09T10:00Z"))
	if s.Open || !s.Opens.IsZero() || len(s.Holding) == 0 {
		t.Errorf("never open: %+v", stateOf(s))
	}
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("took %s", d)
	}
}

func hhmm(m int) string {
	return time.Date(2000, 1, 1, 0, m, 0, 0, time.UTC).Format("15:04")
}

func TestApplies(t *testing.T) {
	t.Parallel()
	ws := compile(t,
		ChangeWindow{Name: "prod", When: "params.env == 'prod'", Allow: []string{"Mon 08:00-09:00 UTC"}},
		ChangeWindow{Name: "safe", When: "has(params.env) && params.env == 'prod'", Allow: []string{"Mon 08:00-09:00 UTC"}},
		ChangeWindow{Name: "vm", When: "request.type == 'vm' && request.kind != 'delete' && request.repository == 'infra-live'", Allow: []string{"Mon 08:00-09:00 UTC"}},
		ChangeWindow{Name: "all", Allow: []string{"Mon 08:00-09:00 UTC"}},
	)
	prod := expr.Vars{Params: map[string]any{"env": "prod"}, Request: expr.Request{Kind: "create", Type: "vm", Repository: "infra-live"}}
	dev := expr.Vars{Params: map[string]any{"env": "dev"}, Request: expr.Request{Kind: "delete", Type: "vm", Repository: "infra-live"}}
	bare := expr.Vars{Request: expr.Request{Kind: "create", Type: "dns", Repository: "infra-live"}}
	for name, tt := range map[string]struct {
		w        int
		subjects []expr.Vars
		want     bool
		err      string
	}{
		"prod":                {0, []expr.Vars{prod}, true, ""},
		"dev":                 {0, []expr.Vars{dev}, false, ""},
		"one of them":         {0, []expr.Vars{dev, prod}, true, ""},
		"no env: an error":    {0, []expr.Vars{bare}, false, "change window prod:"},
		"an error dominates":  {0, []expr.Vars{prod, bare}, false, "change window prod:"},
		"guarded":             {1, []expr.Vars{bare}, false, ""},
		"by type and kind":    {2, []expr.Vars{prod}, true, ""},
		"not on delete":       {2, []expr.Vars{dev}, false, ""},
		"no condition":        {3, []expr.Vars{dev}, true, ""},
		"no subjects applies": {0, nil, true, ""},
	} {
		got, err := ws[tt.w].Applies(tt.subjects)
		if (err == nil) != (tt.err == "") || (err != nil && !strings.Contains(err.Error(), tt.err)) || got != tt.want {
			t.Errorf("%s: %v %v, want %v %q", name, got, err, tt.want, tt.err)
		}
	}
}

func TestWhen(t *testing.T) {
	t.Parallel()
	zurich, _ := time.LoadLocation("Europe/Zurich")
	now := utc("2026-11-12T16:00Z")
	if got := When(utc("2026-11-16T07:00Z"), now, zurich); got != "Mon 08:00 Europe/Zurich" {
		t.Errorf("within the week: %q", got)
	}
	if got := When(utc("2027-01-05T07:00Z"), now, zurich); got != "Tue 5 Jan 2027 08:00 Europe/Zurich" {
		t.Errorf("further: %q", got)
	}
	if got := When(utc("2026-11-16T07:00Z"), now, time.UTC); got != "Mon 07:00 UTC" {
		t.Errorf("in UTC: %q", got)
	}
	s := At(compile(t, ChangeWindow{Name: "w", Allow: []string{"Mon 08:00-09:00 Asia/Tokyo"}}, ChangeWindow{Name: "x", Deny: []string{"2020-01-01 to 2020-01-02 UTC"}}), now)
	if s.Zone().String() != "Asia/Tokyo" {
		t.Errorf("zone %s", s.Zone())
	}
}
