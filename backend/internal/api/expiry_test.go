package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/damhau/quai/backend/internal/config"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/notify"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/requests"
)

// withApprovals sets the reminders and the escalation (approvals in Quai's configuration).
func withApprovals(remind, escalate time.Duration, to ...string) func(*config.Config) {
	return func(c *config.Config) {
		c.Approvals = config.Approvals{Reminders: config.Reminders{After: remind}, Escalation: config.Escalation{After: escalate, To: to}}
	}
}

// withDana adds dana, an auditor with a verified email who approves nothing, to the dev users.
func withDana(c *config.Config) {
	c.Auth.Dev.Users = append(c.Auth.Dev.Users, config.DevUser{Name: "dana", Email: "dana@example.com", Groups: []string{"auditors"}})
}

// withExpiry gives the vm type an approval expiry.
func withExpiry(expiry string) func(path string, b []byte) []byte {
	return func(path string, b []byte) []byte {
		if path != ".quai/types/vm.yaml" {
			return b
		}
		return bytes.Replace(b, []byte("  approval:\n    rules:"), []byte("  approval:\n    expiry: "+expiry+"\n    rules:"), 1)
	}
}

// expiryHarness serves the example repository delivered by direct commits, its floor rule asking
// platform-admins for production, with notifications to mail when it is not nil, the vm type's
// approval expiry set to expiry ("" for none), and Quai's configuration changed by configure.
func expiryHarness(t *testing.T, mail *mailtest.Server, expiry string, configure ...func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	if mail != nil {
		configure = append([]func(*config.Config){withMail(mail)}, configure...)
	}
	h := startRepo(t, dir, "{ name: infra-live, title: Infrastructure }", `connector: generic
  url: `+bare+`
  enforcement:
    floorRules: [{ name: production-floor, when: "params.env == 'prod'", approvers: [group:platform-admins] }]
  sync: { minInterval: 1ns }`, repositories.Options{}, configure...)
	h.bare, h.work = bare, work
	if expiry != "" {
		h.setExpiry(expiry)
	}
	return h
}

// setExpiry pushes the vm type with an approval expiry, as someone would.
func (h *harness) setExpiry(expiry string) {
	h.t.Helper()
	vm, ok := h.file(".quai/types/vm.yaml")
	if !ok {
		h.t.Fatal("no vm type")
	}
	if i := strings.Index(vm, "    expiry: "); i >= 0 {
		end := i + strings.Index(vm[i:], "\n")
		vm = vm[:i] + vm[end+1:]
	}
	h.push(".quai/types/vm.yaml", string(withExpiry(expiry)(".quai/types/vm.yaml", []byte(vm))))
}

// sweep runs the approvals' sweep with the clock at at, as its periodic job would.
func (h *harness) sweep(at time.Time) {
	h.t.Helper()
	h.clock.set(at)
	if err := h.requests.Sweep(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// eventKinds returns the kinds of a request's events, oldest first.
func (h *harness) eventKinds(id string) []string {
	h.t.Helper()
	events, err := h.store.ListRequestEvents(context.Background(), pgtype.UUID{Bytes: uuid.MustParse(id), Valid: true})
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}

// count returns how often kind is among kinds.
func count(kinds []string, kind string) int {
	n := 0
	for _, k := range kinds {
		if k == kind {
			n++
		}
	}
	return n
}

// TestApprovalReminders: with approvals.reminders.after 24h and approvals.escalation.after 48h,
// a request still pending approval reminds the people it waits for after a day (a delegate in
// their delegator's place, on their behalf; with their preferences, as an approval needed),
// never its requester; after two days, the escalation contacts hear of it, each with what they
// may read: the request's content to an approver, its identifiers to the others, nothing to the
// requester. Its requester hears a day before it expires. Each step once, whatever the sweeps.
func TestApprovalReminders(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := expiryHarness(t, mail, "3d", withApprovals(24*time.Hour, 48*time.Hour, "group:auditors", "user:pat", "user:bob"), withDana)
	bob, alice, pat := h.as("bob"), h.as("alice"), h.as("pat")
	h.as("fiona")
	h.as("dana")
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	alice.mustDelegate("fiona", t0, t0.Add(10*24*time.Hour))
	pat.setPrefs(prefs(8, "UTC", "approval_needed=immediate", "submitted=off"))
	body := newVM("prod", "remind01", 2)
	body["reason"] = "the secret reason of the remind01 request"
	r := bob.submit(body)
	if r.Expiry != "3d" || r.ExpiresAt == nil || !r.ExpiresAt.Equal(t0.Add(72*time.Hour)) {
		t.Fatalf("expiry %q at %v, want 3d at %v", r.Expiry, r.ExpiresAt, t0.Add(72*time.Hour))
	}
	h.quiet(t)
	title := fmt.Sprintf("#%d Create infra-live/vm/prod/remind01", r.Number)
	want := map[string][]string{
		"bob@example.com":   {"[Quai] Submitted: " + title},
		"pat@example.com":   {"[Quai] Approval needed: " + title},
		"fiona@example.com": {"[Quai] Approval needed: " + title},
	}
	check := func(when string) {
		t.Helper()
		total := 0
		for address, subjects := range want {
			total += len(subjects)
			slices.Sort(subjects)
			if got := inbox(t, mail, address); !slices.Equal(got, subjects) {
				t.Errorf("%s: %s's messages = %q, want %q", when, address, got, subjects)
			}
		}
		if n := len(mail.Messages(t)); n != total {
			t.Errorf("%s: %d messages, want %d", when, n, total)
		}
	}
	check("submitted")

	h.sweep(t0.Add(23 * time.Hour))
	h.quiet(t)
	check("before the reminder")

	h.sweep(t0.Add(24 * time.Hour))
	h.sweep(t0.Add(25 * time.Hour)) // a second sweep reminds no one again
	h.quiet(t)
	want["pat@example.com"] = append(want["pat@example.com"], "[Quai] Reminder: "+title)
	want["fiona@example.com"] = append(want["fiona@example.com"], "[Quai] Reminder: "+title)
	check("reminded")
	for _, m := range mail.To(t, "fiona@example.com") {
		if strings.HasPrefix(m.Subject, "[Quai] Reminder") {
			text := mail.Message(t, m.ID).Text
			if !strings.Contains(text, "Reminder: bob's request #") || !strings.Contains(text, "24h0m0s after it was submitted") ||
				!strings.Contains(text, "You hold alice's approvals through a delegation") {
				t.Errorf("fiona's reminder:\n%s", text)
			}
		}
	}
	if got := count(h.eventKinds(r.ID), "reminded"); got != 1 {
		t.Errorf("%d reminded events, want 1", got)
	}
	ev, personal := h.audited("request.reminded", r.ID)
	approvers := []string{}
	for _, a := range ev[0].Details["approvers"].([]any) {
		p := a.(map[string]any)
		name := personal[p["approver"].(string)]
		if o, ok := p["onBehalfOf"].(string); ok {
			name += " for " + personal[o]
		}
		approvers = append(approvers, name)
	}
	slices.Sort(approvers)
	if len(ev) != 1 || !slices.Equal(approvers, []string{"fiona for alice", "pat"}) || ev[0].Actor.Type != "system" {
		t.Errorf("request.reminded = %+v (%q)", ev, approvers)
	}

	h.sweep(t0.Add(36 * time.Hour))
	h.quiet(t)
	check("before the escalation")

	// The escalation and the warning a day before the expiry, both due at 48h.
	h.sweep(t0.Add(48 * time.Hour))
	h.sweep(t0.Add(49 * time.Hour))
	h.quiet(t)
	want["pat@example.com"] = append(want["pat@example.com"], "[Quai] Escalation: "+title)
	want["dana@example.com"] = []string{fmt.Sprintf("[Quai] Escalation: #%d", r.Number)}
	want["bob@example.com"] = append(want["bob@example.com"], "[Quai] Expiring soon: "+title)
	check("escalated")
	if m := mail.Message(t, mail.To(t, "dana@example.com")[0].ID); strings.Contains(m.Text, "remind01") || strings.Contains(m.Text, "secret reason") ||
		strings.Contains(m.HTML, "remind01") || !strings.Contains(m.Text, "still waits for approval, 48h0m0s after it was submitted") || !strings.Contains(m.Text, "/requests/"+r.ID) {
		t.Errorf("dana, who may not read the request, gets its content, or not its link:\n%s", m.Text)
	}
	for _, m := range mail.To(t, "pat@example.com") {
		if strings.HasPrefix(m.Subject, "[Quai] Escalation") {
			if text := mail.Message(t, m.ID).Text; !strings.Contains(text, "the secret reason of the remind01 request") || !strings.Contains(text, "escalation contact") {
				t.Errorf("pat's escalation, an approver's, lacks the content:\n%s", text)
			}
		}
	}
	for _, m := range mail.To(t, "bob@example.com") {
		if strings.HasPrefix(m.Subject, "[Quai] Expiring soon") {
			if text := mail.Message(t, m.ID).Text; !strings.Contains(text, "it expires on "+t0.Add(72*time.Hour).Format("2 January 2006 at 15:04 UTC")) {
				t.Errorf("bob's warning:\n%s", text)
			}
		}
	}
	kinds := h.eventKinds(r.ID)
	if count(kinds, "escalated") != 1 || count(kinds, "expiring_soon") != 1 || count(kinds, "reminded") != 1 {
		t.Errorf("events = %q", kinds)
	}
	ev, personal = h.audited("request.escalated", r.ID)
	if to, _ := json.Marshal(ev[0].Details["to"]); len(ev) != 1 || !strings.Contains(string(to), `"group:auditors"`) || strings.Contains(string(to), `"user:pat"`) {
		t.Errorf("request.escalated = %+v", ev)
	} else if u := ev[0].Details["to"].([]any)[1].(map[string]any)["user"].(string); personal[u] != "pat" {
		t.Errorf("the escalation's user entry is not pseudonymised: %q", u)
	}

	// Approved, it is never escalated nor expired, whatever the clock says.
	pat.approve(r.ID)
	r = bob.await(r.ID, "applied")
	h.sweep(t0.Add(100 * 24 * time.Hour))
	h.quiet(t)
	if kinds := h.eventKinds(r.ID); count(kinds, "expired") != 0 || count(kinds, "reminded") != 1 {
		t.Errorf("events after the approval = %q", kinds)
	}
}

// TestApprovalRemindersDigest: a reminder is an approval needed: it follows its recipient's
// preferences for approval needed, into their digest.
func TestApprovalRemindersDigest(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h := expiryHarness(t, mail, "", withApprovals(time.Hour, 0))
	bob, pat := h.as("bob"), h.as("pat")
	h.as("alice")
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	pat.setPrefs(prefs(8, "UTC", "approval_needed=digest"))
	r := bob.submit(newVM("prod", "remind02", 2))
	h.sweep(t0.Add(time.Hour))
	h.quiet(t)
	var reminders, digested int
	for _, n := range h.notifications(t, r.ID) {
		if n.Kind == notify.ApprovalNeeded && n.Recipient == "user:pat" {
			reminders++
			if n.State == notify.InDigest {
				digested++
			}
		}
	}
	if reminders != 2 || digested != 2 {
		t.Errorf("pat's approvals needed: %d, %d in the digest; want the submission's and the reminder's, both", reminders, digested)
	}
	if got := inbox(t, mail, "pat@example.com"); len(got) != 0 {
		t.Errorf("pat's messages = %q", got)
	}
}

// TestApprovalExpiry: on Gitea, a request pending approval longer than its vm type's expiry
// (2d) expires: its approvals are void and never count again, quai/approval fails ("Expired in
// Quai"), a comment says why on its pull request, once, and Quai closes the pull request and
// deletes its branch; its requester hears of it. A comment already there (an earlier run that
// stopped before recording it) is not added again. A pull request closed by someone else is left
// as it is.
func TestApprovalExpiry(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h, remote := giteaHarnessOver(t, "auto", "", withExpiry("2d"), nil, giteaconn.New, withMail(mail))
	g := giteatest.Get(t)
	bob, alice, fiona := h.as("bob"), h.as("alice"), h.as("fiona")
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)

	// Production and large-vm apply: alice's approval alone leaves it pending.
	r := bob.submit(newVM("prod", "expire01", 12))
	r = bob.awaitDelivery(r.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	number := mustInt64(t, r.Delivery.PullRequest)
	alice.approve(r.ID)
	if r = bob.await(r.ID, "pending_approval"); !r.ExpiresAt.Equal(t0.Add(48 * time.Hour)) {
		t.Fatalf("expires at %v", r.ExpiresAt)
	}
	h.sweep(t0.Add(48*time.Hour - time.Second))
	if r = bob.await(r.ID, "pending_approval"); count(h.eventKinds(r.ID), "expired") != 0 {
		t.Fatal("expired early")
	}

	h.sweep(t0.Add(48 * time.Hour))
	r = bob.await(r.ID, "expired")
	if len(r.Reviews) != 1 || !r.Reviews[0].Voided {
		t.Errorf("the reviews after the expiry: %+v", r.Reviews)
	}
	if status := fiona.tryApprove(r.ID); status != http.StatusConflict {
		t.Errorf("a review of an expired request = %d, want 409", status)
	}
	if status, _ := bob.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/cancel", nil, nil); status != http.StatusConflict {
		t.Errorf("cancelling an expired request = %d, want 409", status)
	}
	h.awaitPullRequest(t, remote, number, func(pr *gitea.PullRequest) bool { return pr.State == gitea.StateClosed && !pr.HasMerged })
	pr, _, err := g.Client.GetPullRequest(remote.Owner, remote.Name, number)
	if err != nil {
		t.Fatal(err)
	}
	h.awaitStatus(t, remote, pr.Head.Sha, gitea.StatusFailure)
	if combined, _, err := g.Client.GetCombinedStatus(remote.Owner, remote.Name, pr.Head.Sha); err != nil || combined.Statuses[0].Description != "Expired in Quai" {
		t.Errorf("quai/approval = %+v, %v", combined, err)
	}
	expiredComments := func(n int64) []string {
		t.Helper()
		comments, _, err := g.Client.ListIssueComments(remote.Owner, remote.Name, n, gitea.ListIssueCommentOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range comments {
			if strings.Contains(c.Body, "Expired in Quai") {
				out = append(out, c.Body)
			}
		}
		return out
	}
	awaitComments := func(n int64, want int) []string {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			got := expiredComments(n)
			if len(got) == want || time.Now().After(deadline) {
				return got
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if got := awaitComments(number, 1); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("request #%d waited for approval longer than its expiry (2d)", r.Number)) ||
		!strings.Contains(got[0], "https://quai.example.com/requests/"+r.ID) || strings.Contains(got[0], "expire01") {
		t.Errorf("the comments: %q", got)
	}
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		if _, resp, _ := g.Client.GetRepoBranch(remote.Owner, remote.Name, r.Delivery.Branch); resp != nil && resp.StatusCode == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the branch was not deleted")
		}
	}

	// Again: the sweep and the status job do nothing more.
	h.sweep(t0.Add(72 * time.Hour))
	if _, err := h.jobs.Insert(context.Background(), requests.StatusArgs{RequestID: uuid.MustParse(r.ID)}, nil); err != nil {
		t.Fatal(err)
	}
	h.awaitJobs(t)
	if got := expiredComments(number); len(got) != 1 {
		t.Errorf("comments after running again: %q", got)
	}
	kinds := h.eventKinds(r.ID)
	for kind, want := range map[string]int{"expired": 1, "expiry_commented": 1, "pull_request_closed": 1, "pull_request_left": 0} {
		if got := count(kinds, kind); got != want {
			t.Errorf("%d %s events, want %d: %q", got, kind, want, kinds)
		}
	}
	h.quiet(t)
	if got := inbox(t, mail, "bob@example.com"); !slices.Contains(got, fmt.Sprintf("[Quai] Expired: #%d Create infra-live/vm/prod/expire01", r.Number)) {
		t.Errorf("bob's messages = %q", got)
	}
	ev, _ := h.audited("request.expired", r.ID)
	if len(ev) != 1 || ev[0].Details["expiry"] != "2d" || ev[0].Details["voidedReviews"] != float64(1) {
		t.Errorf("request.expired = %+v", ev)
	}
	if ev, _ := h.audited("request.pull_request_closed", r.ID); len(ev) != 1 || ev[0].Details["pullRequest"] != r.Delivery.PullRequest {
		t.Errorf("request.pull_request_closed = %+v", ev)
	}

	// Expired is final: asked again, it is a new request with new approvals.
	again := bob.submit(newVM("prod", "expire01", 12))
	if again.ID == r.ID || again.State != "pending_approval" || len(again.Reviews) != 0 {
		t.Errorf("the new request: %+v", again)
	}
	bob.awaitDelivery(again.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })

	// A comment already there, from a run that stopped before recording it, is not added again.
	second := bob.submit(newVM("prod", "expire02", 2))
	second = bob.awaitDelivery(second.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	n2 := mustInt64(t, second.Delivery.PullRequest)
	if _, _, err := g.Client.CreateIssueComment(remote.Owner, remote.Name, n2, gitea.CreateIssueCommentOption{
		Body: "Expired in Quai (posted before a crash).\n\n<!-- quai:expired=" + second.ID + " -->\n"}); err != nil {
		t.Fatal(err)
	}
	// A pull request someone closed is left as it is.
	third := bob.submit(newVM("prod", "expire03", 2))
	third = bob.awaitDelivery(third.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	n3 := mustInt64(t, third.Delivery.PullRequest)
	closed := gitea.StateClosed
	if _, _, err := g.Client.EditPullRequest(remote.Owner, remote.Name, n3, gitea.EditPullRequestOption{State: &closed}); err != nil {
		t.Fatal(err)
	}
	// Submitted at t0+72h, they expire at t0+120h.
	h.sweep(t0.Add(120 * time.Hour))
	bob.await(second.ID, "expired")
	bob.await(third.ID, "expired")
	h.awaitPullRequest(t, remote, n2, func(pr *gitea.PullRequest) bool { return pr.State == gitea.StateClosed })
	h.awaitJobs(t)
	if got := expiredComments(n2); len(got) != 1 {
		t.Errorf("the second pull request's comments: %q", got)
	}
	if got := expiredComments(n3); len(got) != 0 {
		t.Errorf("the closed pull request was commented: %q", got)
	}
	if kinds := h.eventKinds(third.ID); count(kinds, "pull_request_left") != 1 || count(kinds, "pull_request_closed") != 0 {
		t.Errorf("the closed pull request's request: %q", kinds)
	}
}

// awaitJobs waits until the job queue has nothing left to do.
func (h *harness) awaitJobs(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var n int
		if err := h.store.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM river_job WHERE state NOT IN ('completed', 'discarded', 'cancelled')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d jobs still to do\nlog:\n%s", n, h.log.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func mustInt64(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestExpiryRace: a request's expiry and its final approval decided at the same moment: whichever
// commits first wins, and the other sees the new state and does nothing. Expired first, the
// approval is refused (409) and no review is recorded; approved first, the sweep leaves it alone.
// Both at once, waiting for the request's row lock in one order and then the other: the same.
func TestExpiryRace(t *testing.T) {
	t.Parallel()
	h := expiryHarness(t, nil, "1d")
	bob, alice := h.as("bob"), h.as("alice")
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)

	first := bob.submit(newVM("prod", "race01", 2))
	h.sweep(t0.Add(24 * time.Hour))
	bob.await(first.ID, "expired")
	if status := alice.tryApprove(first.ID); status != http.StatusConflict {
		t.Errorf("approving an expired request = %d, want 409", status)
	}
	if r := bob.await(first.ID, "expired"); len(r.Reviews) != 0 {
		t.Errorf("a review was recorded on the expired request: %+v", r.Reviews)
	}

	h.clock.set(t0)
	second := bob.submit(newVM("prod", "race02", 2))
	alice.approve(second.ID)
	bob.await(second.ID, "applied")
	h.sweep(t0.Add(24 * time.Hour))
	if r := bob.await(second.ID, "applied"); count(h.eventKinds(r.ID), "expired") != 0 || len(r.Reviews) != 1 || r.Reviews[0].Voided {
		t.Errorf("the approved request after the sweep: %+v", r)
	}

	// At the same moment: the row is held while one side, then the other, waits for it; let go,
	// the first in line decides and the second sees its decision.
	for i, approveFirst := range []bool{true, false} {
		h.clock.set(t0)
		r := bob.submit(newVM("prod", fmt.Sprintf("race1%d", i), 2))
		ctx := context.Background()
		tx, err := h.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM requests WHERE id = $1 FOR UPDATE`, r.ID); err != nil {
			t.Fatal(err)
		}
		h.clock.set(t0.Add(24 * time.Hour))
		var wg sync.WaitGroup
		var status int
		var sweepErr error
		approve := func() { defer wg.Done(); status = alice.tryApprove(r.ID) }
		sweep := func() { defer wg.Done(); sweepErr = h.requests.Sweep(ctx) }
		first, second := sweep, approve
		if approveFirst {
			first, second = approve, sweep
		}
		wg.Add(2)
		go first()
		h.awaitLockWaiters(t, 1)
		go second()
		h.awaitLockWaiters(t, 2)
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		if sweepErr != nil {
			t.Fatal(sweepErr)
		}
		var v RequestView
		bob.do(http.MethodGet, Prefix+"/requests/"+r.ID, nil, &v)
		kinds := h.eventKinds(r.ID)
		if approveFirst {
			if status != http.StatusOK || count(kinds, "expired") != 0 || len(v.Reviews) != 1 || v.Reviews[0].Voided || v.State == "expired" {
				t.Errorf("approved first: %s, the review answered %d, reviews %+v, events %q", v.State, status, v.Reviews, kinds)
			}
			bob.await(r.ID, "applied")
		} else if v.State != "expired" || status != http.StatusConflict || len(v.Reviews) != 0 || count(kinds, "approved") != 0 {
			t.Errorf("expired first: %s, the review answered %d, reviews %+v, events %q", v.State, status, v.Reviews, kinds)
		}
	}
}

// awaitLockWaiters waits until n sessions of the test's database wait for a lock.
func (h *harness) awaitLockWaiters(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var waiting int
		if err := h.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d sessions wait for a lock, want %d", waiting, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestExpirySnapshot: the expiry a request has is the one its definitions set when it was
// submitted: a later change of its type neither expires a waiting request sooner, nor gives one
// an expiry it did not have.
func TestExpirySnapshot(t *testing.T) {
	t.Parallel()
	h := expiryHarness(t, nil, "")
	bob := h.as("bob")
	h.as("alice")
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	none := bob.submit(newVM("prod", "snap01", 2))
	h.setExpiry("2d")
	two := bob.submit(newVM("prod", "snap02", 2))
	h.setExpiry("1d")
	one := bob.submit(newVM("prod", "snap03", 2))
	if none.ExpiresAt != nil || two.Expiry != "2d" || one.Expiry != "1d" {
		t.Fatalf("expiries: %v, %q, %q", none.ExpiresAt, two.Expiry, one.Expiry)
	}
	h.sweep(t0.Add(24 * time.Hour))
	bob.await(one.ID, "expired")
	bob.await(two.ID, "pending_approval")
	h.sweep(t0.Add(48 * time.Hour))
	bob.await(two.ID, "expired")
	h.sweep(t0.Add(400 * 24 * time.Hour))
	bob.await(none.ID, "pending_approval")
}

// TestExpiringSoonShort: an expiry shorter than two days warns at half its length. The sweep is
// a job of the queue: one inserted runs it, and one more inserted in the same minute is the same
// job.
func TestExpiringSoonShort(t *testing.T) {
	t.Parallel()
	h := expiryHarness(t, nil, "12h")
	bob := h.as("bob")
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	r := bob.submit(newVM("prod", "short01", 2))
	h.sweep(t0.Add(6*time.Hour - time.Second))
	if count(h.eventKinds(r.ID), "expiring_soon") != 0 {
		t.Error("warned before half its expiry")
	}
	h.clock.set(t0.Add(6 * time.Hour))
	ctx := context.Background()
	first, err := h.jobs.Insert(ctx, requests.SweepArgs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.jobs.Insert(ctx, requests.SweepArgs{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !again.UniqueSkippedAsDuplicate || again.Job.ID != first.Job.ID {
		t.Errorf("a second sweep in the same minute: %+v", again)
	}
	h.awaitJobs(t)
	if count(h.eventKinds(r.ID), "expiring_soon") != 1 {
		t.Error("not warned at half its expiry")
	}
	h.sweep(t0.Add(12 * time.Hour))
	bob.await(r.ID, "expired")
}
