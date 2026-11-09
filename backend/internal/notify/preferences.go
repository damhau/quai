package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/store/db"
)

// Settings are what people may choose of their notifications, and what they get until they do:
// the channels configured, and notifications.defaults. The zero value configures no channel,
// sends every kind at once, and digests at midnight UTC; NewSettings has config.DefaultDigestHour
// when the configuration has no hour.
type Settings struct {
	channels []string
	defaults map[string]map[string]string // channel -> kind -> mode
	hour     int
	zone     string
}

// NewSettings returns the settings of the configuration's notifications, which config.Load
// validated.
func NewSettings(n config.Notifications) Settings {
	s := Settings{channels: n.Channels(), defaults: n.Defaults.Channels, hour: config.DefaultDigestHour, zone: n.Defaults.Digest.TimeZone}
	if h := n.Defaults.Digest.Hour; h != nil {
		s.hour = *h
	}
	return s
}

// Channels are the channels configured.
func (s Settings) Channels() []string { return slices.Clone(s.channels) }

// DefaultMode is how a kind of notification reaches someone on a channel until they choose.
func (s Settings) DefaultMode(channel, kind string) string {
	if m := s.defaults[channel][kind]; m != "" && kind != Alert {
		return m
	}
	return config.ModeImmediate
}

// DefaultDigest is when digests go until someone chooses: an hour and an IANA time zone.
func (s Settings) DefaultDigest() (int, string) {
	if s.zone == "" {
		return s.hour, "UTC"
	}
	return s.hour, s.zone
}

// Modes are the modes a kind may have on a channel. Alerts always go at once. Approval needed is
// never off on every channel: approvers must hear of what waits for them, so with one channel it
// is immediate or in the digest.
func (s Settings) Modes(kind string) []string {
	switch kind {
	case Alert:
		return []string{config.ModeImmediate}
	case ApprovalNeeded:
		if len(s.channels) < 2 {
			return []string{config.ModeImmediate, config.ModeDigest}
		}
	}
	return slices.Clone(config.NotificationModes)
}

// Preferences are a person's choices, where they differ from the defaults.
type Preferences struct {
	Modes    map[string]map[string]string // channel -> kind -> mode
	Hour     *int
	TimeZone string // empty: the default
}

// PreferencesOf returns the preferences a row holds.
func PreferencesOf(row db.NotificationPreference) Preferences {
	var p Preferences
	_ = json.Unmarshal(row.Modes, &p.Modes)
	if row.DigestHour.Valid {
		h := int(row.DigestHour.Int16)
		p.Hour = &h
	}
	p.TimeZone = row.TimeZone.String
	return p
}

// Mode returns how a kind of notification reaches someone on a channel: their choice, or the
// default. Alerts always go at once, whatever is stored, and approval needed goes at once
// everywhere when it would otherwise be off on every channel (a channel removed since).
func (s Settings) Mode(p Preferences, channel, kind string) string {
	switch kind {
	case Alert:
		return config.ModeImmediate
	case ApprovalNeeded:
		if !slices.ContainsFunc(s.channels, func(c string) bool { return s.chosen(p, c, kind) != config.ModeOff }) {
			return config.ModeImmediate
		}
	}
	return s.chosen(p, channel, kind)
}

// chosen is someone's choice for a kind on a channel when it is still one they may make (the
// channel configured, the mode allowed), the default otherwise.
func (s Settings) chosen(p Preferences, channel, kind string) string {
	if m := p.Modes[channel][kind]; m != "" && slices.Contains(s.channels, channel) && slices.Contains(s.Modes(kind), m) {
		return m
	}
	return s.DefaultMode(channel, kind)
}

// Digest returns when someone's digest goes: an hour of the day in their time zone. A time zone
// the system no longer knows is the default's, or UTC.
func (s Settings) Digest(p Preferences) (int, *time.Location) {
	hour, zone := s.DefaultDigest()
	if p.Hour != nil {
		hour = *p.Hour
	}
	for _, z := range []string{p.TimeZone, zone} {
		if z == "" {
			continue
		}
		if loc, err := config.TimeZone(z); err == nil {
			return hour, loc
		}
	}
	return hour, time.UTC
}

// Choice is how someone wants a kind of notification on a channel.
type Choice struct {
	Kind, Channel, Mode string
}

// FieldError is what is wrong with a field of someone's preferences, at its location in the
// request's body.
type FieldError struct {
	Location, Message string
}

// Choose checks someone's choices and when their digest goes, and returns the preferences to
// keep: those that differ from the defaults. The kinds, channels and modes come from fixed lists;
// alerts are not a kind one chooses; the channel must be configured; the hour is 0 to 23; the time
// zone an IANA name (config.TimeZone).
func (s Settings) Choose(choices []Choice, hour int, zone string) (Preferences, []FieldError) {
	var errs []FieldError
	fail := func(loc, format string, args ...any) {
		errs = append(errs, FieldError{loc, fmt.Sprintf(format, args...)})
	}
	p := Preferences{Modes: map[string]map[string]string{}}
	chosen := map[[2]string]bool{}
	for i, c := range choices {
		at := fmt.Sprintf("body.events[%d]", i)
		switch {
		case c.Kind == Alert:
			fail(at+".kind", "alerts always go at once: they cannot be turned off or put in a digest")
			continue
		case !slices.Contains(config.NotificationKinds, c.Kind):
			fail(at+".kind", "%q is not a kind of notification (%s)", c.Kind, strings.Join(config.NotificationKinds, ", "))
			continue
		case !slices.Contains(config.NotificationChannels, c.Channel):
			fail(at+".channel", "%q is not a channel (%s)", c.Channel, strings.Join(config.NotificationChannels, ", "))
			continue
		case !slices.Contains(s.channels, c.Channel):
			fail(at+".channel", "the %s channel is not configured", c.Channel)
			continue
		case !slices.Contains(config.NotificationModes, c.Mode):
			fail(at+".mode", "%q is not a mode (%s)", c.Mode, strings.Join(config.NotificationModes, ", "))
			continue
		case !slices.Contains(s.Modes(c.Kind), c.Mode):
			fail(at+".mode", "%s cannot be %s on %s: approvers must hear of what waits for them on some channel", c.Kind, c.Mode, c.Channel)
			continue
		case chosen[[2]string{c.Kind, c.Channel}]:
			fail(at, "%s on %s is chosen twice", c.Kind, c.Channel)
			continue
		}
		chosen[[2]string{c.Kind, c.Channel}] = true
		if c.Mode != s.DefaultMode(c.Channel, c.Kind) {
			if p.Modes[c.Channel] == nil {
				p.Modes[c.Channel] = map[string]string{}
			}
			p.Modes[c.Channel][c.Kind] = c.Mode
		}
	}
	if len(s.channels) > 0 && !slices.ContainsFunc(s.channels, func(c string) bool { return s.chosen(p, c, ApprovalNeeded) != config.ModeOff }) {
		fail("body.events", "approval_needed cannot be off on every channel: approvers must hear of what waits for them")
	}
	if hour < 0 || hour > 23 {
		fail("body.digest.hour", "must be 0 to 23, not %d", hour)
	}
	if _, err := config.TimeZone(zone); err != nil {
		fail("body.digest.timeZone", "%s", err)
	}
	if len(errs) > 0 {
		return Preferences{}, errs
	}
	defHour, defZone := s.DefaultDigest()
	if hour != defHour {
		p.Hour = &hour
	}
	if zone != defZone {
		p.TimeZone = zone
	}
	return p, nil
}

// Equal reports whether two preferences hold the same choices.
func (p Preferences) Equal(o Preferences) bool {
	return p.TimeZone == o.TimeZone && (p.Hour == nil) == (o.Hour == nil) && (p.Hour == nil || *p.Hour == *o.Hour) &&
		maps.EqualFunc(nonEmpty(p.Modes), nonEmpty(o.Modes), maps.Equal)
}

func nonEmpty(m map[string]map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for k, v := range m {
		if len(v) > 0 {
			out[k] = v
		}
	}
	return out
}

// Choices lists the preferences' choices, sorted: channel/kind=mode.
func (p Preferences) Choices() []string {
	out := []string{}
	for channel, kinds := range p.Modes {
		for kind, mode := range kinds {
			out = append(out, channel+"/"+kind+"="+mode)
		}
	}
	slices.Sort(out)
	return out
}

// Effective is how a kind of notification reaches someone on a channel: their mode, the default,
// and the modes they may choose.
type Effective struct {
	Kind, Channel, Mode, Default string
	Modes                        []string
}

// Effective lists, for each kind one chooses and each channel configured, how it reaches someone.
func (s Settings) Effective(p Preferences) []Effective {
	var out []Effective
	for _, kind := range config.NotificationKinds {
		for _, channel := range s.channels {
			out = append(out, Effective{Kind: kind, Channel: channel, Mode: s.Mode(p, channel, kind), Default: s.DefaultMode(channel, kind),
				Modes: s.Modes(kind)})
		}
	}
	return out
}

// preferences returns a person's preferences; none when they have chosen nothing.
func (s *Service) preferences(ctx context.Context, name string) (Preferences, error) {
	row, err := s.opts.Store.GetNotificationPreferences(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Preferences{}, nil
	}
	if err != nil {
		return Preferences{}, err
	}
	return PreferencesOf(row), nil
}

// lastDue returns when the latest digest of someone whose digest goes at hour in loc was due, at
// or before now, and its period: the local date it was due on, as midnight UTC. The hour is the
// wall clock's: across a change to or from daylight saving time the digest still goes at that
// hour, 23 or 25 hours after the one before. An hour a change skips (02:00 when clocks go from
// 02:00 to 03:00) is the moment of the change; an hour that happens twice is the first.
func lastDue(now time.Time, hour int, loc *time.Location) (due, period time.Time) {
	y, m, d := now.In(loc).Date()
	due = slot(y, m, d, hour, loc)
	if due.After(now) {
		y, m, d = time.Date(y, m, d-1, 12, 0, 0, 0, time.UTC).Date()
		due = slot(y, m, d, hour, loc)
	}
	return due, time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// slot is hour o'clock on a date in loc: the first such instant, or, when the clocks skip that
// hour, the instant they change.
func slot(y int, m time.Month, d, hour int, loc *time.Location) time.Time {
	t := time.Date(y, m, d, hour, 0, 0, 0, loc)
	if t.Hour() != hour {
		// Skipped: Go normalizes to an hour later; the change is at the start of that hour.
		return time.Date(y, m, d, t.Hour(), 0, 0, 0, loc)
	}
	// Twice: the earlier of the two, an hour before, when it reads the same.
	if e := t.Add(-time.Hour); e.In(loc).Hour() == hour && e.In(loc).Day() == d {
		return e
	}
	return t
}
