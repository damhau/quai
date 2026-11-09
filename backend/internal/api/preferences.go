package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/audit"
	"github.com/damhau/quai/backend/internal/authz"
	"github.com/damhau/quai/backend/internal/notify"
	"github.com/damhau/quai/backend/internal/store"
	"github.com/damhau/quai/backend/internal/store/db"
)

// EventPreferenceView is how a kind of notification reaches the caller on a channel. The enums are
// config.NotificationKinds, config.NotificationChannels and config.NotificationModes (a test keeps
// them equal).
type EventPreferenceView struct {
	Kind    string   `json:"kind" enum:"approval_needed,submitted,approved,rejected,stale,merged,applied,failed,expiring_soon"`
	Channel string   `json:"channel" enum:"email"`
	Mode    string   `json:"mode" enum:"immediate,digest,off" doc:"At once, in the daily digest, or not at all"`
	Default string   `json:"default" enum:"immediate,digest,off" doc:"The mode until one is chosen (notifications.defaults)"`
	Modes   []string `json:"modes" doc:"The modes one may choose: approval_needed cannot be off on every channel"`
}

// DigestView is when a daily digest goes: an hour of the day in a time zone.
type DigestView struct {
	Hour     int    `json:"hour" minimum:"0" maximum:"23"`
	TimeZone string `json:"timeZone" doc:"An IANA time zone, such as Europe/Zurich"`
}

// PreferencesOutput is the caller's notification preferences.
type PreferencesOutput struct {
	Body struct {
		Channels      []string              `json:"channels" doc:"The channels configured; none: Quai sends no notifications"`
		Events        []EventPreferenceView `json:"events" doc:"For each kind one chooses and channel configured. Alerts are not among them: they always go at once"`
		Digest        DigestView            `json:"digest"`
		DefaultDigest DigestView            `json:"defaultDigest" doc:"When the digest goes until one chooses (notifications.defaults.digest)"`
		Customized    bool                  `json:"customized" doc:"Whether anything differs from the defaults"`
	}
}

// EventPreferenceInput is how the caller wants a kind of notification on a channel.
type EventPreferenceInput struct {
	Kind    string `json:"kind" enum:"approval_needed,submitted,approved,rejected,stale,merged,applied,failed,expiring_soon"`
	Channel string `json:"channel" enum:"email"`
	Mode    string `json:"mode" enum:"immediate,digest,off"`
}

// DigestInput is when the caller wants their daily digest.
type DigestInput struct {
	Hour     int    `json:"hour" minimum:"0" maximum:"23"`
	TimeZone string `json:"timeZone" minLength:"1" maxLength:"100" doc:"An IANA time zone, such as Europe/Zurich"`
}

// UpdatePreferencesInput sets the caller's notification preferences: a kind and channel left out
// follows the default, as does a choice equal to it.
type UpdatePreferencesInput struct {
	Body struct {
		Events []EventPreferenceInput `json:"events" maxItems:"64"`
		Digest DigestInput            `json:"digest"`
	}
}

// registerPreferences registers the caller's notification preferences: theirs only, the name from
// the session, never from the path or the body; API tokens can neither read nor change them
// (authz.ManageOwnPreferences, open question 58).
func registerPreferences(a huma.API, s *store.Store, settings notify.Settings) {
	huma.Register(a, huma.Operation{
		OperationID: "get-preferences", Method: http.MethodGet, Path: Prefix + "/me/preferences",
		Summary: "Get your notification preferences", Tags: []string{"Identity"},
	}, func(ctx context.Context, _ *struct{}) (*PreferencesOutput, error) {
		id, err := authorize(ctx, authz.ManageOwnPreferences, authz.Object{})
		if err != nil {
			return nil, err
		}
		row, err := s.GetNotificationPreferences(ctx, id.Principal.Name)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return preferencesView(settings, notify.PreferencesOf(row)), nil
	})

	huma.Register(a, huma.Operation{
		OperationID: "update-preferences", Method: http.MethodPut, Path: Prefix + "/me/preferences",
		Summary: "Set your notification preferences", Tags: []string{"Identity"},
		Description: "Each kind and channel left out, or chosen as its default, follows the default. Alerts are not a kind one " +
			"chooses: they always go at once. Approval needed cannot be off on every channel.",
	}, func(ctx context.Context, in *UpdatePreferencesInput) (*PreferencesOutput, error) {
		id, err := authorize(ctx, authz.ManageOwnPreferences, authz.Object{})
		if err != nil {
			return nil, err
		}
		choices := make([]notify.Choice, len(in.Body.Events))
		for i, e := range in.Body.Events {
			choices[i] = notify.Choice{Kind: e.Kind, Channel: e.Channel, Mode: e.Mode}
		}
		p, ferrs := settings.Choose(choices, in.Body.Digest.Hour, in.Body.Digest.TimeZone)
		if len(ferrs) > 0 {
			details := make([]error, len(ferrs))
			for i, f := range ferrs {
				details[i] = &huma.ErrorDetail{Location: f.Location, Message: f.Message}
			}
			return nil, huma.Error422UnprocessableEntity("some preferences need attention", details...)
		}
		modes, err := json.Marshal(p.Modes)
		if err != nil {
			return nil, err
		}
		name := id.Principal.Name
		err = s.Tx(ctx, func(tx pgx.Tx, q *db.Queries) error {
			row, err := q.GetNotificationPreferences(ctx, name)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if notify.PreferencesOf(row).Equal(p) {
				return nil // nothing changes: nothing to record
			}
			params := db.SetNotificationPreferencesParams{Name: name, Modes: modes, TimeZone: pgtype.Text{String: p.TimeZone, Valid: p.TimeZone != ""}}
			if p.Hour != nil {
				params.DigestHour = pgtype.Int2{Int16: int16(*p.Hour), Valid: true} // #nosec G115 -- 0 to 23 (Choose)
			}
			if err := q.SetNotificationPreferences(ctx, params); err != nil {
				return err
			}
			// The choices differ from the defaults; the time zone says where someone is: personal.
			return audit.Add(ctx, tx, audit.Event{Action: "preferences.changed", Object: audit.Object{Type: "user", ID: name},
				Details: map[string]any{"choices": p.Choices(), "digestHour": in.Body.Digest.Hour,
					"timeZone": audit.Personal(in.Body.Digest.TimeZone)}})
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			// The directory no longer holds the caller: erased since they signed in.
			return nil, huma.Error409Conflict("Quai has no record of you: sign in again")
		}
		if err != nil {
			return nil, err
		}
		return preferencesView(settings, p), nil
	})
}

func preferencesView(settings notify.Settings, p notify.Preferences) *PreferencesOutput {
	out := &PreferencesOutput{}
	out.Body.Channels = settings.Channels()
	if out.Body.Channels == nil {
		out.Body.Channels = []string{}
	}
	out.Body.Events = []EventPreferenceView{}
	for _, e := range settings.Effective(p) {
		out.Body.Events = append(out.Body.Events, EventPreferenceView{Kind: e.Kind, Channel: e.Channel, Mode: e.Mode, Default: e.Default, Modes: e.Modes})
	}
	hour, loc := settings.Digest(p)
	out.Body.Digest = DigestView{Hour: hour, TimeZone: loc.String()}
	defHour, defZone := settings.DefaultDigest()
	out.Body.DefaultDigest = DigestView{Hour: defHour, TimeZone: defZone}
	out.Body.Customized = !p.Equal(notify.Preferences{})
	return out
}
