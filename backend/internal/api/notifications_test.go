package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/config"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/notify"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// withMail configures notifications by email to a Mailpit, and dev users with verified emails:
// alice and pat (platform-admins), bob (shop), fiona (finops); carol has none.
func withMail(srv *mailtest.Server, alerts ...string) func(*config.Config) {
	return func(c *config.Config) {
		c.Network.CAFile = srv.CAFile
		c.Notifications = config.Notifications{Alerts: alerts, Email: &config.Email{Host: srv.Host, Port: srv.Port,
			Transport: config.EmailSTARTTLS, From: "Quai <quai@example.com>"}}
		c.Auth.Dev.Users = []config.DevUser{
			{Name: "alice", Email: "alice@example.com", Groups: []string{"platform-admins"}},
			{Name: "pat", Email: "pat@example.com", Groups: []string{"platform-admins"}},
			{Name: "bob", Email: "bob@example.com", Groups: []string{"developers", "shop"}},
			{Name: "fiona", Email: "fiona@example.com", Groups: []string{"finops", "developers"}},
			{Name: "carol", Groups: []string{"shop", "developers"}},
		}
	}
}

// notifiedHarness is newHarness with notifications to mail, and the repository's metadata.
func notifiedHarness(t *testing.T, mail *mailtest.Server, metadata string, alerts ...string) *harness {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := startRepo(t, dir, metadata, `connector: generic
  url: `+bare+`
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { minInterval: 1ns }`, repositories.Options{}, withMail(mail, alerts...))
	h.bare, h.work = bare, work
	return h
}

// quiet waits until the notifications have nothing left to do: no job on their queue, no
// notification pending.
func (h *harness) quiet(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var jobs, pending int
		if err := h.store.Pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM river_job WHERE queue = 'notifications' AND state NOT IN ('completed', 'discarded', 'cancelled')),
			(SELECT count(*) FROM notifications WHERE state = 'pending')`).Scan(&jobs, &pending); err != nil {
			t.Fatal(err)
		}
		if jobs == 0 && pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications still busy: %d jobs, %d pending\nlog:\n%s", jobs, pending, h.log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// notifications returns the notifications of the events of a request, or of a repository.
func (h *harness) notifications(t *testing.T, target string) []db.Notification {
	t.Helper()
	rows, err := h.store.Pool.Query(context.Background(), `SELECT n.* FROM notifications n JOIN events e ON e.id = n.event_id
		WHERE e.target = $1 OR e.request_id::text = $1 ORDER BY n.id`, target)
	if err != nil {
		t.Fatal(err)
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByName[db.Notification])
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// inbox returns the subjects of the messages to an address, sorted.
func inbox(t *testing.T, mail *mailtest.Server, address string) []string {
	t.Helper()
	var out []string
	for _, m := range mail.To(t, address) {
		out = append(out, m.Subject)
	}
	sort.Strings(out)
	return out
}

// TestNotifications: each event of a request reaches its recipients once: the requester hears of
// its submission, approval and merge; the approvers it waits for, every known member of the
// groups its rules name, hear that it needs their approval. Running the jobs again sends nothing
// more.
func TestNotifications(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := notifiedHarness(t, mail, "{ name: infra-live, title: Infrastructure }")
	bob, alice, fiona := h.as("bob"), h.as("alice"), h.as("fiona")
	h.as("pat")
	h.as("carol")
	body := newVM("prod", "big01", 16)
	body["reason"] = "<script>alert(1)</script> for the <b>shop</b>"
	var r RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", body, &r); status != http.StatusCreated || r.State != "pending_approval" {
		t.Fatalf("submit = %d %s", status, out)
	}
	h.quiet(t)
	title := fmt.Sprintf("#%d Create infra-live/vm/prod/big01", r.Number)
	want := map[string][]string{
		"bob@example.com":   {"[Quai] Submitted: " + title},
		"alice@example.com": {"[Quai] Approval needed: " + title},
		"pat@example.com":   {"[Quai] Approval needed: " + title},
		"fiona@example.com": {"[Quai] Approval needed: " + title},
	}
	check := func(when string) {
		t.Helper()
		total := 0
		for address, subjects := range want {
			total += len(subjects)
			sort.Strings(subjects)
			if got := inbox(t, mail, address); !slices.Equal(got, subjects) {
				t.Errorf("%s: %s's messages = %q, want %q", when, address, got, subjects)
			}
		}
		if n := len(mail.Messages(t)); n != total {
			t.Errorf("%s: %d messages, want %d", when, n, total)
		}
	}
	check("submitted")

	// The message an approver gets: what the request page shows, escaped, and the link to it.
	m := mail.Message(t, mail.To(t, "fiona@example.com")[0].ID)
	link := "https://quai.example.com/requests/" + r.ID
	for _, part := range []string{m.Text, m.HTML} {
		for _, s := range []string{"bob asks for your approval of request #", "envs/prod/vms/big01.yaml", link, "type/large-vm (group:finops)"} {
			if !strings.Contains(part, s) {
				t.Errorf("the approver's message lacks %q:\n%s", s, part)
			}
		}
	}
	if !strings.Contains(m.HTML, "&lt;script&gt;alert(1)&lt;/script&gt; for the &lt;b&gt;shop&lt;/b&gt;") || strings.Contains(m.HTML, "<script>") {
		t.Errorf("the reason is not escaped in the HTML part:\n%s", m.HTML)
	}

	alice.approve(r.ID)
	h.quiet(t)
	check("one rule satisfied") // a review alone is not notified
	fiona.approve(r.ID)
	bob.await(r.ID, "applied")
	h.quiet(t)
	want["bob@example.com"] = append(want["bob@example.com"], "[Quai] Approved: "+title, "[Quai] Merged: "+title)
	check("applied")

	// Once per event, recipient and channel: the fan-outs and the sends again send nothing.
	ctx := context.Background()
	events, err := h.store.ListRequestEvents(ctx, pgtype.UUID{Bytes: uuid.MustParse(r.ID), Valid: true})
	if err != nil {
		t.Fatal(err)
	}
	before := h.notifications(t, r.ID)
	if len(before) != 6 {
		t.Fatalf("notifications = %+v", before)
	}
	for _, e := range events {
		if _, err := h.jobs.Insert(ctx, notify.EventArgs{EventID: e.ID}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range before {
		if n.State != notify.Sent || n.Attempts != 1 || !n.SentAt.Valid {
			t.Errorf("notification %+v", n)
		}
		if _, err := h.jobs.Insert(ctx, notify.SendArgs{NotificationID: n.ID}, nil); err != nil {
			t.Fatal(err)
		}
	}
	h.quiet(t)
	check("again")
	if after := h.notifications(t, r.ID); len(after) != len(before) {
		t.Errorf("notifications after the jobs ran again = %d, want %d", len(after), len(before))
	}
}

// TestNotificationsUnreached: whom notifications do not reach is said on the request: a rule
// whose approvers never signed in or have no verified email, a requester without one; nothing is
// sent to them, and their notifications say why.
func TestNotificationsUnreached(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := notifiedHarness(t, mail, "{ name: infra-live, title: Infrastructure }")
	bob, carol := h.as("bob"), h.as("carol")
	var r RequestView
	bob.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "web07", 16), &r)
	r = bob.read(r.ID)
	if r.RequesterNotNotified || len(r.Rules) != 2 || !r.Rules[0].NoOneToNotify || !r.Rules[1].NoOneToNotify {
		t.Errorf("before the approvers signed in: requester %v, rules %+v", r.RequesterNotNotified, r.Rules)
	}
	var c RequestView
	carol.do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "web08", 16), &c)
	if c = carol.read(c.ID); !c.RequesterNotNotified {
		t.Errorf("carol has no email: %+v", c)
	}
	h.quiet(t)
	// Recipients are resolved when an event is notified: alice, signing in later, sees the
	// request in her inbox, and is told of its next events.
	h.as("alice")
	if r = bob.read(r.ID); r.Rules[0].NoOneToNotify || !r.Rules[1].NoOneToNotify {
		t.Errorf("after alice signed in: %+v", r.Rules)
	}
	if got := inbox(t, mail, "bob@example.com"); len(got) != 1 || len(mail.Messages(t)) != 1 {
		t.Errorf("messages = %+v", mail.Messages(t))
	}
	list := h.notifications(t, c.ID)
	if len(list) != 1 || list[0].Recipient != "user:carol" || list[0].State != notify.Skipped || !strings.Contains(list[0].LastError, "verified email") {
		t.Errorf("carol's notifications = %+v", list)
	}
	// Not configured, nothing is said.
	plain := newHarness(t)
	var p RequestView
	plain.as("carol").do(http.MethodPost, Prefix+"/requests/type", newVM("prod", "web09", 16), &p)
	if p = plain.as("carol").read(p.ID); p.RequesterNotNotified || p.Rules[0].NoOneToNotify {
		t.Errorf("without notifications: %+v", p)
	}
}

// TestNotificationsRetried: a mail server that refuses messages holds no request back; the
// notifications are tried again, and sent once it takes them.
func TestNotificationsRetried(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := notifiedHarness(t, mail, "{ name: infra-live, title: Infrastructure }")
	mail.Fail(t)
	bob := h.as("bob")
	var r RequestView
	if status, out := bob.do(http.MethodPost, Prefix+"/requests/type", newVM("dev", "tmp01", 2), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, out)
	}
	bob.await(r.ID, "applied")
	deadline := time.Now().Add(60 * time.Second)
	for {
		list := h.notifications(t, r.ID)
		retried := len(list) == 2
		for _, n := range list {
			retried = retried && n.State == notify.Pending && n.Attempts >= 2 && strings.Contains(n.LastError, "451")
		}
		if retried {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifications = %+v\nlog:\n%s", list, h.log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := len(mail.Messages(t)); n != 0 {
		t.Fatalf("%d messages while the server refuses them", n)
	}
	mail.Recover(t)
	h.quiet(t)
	title := fmt.Sprintf("#%d Create infra-live/vm/dev/tmp01", r.Number)
	if got := inbox(t, mail, "bob@example.com"); !slices.Equal(got, []string{"[Quai] Approved: " + title, "[Quai] Merged: " + title}) {
		t.Errorf("bob's messages = %q", got)
	}
	for _, n := range h.notifications(t, r.ID) {
		if n.State != notify.Sent || n.LastError != "" {
			t.Errorf("notification %+v", n)
		}
	}
	if !strings.Contains(h.log.String(), "a notification was not sent") {
		t.Error("the failed attempts are not logged")
	}
}

// TestAlertNotifications: an alert reaches the repository's administrators (metadata.owners, as
// people of the directory) and notifications.alerts, each address once, with no request content.
func TestAlertNotifications(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := notifiedHarness(t, mail, "{ name: infra-live, title: Infrastructure, owners: [group:platform-admins, user:bob] }",
		"security@example.com", "Alice@example.com")
	for _, name := range []string{"alice", "pat", "bob", "fiona", "carol"} {
		h.as(name)
	}
	ctx := context.Background()
	if err := h.store.Tx(ctx, func(tx pgx.Tx, _ *db.Queries) error {
		_, err := h.store.RecordEvent(ctx, tx, db.AddEventParams{Target: "repository:infra-live", Kind: "alert_bypass",
			Data: json.RawMessage(`{"number": 12, "state": "pending_approval", "pullRequest": "7", "commit": "0a1b2c3"}`)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h.quiet(t)
	subject := "[Quai] Alert: a pull request was merged without approval in infra-live"
	for _, address := range []string{"alice@example.com", "pat@example.com", "bob@example.com", "security@example.com"} {
		if got := inbox(t, mail, address); !slices.Equal(got, []string{subject}) {
			t.Errorf("%s's messages = %q", address, got)
		}
	}
	if n := len(mail.Messages(t)); n != 4 {
		t.Errorf("%d messages, want 4", n)
	}
	m := mail.Message(t, mail.To(t, "security@example.com")[0].ID)
	for _, s := range []string{"merged without Quai", "0a1b2c3", "https://quai.example.com/admin/repositories/infra-live"} {
		if !strings.Contains(m.Text, s) || !strings.Contains(m.HTML, s) {
			t.Errorf("the alert lacks %q:\n%s", s, m.Text)
		}
	}
	list := h.notifications(t, "repository:infra-live")
	var recipients []string
	for _, n := range list {
		recipients = append(recipients, n.Recipient)
	}
	sort.Strings(recipients)
	if want := []string{"email:security@example.com", "user:alice", "user:bob", "user:pat"}; !slices.Equal(recipients, want) {
		t.Errorf("recipients = %v, want %v", recipients, want)
	}
}

// TestEnforcementAlertNotified: the alert of approvals no longer enforced, as Quai finds it,
// reaches notifications.alerts.
func TestEnforcementAlertNotified(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h, remote := giteaHarnessOver(t, "manual", "", nil, nil, giteaconn.New, withMail(mail, "security@example.com"))
	if err := remote.Unprotect("main"); err != nil {
		t.Fatal(err)
	}
	if status, body := h.as("bob").do(http.MethodPost, Prefix+"/plans/type", planBody(6), nil); status != http.StatusConflict {
		t.Fatalf("plan = %d %s", status, body)
	}
	h.quiet(t)
	if got := inbox(t, mail, "security@example.com"); !slices.Equal(got, []string{"[Quai] Alert: approvals are not enforced on infra-live"}) {
		t.Errorf("messages = %q", got)
	}
	m := mail.Message(t, mail.To(t, "security@example.com")[0].ID)
	if !strings.Contains(m.Text, "branch main is not protected") {
		t.Errorf("the alert lacks the problem:\n%s", m.Text)
	}
}

// TestPlanNotified: a plan that makes a rule apply asks its approvers; the message shows the
// plan as the request page does, its resources by address, never a value.
func TestPlanNotified(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h, _ := giteaHarnessOver(t, "manual", runner, costRule, nil, giteaconn.New, withMail(mail))
	bob, alice := h.as("bob"), h.as("alice")
	h.as("fiona")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resize("prod/web01", 6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery != nil && v.Delivery.PullRequest != "" })
	h.quiet(t)
	if got := inbox(t, mail, "alice@example.com"); len(got) != 1 {
		t.Errorf("alice's messages = %q", got)
	}
	alice.approve(r.ID)
	// The cost rule waits for the plan: FinOps are not asked yet.
	if got := inbox(t, mail, "fiona@example.com"); len(got) != 0 {
		t.Errorf("fiona's messages before the plan = %q", got)
	}
	h.planOf(r, "https://ci.example.com/1", 900)
	h.quiet(t)
	title := fmt.Sprintf("#%d resize on infra-live/vm/prod/web01", r.Number)
	got := mail.To(t, "fiona@example.com")
	if len(got) != 1 || got[0].Subject != "[Quai] Approval needed: "+title {
		t.Fatalf("fiona's messages = %+v", got)
	}
	m := mail.Message(t, got[0].ID)
	for _, s := range []string{"its plan makes a rule apply", "+900.00 CHF a month", `update module.vm["web01"].terraform_data.vm`,
		"2 to add, 1 to change, 2 to destroy"} {
		if !strings.Contains(m.Text, s) {
			t.Errorf("the message lacks %q:\n%s", s, m.Text)
		}
	}
	if strings.Contains(m.Text, "10.0.12.11") || strings.Contains(m.HTML, "10.0.12.11") || strings.Contains(m.Text, "ubuntu-24.04") {
		t.Errorf("the message holds a value of the plan:\n%s", m.Text)
	}
}
