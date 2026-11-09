package notify

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/damhau/quai/backend/internal/config"
)

func settings(t *testing.T, defaults string) Settings {
	t.Helper()
	hour, zone := 8, "Europe/Zurich"
	n := config.Notifications{Email: &config.Email{}, Defaults: config.NotificationDefaults{Digest: config.DigestDefaults{Hour: &hour, TimeZone: zone}}}
	if defaults != "" {
		n.Defaults.Channels = map[string]map[string]string{"email": {}}
		for _, kv := range strings.Split(defaults, ",") {
			k, v, _ := strings.Cut(kv, "=")
			n.Defaults.Channels["email"][k] = v
		}
	}
	return NewSettings(n)
}

// TestMode: someone's choice, else the default; alerts always at once; approval needed never off
// everywhere, whatever is stored.
func TestMode(t *testing.T) {
	s := settings(t, "approved=digest,stale=off")
	none := Preferences{}
	chosen := Preferences{Modes: map[string]map[string]string{"email": {Approved: "immediate", Merged: "digest", Alert: "off", ApprovalNeeded: "off", Submitted: "weekly"}}}
	for _, tt := range []struct {
		p          Preferences
		kind, want string
	}{
		{none, Submitted, "immediate"},
		{none, Approved, "digest"},
		{none, Stale, "off"},
		{none, Alert, "immediate"},
		{chosen, Approved, "immediate"},
		{chosen, Merged, "digest"},
		{chosen, Stale, "off"},
		{chosen, Alert, "immediate"},          // never off
		{chosen, ApprovalNeeded, "immediate"}, // off on every channel: at once
		{chosen, Submitted, "immediate"},      // not a mode: the default
	} {
		if got := s.Mode(tt.p, "email", tt.kind); got != tt.want {
			t.Errorf("Mode(%v, %s) = %s, want %s", tt.p.Modes, tt.kind, got, tt.want)
		}
	}
	if got := s.Modes(ApprovalNeeded); !slices.Equal(got, []string{"immediate", "digest"}) {
		t.Errorf("approval needed may be %v with one channel", got)
	}
	if got := (Settings{}).Mode(chosen, "email", Merged); got != "immediate" {
		t.Errorf("with no channel configured, Mode = %s", got)
	}
}

// TestChoose: the server checks every choice against fixed lists and keeps only what differs from
// the defaults.
func TestChoose(t *testing.T) {
	s := settings(t, "approved=digest")
	p, errs := s.Choose([]Choice{{Approved, "email", "digest"}, {Merged, "email", "off"}, {ApprovalNeeded, "email", "digest"}, {Submitted, "email", "immediate"}},
		8, "Europe/Zurich")
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if !slices.Equal(p.Choices(), []string{"email/approval_needed=digest", "email/merged=off"}) || p.Hour != nil || p.TimeZone != "" {
		t.Errorf("kept %v, hour %v, zone %q: want only what differs from the defaults", p.Choices(), p.Hour, p.TimeZone)
	}
	p, errs = s.Choose(nil, 7, "America/New_York")
	if len(errs) > 0 || p.Hour == nil || *p.Hour != 7 || p.TimeZone != "America/New_York" {
		t.Errorf("digest = %v, %q, %v", p.Hour, p.TimeZone, errs)
	}
	if !p.Equal(Preferences{Hour: p.Hour, TimeZone: "America/New_York", Modes: map[string]map[string]string{"email": {}}}) {
		t.Error("an empty channel's choices are none")
	}
	for name, tt := range map[string]struct {
		choices  []Choice
		hour     int
		zone     string
		location string
		want     string
	}{
		"alert":          {[]Choice{{Alert, "email", "off"}}, 8, "UTC", "body.events[0].kind", "alerts always go at once"},
		"unknown kind":   {[]Choice{{"deployed", "email", "off"}}, 8, "UTC", "body.events[0].kind", "not a kind"},
		"unknown":        {[]Choice{{Merged, "sms", "off"}}, 8, "UTC", "body.events[0].channel", `"sms" is not a channel`},
		"unknown mode":   {[]Choice{{Merged, "email", "weekly"}}, 8, "UTC", "body.events[0].mode", "not a mode"},
		"approvals off":  {[]Choice{{Merged, "email", "off"}, {ApprovalNeeded, "email", "off"}}, 8, "UTC", "body.events[1].mode", "cannot be off"},
		"twice":          {[]Choice{{Merged, "email", "off"}, {Merged, "email", "digest"}}, 8, "UTC", "body.events[1]", "chosen twice"},
		"hour":           {nil, 24, "UTC", "body.digest.hour", "0 to 23"},
		"negative hour":  {nil, -1, "UTC", "body.digest.hour", "0 to 23"},
		"local":          {nil, 8, "Local", "body.digest.timeZone", "not an IANA time zone"},
		"path":           {nil, 8, "../../etc/passwd", "body.digest.timeZone", "not an IANA time zone"},
		"absolute path":  {nil, 8, "/etc/localtime", "body.digest.timeZone", "not an IANA time zone"},
		"unknown zone":   {nil, 8, "Mars/Olympus_Mons", "body.digest.timeZone", "not a known time zone"},
		"empty timezone": {nil, 8, "", "body.digest.timeZone", "not an IANA time zone"},
	} {
		_, errs := s.Choose(tt.choices, tt.hour, tt.zone)
		if !slices.ContainsFunc(errs, func(e FieldError) bool { return e.Location == tt.location && strings.Contains(e.Message, tt.want) }) {
			t.Errorf("%s: errors = %v, want %q at %s", name, errs, tt.want, tt.location)
		}
	}
	if _, errs := (Settings{}).Choose([]Choice{{Merged, "email", "off"}}, 8, "UTC"); len(errs) != 1 || !strings.Contains(errs[0].Message, "not configured") {
		t.Errorf("a channel not configured: %v", errs)
	}
}

// TestLastDue: a digest goes at its hour on the wall clock of the recipient's time zone, across
// changes to and from daylight saving time (Europe/Zurich: 29 March and 25 October 2026).
func TestLastDue(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tt := range []struct {
		now         string
		hour        int
		loc         *time.Location
		due, period string
	}{
		{"2026-03-28T06:59:00Z", 8, zurich, "2026-03-27T07:00:00Z", "2026-03-27"}, // 07:59 CET: yesterday's
		{"2026-03-28T07:00:00Z", 8, zurich, "2026-03-28T07:00:00Z", "2026-03-28"}, // 08:00 CET
		{"2026-03-29T05:59:00Z", 8, zurich, "2026-03-28T07:00:00Z", "2026-03-28"}, // 07:59 CEST
		{"2026-03-29T06:00:00Z", 8, zurich, "2026-03-29T06:00:00Z", "2026-03-29"}, // 08:00 CEST, 23 hours later
		{"2026-10-24T06:00:00Z", 8, zurich, "2026-10-24T06:00:00Z", "2026-10-24"}, // 08:00 CEST
		{"2026-10-25T06:30:00Z", 8, zurich, "2026-10-24T06:00:00Z", "2026-10-24"}, // 07:30 CET
		{"2026-10-25T07:00:00Z", 8, zurich, "2026-10-25T07:00:00Z", "2026-10-25"}, // 08:00 CET, 25 hours later
		{"2026-03-29T01:00:00Z", 2, zurich, "2026-03-29T01:00:00Z", "2026-03-29"}, // 02:00 is skipped: the change, 03:00 CEST
		{"2026-03-29T00:59:00Z", 2, zurich, "2026-03-28T01:00:00Z", "2026-03-28"},
		{"2026-10-25T00:00:00Z", 2, zurich, "2026-10-25T00:00:00Z", "2026-10-25"}, // 02:00 twice: the first, CEST
		{"2026-10-25T01:30:00Z", 2, zurich, "2026-10-25T00:00:00Z", "2026-10-25"}, // the second does not make another
		{"2026-03-01T00:30:00Z", 23, time.UTC, "2026-02-28T23:00:00Z", "2026-02-28"},
		{"2027-01-01T05:00:00Z", 8, zurich, "2026-12-31T07:00:00Z", "2026-12-31"},
	} {
		due, period := lastDue(utc(tt.now), tt.hour, tt.loc)
		if !due.Equal(utc(tt.due)) || period.Format(time.DateOnly) != tt.period {
			t.Errorf("lastDue(%s, %d, %s) = %s, %s; want %s, %s", tt.now, tt.hour, tt.loc, due.UTC().Format(time.RFC3339), period.Format(time.DateOnly), tt.due, tt.period)
		}
	}
}
