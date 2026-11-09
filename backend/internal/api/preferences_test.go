package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/notify"
	"github.com/damhau/quai/backend/internal/store/db"
)

// prefs is a body of PUT /me/preferences: kind=mode on email, and when the digest goes.
func prefs(hour int, zone string, modes ...string) map[string]any {
	events := []map[string]string{}
	for _, m := range modes {
		kind, mode, _ := strings.Cut(m, "=")
		events = append(events, map[string]string{"kind": kind, "channel": "email", "mode": mode})
	}
	return map[string]any{"events": events, "digest": map[string]any{"hour": hour, "timeZone": zone}}
}

// setPrefs sets someone's preferences, and fails the test when they are refused.
func (u *user) setPrefs(body map[string]any) PreferencesOutput {
	u.h.t.Helper()
	var out PreferencesOutput
	if status, raw := u.do(http.MethodPut, Prefix+"/me/preferences", body, &out.Body); status != http.StatusOK {
		u.h.t.Fatalf("set preferences = %d %s", status, raw)
	}
	return out
}

// modes returns the modes of a view, as kind=mode on email.
func modes(v PreferencesOutput) []string {
	var out []string
	for _, e := range v.Body.Events {
		out = append(out, e.Kind+"="+e.Mode)
	}
	return out
}

// TestPreferences: people read and change their own notification preferences, never someone
// else's, in a browser session only; the server checks every value, keeps what differs from the
// defaults, and records each change in the audit log, pseudonymised. Alerts cannot be turned off,
// even by a preference stored behind the API's back.
func TestPreferences(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := notifiedHarness(t, mail, "{ name: infra-live, title: Infrastructure, owners: [user:bob] }")
	bob, alice := h.as("bob"), h.as("alice")

	var v PreferencesOutput
	if status, raw := bob.do(http.MethodGet, Prefix+"/me/preferences", nil, &v.Body); status != http.StatusOK {
		t.Fatalf("get = %d %s", status, raw)
	}
	want := []string{"approval_needed=immediate", "submitted=immediate", "approved=immediate", "rejected=immediate", "stale=immediate", "merged=immediate",
		"applied=immediate", "failed=immediate", "expiring_soon=immediate", "expired=immediate"}
	if !slices.Equal(modes(v), want) || !slices.Equal(v.Body.Channels, []string{"email"}) || v.Body.Customized ||
		v.Body.Digest != (DigestView{8, "UTC"}) || v.Body.DefaultDigest != (DigestView{8, "UTC"}) {
		t.Errorf("defaults = %+v", v.Body)
	}
	if e := v.Body.Events[0]; !slices.Equal(e.Modes, []string{"immediate", "digest"}) || e.Default != "immediate" {
		t.Errorf("approval needed = %+v: with one channel it is never off", e)
	}
	if e := v.Body.Events[1]; !slices.Equal(e.Modes, []string{"immediate", "digest", "off"}) {
		t.Errorf("submitted = %+v", e)
	}

	// Bob's own, kept and shown back; a choice equal to the default is not kept.
	v = bob.setPrefs(prefs(7, "Europe/Zurich", "approved=digest", "merged=off", "approval_needed=digest", "submitted=immediate"))
	want[0], want[2], want[5] = "approval_needed=digest", "approved=digest", "merged=off"
	if !slices.Equal(modes(v), want) || v.Body.Digest != (DigestView{7, "Europe/Zurich"}) || !v.Body.Customized {
		t.Errorf("set = %+v", v.Body)
	}
	ctx := context.Background()
	row, err := h.store.GetNotificationPreferences(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if string(row.Modes) != `{"email": {"merged": "off", "approved": "digest", "approval_needed": "digest"}}` || row.DigestHour.Int16 != 7 || row.TimeZone.String != "Europe/Zurich" {
		t.Errorf("stored = %s %v %v", row.Modes, row.DigestHour, row.TimeZone)
	}

	// Alice reads and changes hers only: no field names anyone else.
	var av PreferencesOutput
	alice.do(http.MethodGet, Prefix+"/me/preferences", nil, &av.Body)
	if av.Body.Customized || av.Body.Digest.TimeZone != "UTC" {
		t.Errorf("alice sees bob's preferences: %+v", av.Body)
	}
	alice.setPrefs(prefs(9, "UTC", "stale=off"))
	bob.do(http.MethodGet, Prefix+"/me/preferences", nil, &v.Body)
	if !slices.Equal(modes(v), want) || v.Body.Digest != (DigestView{7, "Europe/Zurich"}) {
		t.Errorf("alice's change reached bob's: %+v", v.Body)
	}
	for name, body := range map[string]map[string]any{
		"a name":           func() map[string]any { b := prefs(8, "UTC"); b["name"] = "bob"; return b }(),
		"a user":           func() map[string]any { b := prefs(8, "UTC"); b["user"] = "bob"; return b }(),
		"a name in event":  {"events": []map[string]string{{"kind": "stale", "channel": "email", "mode": "digest", "name": "bob"}}, "digest": map[string]any{"hour": 8, "timeZone": "UTC"}},
		"a name in digest": {"events": []map[string]string{}, "digest": map[string]any{"hour": 8, "timeZone": "UTC", "name": "bob"}},
	} {
		if status, raw := alice.do(http.MethodPut, Prefix+"/me/preferences", body, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s, want 422: unknown fields are refused", name, status, raw)
		}
	}
	for _, path := range []string{"/users/bob/preferences", "/me/preferences/bob"} {
		if status, _ := alice.do(http.MethodPut, Prefix+path, prefs(8, "UTC"), nil); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
			t.Errorf("PUT %s = %d: no path names another person", path, status)
		}
	}
	bob.do(http.MethodGet, Prefix+"/me/preferences", nil, &v.Body)
	if !slices.Equal(modes(v), want) {
		t.Errorf("bob's preferences changed: %v", modes(v))
	}

	// Nobody signed in, and API tokens, even with every scope: neither read nor change.
	if status, _ := h.as("").do(http.MethodGet, Prefix+"/me/preferences", nil, nil); status != http.StatusUnauthorized {
		t.Errorf("anonymous get = %d", status)
	}
	if status, _ := h.as("").do(http.MethodPut, Prefix+"/me/preferences", prefs(8, "UTC"), nil); status != http.StatusUnauthorized && status != http.StatusForbidden {
		t.Errorf("anonymous put = %d", status)
	}
	var tok CreatedTokenOutput
	if status, raw := bob.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "all", "scopes": []string{"catalog:read", "inventory:read", "requests:read", "requests:write"}},
		&tok.Body); status != http.StatusCreated {
		t.Fatalf("token = %d %s", status, raw)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		b, _ := json.Marshal(prefs(8, "UTC"))
		req, _ := http.NewRequest(method, h.srv.URL+Prefix+"/me/preferences", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok.Body.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(raw), "scopes") {
			t.Errorf("%s with a token = %d %s, want 403", method, resp.StatusCode, raw)
		}
	}

	// Every value from a fixed list, or checked.
	for name, tt := range map[string]struct {
		body     map[string]any
		location string
	}{
		"alert":           {prefs(8, "UTC", "alert=off"), "body.events[0].kind"},
		"unknown kind":    {prefs(8, "UTC", "deployed=off"), "body.events[0].kind"},
		"unknown mode":    {prefs(8, "UTC", "merged=weekly"), "body.events[0].mode"},
		"approvals off":   {prefs(8, "UTC", "approval_needed=off"), "body.events[0].mode"},
		"twice":           {prefs(8, "UTC", "merged=off", "merged=digest"), "body.events[1]"},
		"hour":            {prefs(24, "UTC"), "body.digest.hour"},
		"negative hour":   {prefs(-1, "UTC"), "body.digest.hour"},
		"local":           {prefs(8, "Local"), "body.digest.timeZone"},
		"path":            {prefs(8, "../../../etc/passwd"), "body.digest.timeZone"},
		"absolute path":   {prefs(8, "/etc/localtime"), "body.digest.timeZone"},
		"dots":            {prefs(8, "Europe/../Zurich"), "body.digest.timeZone"},
		"unknown zone":    {prefs(8, "Mars/Olympus_Mons"), "body.digest.timeZone"},
		"empty zone":      {prefs(8, ""), "body.digest.timeZone"},
		"no digest":       {map[string]any{"events": []any{}}, "body"},
		"unknown channel": {map[string]any{"events": []map[string]string{{"kind": "merged", "channel": "sms", "mode": "off"}}, "digest": map[string]any{"hour": 8, "timeZone": "UTC"}}, "body.events[0].channel"},
		"teams":           {map[string]any{"events": []map[string]string{{"kind": "merged", "channel": "teams", "mode": "off"}}, "digest": map[string]any{"hour": 8, "timeZone": "UTC"}}, "body.events[0].channel"},
	} {
		var p struct {
			Errors []struct{ Location, Message string }
		}
		status, raw := bob.do(http.MethodPut, Prefix+"/me/preferences", tt.body, nil)
		_ = json.Unmarshal([]byte(raw), &p) // application/problem+json
		if status != http.StatusUnprocessableEntity || !slices.ContainsFunc(p.Errors, func(e struct{ Location, Message string }) bool { return strings.HasPrefix(e.Location, tt.location) }) {
			t.Errorf("%s: %d %s, want 422 at %s", name, status, raw, tt.location)
		}
	}
	bob.do(http.MethodGet, Prefix+"/me/preferences", nil, &v.Body)
	if !slices.Equal(modes(v), want) || v.Body.Digest != (DigestView{7, "Europe/Zurich"}) {
		t.Errorf("a refused change changed bob's preferences: %+v", v.Body)
	}

	// The audit log has each change, pseudonymised; the same preferences again change nothing.
	bob.setPrefs(prefs(7, "Europe/Zurich", "approved=digest", "merged=off", "approval_needed=digest"))
	ev, personal := h.audited("preferences.changed", "bob")
	if len(ev) != 1 {
		t.Fatalf("bob's changes = %d events, want 1: the same preferences again are not a change", len(ev))
	}
	byPerson(t, ev[0], personal, "bob")
	e := ev[0]
	if e.Object.Type != "user" || personal[e.Object.ID] != "bob" || strings.Contains(e.Object.ID, "bob") {
		t.Errorf("object = %+v: the person, pseudonymised", e.Object)
	}
	zone, _ := e.Details["timeZone"].(string)
	if personal[zone] != "Europe/Zurich" || strings.Contains(zone, "Zurich") || e.Details["digestHour"] != float64(7) ||
		!slices.Equal(toStrings(e.Details["choices"]), []string{"email/approval_needed=digest", "email/approved=digest", "email/merged=off"}) {
		t.Errorf("details = %+v", e.Details)
	}
	bob.setPrefs(prefs(8, "UTC"))
	if ev, _ := h.audited("preferences.changed", "bob"); len(ev) != 2 || len(toStrings(ev[1].Details["choices"])) != 0 {
		t.Errorf("back to the defaults = %+v", ev)
	}
	if ev, _ := h.audited("preferences.changed", "alice"); len(ev) != 1 {
		t.Errorf("alice's changes = %d events", len(ev))
	}

	// A preference stored behind the API's back does not turn alerts off, nor approvals everywhere.
	bob.setPrefs(prefs(8, "UTC", "approval_needed=digest", "submitted=digest", "approved=off", "merged=off"))
	if _, err := h.store.Pool.Exec(ctx, `UPDATE notification_preferences SET modes = '{"email": {"alert": "off", "approval_needed": "off"}}' WHERE name = 'bob'`); err != nil {
		t.Fatal(err)
	}
	bob.do(http.MethodGet, Prefix+"/me/preferences", nil, &v.Body)
	if v.Body.Events[0].Mode != "immediate" {
		t.Errorf("approval needed off on every channel shows as %s", v.Body.Events[0].Mode)
	}
	if err := h.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		_, err := h.store.RecordEvent(ctx, tx, db.AddEventParams{Target: "repository:infra-live", Kind: "alert_bypass",
			Data: json.RawMessage(`{"number": 12, "state": "pending_approval", "pullRequest": "7", "commit": "0a1b2c3"}`)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.quiet(t)
	if got := inbox(t, mail, "bob@example.com"); !slices.Equal(got, []string{"[Quai] Alert: a pull request was merged without approval in infra-live"}) {
		t.Errorf("bob's messages = %q: an alert always goes at once", got)
	}
	m := mail.Message(t, mail.To(t, "bob@example.com")[0].ID)
	if strings.Contains(m.Text, "/profile") {
		t.Errorf("an alert offers to choose what one gets, which does not change alerts:\n%s", m.Text)
	}
}

// TestPreferencesVocabulary: the API's enums are the configuration's lists.
func TestPreferencesVocabulary(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[EventPreferenceInput](), reflect.TypeFor[EventPreferenceView]()} {
		for field, want := range map[string][]string{"Kind": config.NotificationKinds, "Channel": config.NotificationChannels, "Mode": config.NotificationModes} {
			f, _ := typ.FieldByName(field)
			if got := f.Tag.Get("enum"); got != strings.Join(want, ",") {
				t.Errorf("%s.%s enum = %q, want %q", typ.Name(), field, got, strings.Join(want, ","))
			}
		}
	}
}

// digests returns the digests, oldest first.
func (h *harness) digests(t *testing.T) []db.NotificationDigest {
	t.Helper()
	rows, err := h.store.Pool.Query(context.Background(), `SELECT * FROM notification_digests ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[db.NotificationDigest])
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// gather runs the digest job at a moment of the notifications' clock, and waits for what it sends.
func (h *harness) gather(t *testing.T, at string) {
	t.Helper()
	when, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	h.clock.set(when)
	if _, err := h.jobs.Insert(context.Background(), notify.DigestArgs{}, nil); err != nil {
		t.Fatal(err)
	}
	h.quiet(t)
}

// TestDigest: notifications people want in their digest wait for it, while the others go at once;
// the digest job, on a test clock, gathers a day's notifications into one message at the hour
// each person chose, in their time zone, across a change to daylight saving time. Each
// notification is told once, in one digest, and each digest sent once, however often the jobs
// run. A digest tells only what its recipient may still read: an approval the request no longer
// waits for is left out, and a person deactivated since gets nothing.
func TestDigest(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := notifiedHarness(t, mail, "{ name: infra-live, title: Infrastructure }")
	h.clock.set(time.Date(2026, 3, 27, 9, 0, 0, 0, time.UTC)) // Friday, 10:00 in Zurich
	bob, alice, pat, fiona := h.as("bob"), h.as("alice"), h.as("pat"), h.as("fiona")
	h.as("carol")
	bob.setPrefs(prefs(8, "Europe/Zurich", "submitted=digest", "approved=digest", "merged=digest", "applied=digest"))
	pat.setPrefs(prefs(8, "Europe/Zurich", "approval_needed=digest"))
	fiona.setPrefs(prefs(8, "Europe/Zurich", "submitted=digest"))

	submit := func(u *user, name string, cpu int) RequestView {
		t.Helper()
		var r RequestView
		if status, out := u.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", name, cpu), &r); status != http.StatusCreated || r.State != "pending_approval" {
			t.Fatalf("submit %s = %d %s", name, status, out)
		}
		return r
	}
	a, b := submit(bob, "big01", 16), submit(bob, "web02", 2) // a waits for platform-admins and finops, b for platform-admins
	c := submit(fiona, "web03", 2)
	h.quiet(t)
	title := func(r RequestView, name string) string {
		return fmt.Sprintf("#%d Create infra-live/vm/prod/%s", r.Number, name)
	}
	want := map[string][]string{
		"alice@example.com": {"[Quai] Approval needed: " + title(a, "big01"), "[Quai] Approval needed: " + title(b, "web02"), "[Quai] Approval needed: " + title(c, "web03")},
		"fiona@example.com": {"[Quai] Approval needed: " + title(a, "big01")},
	}
	check := func(when string) {
		t.Helper()
		total := 0
		for _, address := range []string{"alice@example.com", "pat@example.com", "bob@example.com", "fiona@example.com"} {
			subjects := slices.Sorted(slices.Values(want[address]))
			total += len(subjects)
			if got := inbox(t, mail, address); !slices.Equal(got, subjects) {
				t.Errorf("%s: %s's messages = %q, want %q", when, address, got, subjects)
			}
		}
		if n := len(mail.Messages(t)); n != total {
			t.Errorf("%s: %d messages, want %d", when, n, total)
		}
	}
	check("submitted: only what goes at once")

	alice.approve(a.ID)
	h.quiet(t)
	fiona.approve(a.ID)
	bob.await(a.ID, "applied")
	h.quiet(t)
	check("a approved: bob's news wait for his digest")
	// Fiona leaves before her digest.
	ctx := context.Background()
	if _, err := h.store.Pool.Exec(ctx, "UPDATE users SET deactivated_at = now() WHERE name = 'fiona'"); err != nil {
		t.Fatal(err)
	}

	// 07:59 in Zurich: the 27th's digest hour was before the news; none is due.
	h.gather(t, "2026-03-28T06:59:00Z")
	if d := h.digests(t); len(d) != 0 {
		t.Errorf("digests before their hour: %+v", d)
	}
	check("before the hour")

	// 08:00 in Zurich: one digest each, of everything until then.
	h.gather(t, "2026-03-28T07:00:00Z")
	want["bob@example.com"] = []string{"[Quai] Your digest of Saturday 28 March 2026: 4 updates"}
	want["pat@example.com"] = []string{"[Quai] Your digest of Saturday 28 March 2026: 2 updates"}
	check("at the hour")
	m := mail.Message(t, mail.To(t, "bob@example.com")[0].ID)
	num := fmt.Sprint(a.Number)
	for _, s := range []string{"Your request #" + num + " was submitted", "Your request #" + num + " was approved", "Your request #" + num + " was committed",
		title(b, "web02"), "https://quai.example.com/requests/" + a.ID, "https://quai.example.com/profile"} {
		if !strings.Contains(m.Text, s) || !strings.Contains(m.HTML, s) {
			t.Errorf("bob's digest lacks %q:\n%s\n%s", s, m.Text, m.HTML)
		}
	}
	for _, s := range []string{"Submitted: Your request #" + num, "Approved: Your request #" + num, "Merged: Your request #" + num} {
		if !strings.Contains(m.Text, s) {
			t.Errorf("bob's digest lacks %q:\n%s", s, m.Text)
		}
	}
	m = mail.Message(t, mail.To(t, "pat@example.com")[0].ID)
	if !strings.Contains(m.Text, b.ID) || !strings.Contains(m.Text, c.ID) || strings.Contains(m.Text, a.ID) || strings.Contains(m.Text, "big01") {
		t.Errorf("pat's digest tells of b and c, not a, which no longer waits for them:\n%s", m.Text)
	}
	digests := h.digests(t)
	states := map[string]string{}
	for _, d := range digests {
		states[d.Recipient] = d.State
		if d.Period.Time.Format(time.DateOnly) != "2026-03-28" || !d.Due.Time.Equal(time.Date(2026, 3, 28, 7, 0, 0, 0, time.UTC)) {
			t.Errorf("digest %+v: due at 08:00 in Zurich on the 28th", d)
		}
	}
	if len(digests) != 3 || states["user:bob"] != notify.Sent || states["user:pat"] != notify.Sent || states["user:fiona"] != notify.Skipped {
		t.Errorf("digests = %+v", digests)
	}
	for _, n := range h.notifications(t, c.ID) {
		if n.Recipient == "user:fiona" && (n.State != notify.Skipped || n.LastError != "they are no longer in the directory, or were deactivated") {
			t.Errorf("fiona's notification of c = %+v: she left", n)
		}
	}
	for _, n := range h.notifications(t, a.ID) {
		if n.Recipient == "user:pat" && (n.State != notify.Skipped || n.LastError != "the request no longer waits for approval" || !n.DigestID.Valid) {
			t.Errorf("pat's notification of a = %+v: left out of the digest", n)
		}
		if n.Recipient == "user:bob" && (n.State != notify.Sent || !n.DigestID.Valid || n.Attempts != 1) {
			t.Errorf("bob's notification of a = %+v: sent in the digest", n)
		}
	}

	// Again: the jobs make and send nothing more, and a digested notification never goes alone.
	h.gather(t, "2026-03-28T07:00:00Z")
	h.gather(t, "2026-03-28T09:30:00Z")
	for _, d := range digests {
		if _, err := h.jobs.Insert(ctx, notify.SendDigestArgs{DigestID: d.ID}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []RequestView{a, b, c} {
		for _, n := range h.notifications(t, r.ID) {
			if _, err := h.jobs.Insert(ctx, notify.SendArgs{NotificationID: n.ID}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	h.quiet(t)
	check("again")
	if d := h.digests(t); len(d) != 3 {
		t.Errorf("digests after the jobs ran again = %d, want 3", len(d))
	}
	_, err := h.store.AddDigest(ctx, db.AddDigestParams{Recipient: "user:bob", Channel: "email", Period: pgtype.Date{Time: time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC), Valid: true},
		Due: pgtype.Timestamptz{Time: time.Now(), Valid: true}, CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true}})
	if err != pgx.ErrNoRows {
		t.Errorf("a second digest of bob's for the 28th = %v, want none", err)
	}

	// b approved later that day: in the next digest, at 08:00 in Zurich on the 29th, now summer
	// time: 23 hours after the last.
	h.clock.set(time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC))
	alice.approve(b.ID)
	bob.await(b.ID, "applied")
	h.quiet(t)
	check("b approved")
	h.gather(t, "2026-03-29T05:59:00Z") // 07:59 CEST
	check("before the hour, in summer time")
	h.gather(t, "2026-03-29T06:00:00Z") // 08:00 CEST
	want["bob@example.com"] = append(want["bob@example.com"], "[Quai] Your digest of Sunday 29 March 2026: 2 updates")
	check("at the hour, in summer time")
	digests = h.digests(t)
	if last := digests[len(digests)-1]; len(digests) != 4 || last.Recipient != "user:bob" || last.Period.Time.Format(time.DateOnly) != "2026-03-29" ||
		!last.Due.Time.Equal(time.Date(2026, 3, 29, 6, 0, 0, 0, time.UTC)) {
		t.Errorf("digests = %+v", digests)
	}
}
