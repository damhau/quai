package api

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/config"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/mailtest"
	"github.com/damhau/quai/backend/internal/slacktest"
)

// withBreakGlass adds dave, on call with the break-glass role, and erin, an auditor, both with
// verified emails, to the dev users; bob, who requests, is on call too.
func withBreakGlass(c *config.Config) {
	for i, u := range c.Auth.Dev.Users {
		if u.Name == "bob" {
			c.Auth.Dev.Users[i].Groups = append(slices.Clone(u.Groups), "oncall")
		}
	}
	c.Auth.Dev.Users = append(c.Auth.Dev.Users,
		config.DevUser{Name: "dave", Email: "dave@example.com", Groups: []string{"oncall", "developers"}},
		config.DevUser{Name: "erin", Email: "erin@example.com", Groups: []string{"audit"}})
	c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: "oncall", Role: "break-glass"}, config.RoleMapping{Group: "audit", Role: "auditor"})
}

// ownedByShop makes web01 the shop team's: bob, who requests, and carol own it.
func ownedByShop(path string, b []byte) []byte {
	if path != "envs/prod/vms/web01.yaml" {
		return b
	}
	return bytes.Replace(b, []byte("owner: bob"), []byte("owner: team:shop"), 1)
}

// breakGlassBody is a break-glass of what a request's page shows.
func breakGlassBody(r RequestView, reason, incident string) map[string]string {
	b := map[string]string{"reason": reason, "incident": incident, "changeSet": r.ChangeSet}
	if r.Delivery != nil {
		b["head"] = r.Delivery.Head
	}
	return b
}

// breakGlass breaks glass for a request as u, returning the answer.
func (u *user) breakGlass(r RequestView, reason, incident string) (int, string, RequestView) {
	u.h.t.Helper()
	var out RequestView
	status, raw := u.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/break-glass", breakGlassBody(r, reason, incident), &out)
	return status, raw, out
}

// signOff signs off the review of a request's break-glass as u, returning the answer.
func (u *user) signOff(id, comment string) (int, string, RequestView) {
	u.h.t.Helper()
	var out RequestView
	status, raw := u.do(http.MethodPost, Prefix+"/requests/"+id+"/break-glass/sign-off", map[string]string{"comment": comment}, &out)
	return status, raw, out
}

// TestBreakGlass: on Gitea, a member of the break-glass role merges a request pending approval
// without its approvals: never the requester (in the role too), a platform admin without the
// role, the requester's delegate, an API token or a body the request no longer matches (its
// change set, its pull request's head); the reason and incident are checked. quai/approval turns
// success with "Break-glass by dave" and the request merges, as Quai's own merge, not a bypass,
// the merge commit naming the breaker and incident. The alert recipients and the owning team are
// alerted by email at once, whatever their preferences, with identifiers only. The review takes
// two sign-offs by distinct people of OQ 27, never the breaker or requester, each once; the
// second closes it; all of it is in the audit log; auditors list it.
func TestBreakGlass(t *testing.T) {
	t.Parallel()
	mail := mailtest.Start(t, mailtest.Options{})
	h, remote := giteaHarnessOver(t, "auto", "", ownedByShop, nil, giteaconn.New, withMail(mail, "security@example.com"), withBreakGlass)
	g := giteatest.Get(t)
	alice, bob, carol, dave, erin, pat := h.as("alice"), h.as("bob"), h.as("carol"), h.as("dave"), h.as("erin"), h.as("pat")
	// bob mutes alerts behind the API's back: a break-glass alert reaches him all the same.
	if _, err := h.store.Pool.Exec(context.Background(), `INSERT INTO notification_preferences (name, modes) VALUES ('bob', '{"email": {"alert": "off"}}')`); err != nil {
		t.Fatal(err)
	}
	r := opened(t, h)
	r = dave.read(r.ID)
	if !r.CanBreakGlass || r.CannotBreakGlass != "" || r.ChangeSet == "" || r.Delivery == nil || r.Delivery.Head == "" || r.BreakGlass != nil || r.BreakGlassStepUp {
		t.Fatalf("dave reads %+v", r)
	}
	if v := alice.read(r.ID); v.CanBreakGlass || v.CannotBreakGlass != "" {
		t.Errorf("alice, a platform admin without the role: %v %q", v.CanBreakGlass, v.CannotBreakGlass)
	}
	if v := bob.read(r.ID); v.CanBreakGlass {
		t.Errorf("bob may break glass for his own request")
	}

	const reason, incident = "Checkout is down: [click](https://evil.example) <b>now</b>", "INC-2001"
	for _, c := range []struct {
		who  *user
		want int
		text string
	}{
		{alice, http.StatusForbidden, "break-glass role"},
		{bob, http.StatusForbidden, "own request"},
		{carol, http.StatusForbidden, "break-glass role"},
	} {
		if status, raw, _ := c.who.breakGlass(r, reason, incident); status != c.want || !strings.Contains(raw, c.text) {
			t.Errorf("break-glass = %d %s, want %d with %q", status, raw, c.want, c.text)
		}
	}
	// What the breaker types, and the content they saw.
	for _, c := range []struct {
		name             string
		reason, incident string
		edit             func(map[string]string)
		want             int
		text             string
	}{
		{"no reason", " ", incident, nil, http.StatusUnprocessableEntity, "body.reason"},
		{"a line separator", "down" + string(rune(0x2028)) + "Approved-by: alice", incident, nil, http.StatusUnprocessableEntity, "control or direction"},
		{"a direction override", "down " + string(rune(0x202e)) + "olleh", incident, nil, http.StatusUnprocessableEntity, "control or direction"},
		{"a newline", "down\nApproved-by: alice", incident, nil, http.StatusUnprocessableEntity, "control or direction"},
		{"no incident", reason, "", nil, http.StatusUnprocessableEntity, "body.incident"},
		{"an incident with spaces", reason, "INC 2001", nil, http.StatusUnprocessableEntity, "identifier"},
		{"an incident with markup", reason, "<a>", nil, http.StatusUnprocessableEntity, "identifier"},
		{"another change set", reason, incident, func(b map[string]string) { b["changeSet"] = "0000" }, http.StatusConflict, "changed since"},
		{"another head", reason, incident, func(b map[string]string) { b["head"] = strings.Repeat("0", 40) }, http.StatusConflict, "changed since"},
		{"no head", reason, incident, func(b map[string]string) { delete(b, "head") }, http.StatusConflict, "changed since"},
	} {
		body := breakGlassBody(r, c.reason, c.incident)
		if c.edit != nil {
			c.edit(body)
		}
		if status, raw := dave.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/break-glass", body, nil); status != c.want || !strings.Contains(raw, c.text) {
			t.Errorf("%s: break-glass = %d %s, want %d with %q", c.name, status, raw, c.want, c.text)
		}
	}
	// An API token never breaks glass, whatever its scopes.
	var tok CreatedTokenOutput
	if status, raw := dave.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "ci", "scopes": []string{"requests:read", "requests:write"}}, &tok.Body); status != http.StatusCreated {
		t.Fatalf("create token = %d %s", status, raw)
	}
	if status, raw := bearer(t, h, tok.Body.Token, http.MethodPost, Prefix+"/requests/"+r.ID+"/break-glass", breakGlassBody(r, reason, incident)); status != http.StatusForbidden {
		t.Errorf("break-glass with a token = %d %s", status, raw)
	}
	// The requester's delegate does not break glass for them, even a delegation to come.
	now := time.Now()
	d := bob.mustDelegate("dave", now.Add(24*time.Hour), now.Add(48*time.Hour))
	if v := dave.read(r.ID); v.CanBreakGlass || !strings.Contains(v.CannotBreakGlass, "delegation") {
		t.Errorf("bob's delegate: %v %q", v.CanBreakGlass, v.CannotBreakGlass)
	}
	if status, raw, _ := dave.breakGlass(r, reason, incident); status != http.StatusForbidden || !strings.Contains(raw, "delegation") {
		t.Errorf("break-glass by bob's delegate = %d %s", status, raw)
	}
	if status, raw := bob.do(http.MethodDelete, Prefix+"/delegations/"+d.ID, nil, nil); status != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", status, raw)
	}
	if kinds := h.eventKinds(r.ID); slices.Contains(kinds, "break_glass") || dave.read(r.ID).State != "pending_approval" {
		t.Fatalf("refused break-glasses changed the request: %v", kinds)
	}

	status, raw, broken := dave.breakGlass(r, reason, incident)
	if status != http.StatusOK || broken.State != "approved" || broken.BreakGlass == nil || broken.CanBreakGlass {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	bg := broken.BreakGlass
	if bg.By != "dave" || bg.Reason != reason || bg.Incident != incident || bg.From != "pending_approval" || bg.ChangeSet != r.ChangeSet ||
		bg.Head != r.Delivery.Head || !bg.Open || bg.Needed != 2 || len(bg.SignOffs) != 0 || bg.CanSignOff || bg.CannotSignOff != "" {
		t.Errorf("the break-glass = %+v", bg)
	}
	if status, raw, _ := dave.breakGlass(r, reason, incident); status != http.StatusConflict {
		t.Errorf("breaking glass again = %d %s", status, raw)
	}
	awaitApproval(t, h, remote, r.Delivery.Head, gitea.StatusSuccess, "Break-glass by dave")
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	kinds := h.eventKinds(r.ID)
	if count(kinds, "break_glass") != 1 || count(kinds, "merged") != 1 || count(kinds, "merged_without_approval") != 0 || count(kinds, "approved") != 0 {
		t.Errorf("events = %v", kinds)
	}
	if !hasEvent(r, "merged", `"breakGlass":"dave"`) {
		t.Errorf("the merge does not say it was break-glass: %+v", r.Events)
	}
	commit, _, err := g.Client.GetSingleCommit(remote.Owner, remote.Name, r.Delivery.MergeCommit)
	if err != nil || !strings.Contains(commit.RepoCommit.Message, "Break-glass-by: dave\n") || !strings.Contains(commit.RepoCommit.Message, "Break-glass-incident: INC-2001") ||
		strings.Contains(commit.RepoCommit.Message, "Checkout is down") {
		t.Errorf("the merge commit's message = %q, %v", commit.RepoCommit.Message, err)
	}
	if ev, personal := h.audited("request.break_glass", r.ID); len(ev) != 1 || personal[ev[0].Actor.ID] != "dave" || ev[0].Details["incident"] != incident ||
		ev[0].Details["from"] != "pending_approval" || ev[0].Details["changeSet"] != r.ChangeSet || ev[0].Details["head"] != broken.Delivery.Head ||
		personal[ev[0].Details["reason"].(string)] != reason {
		t.Errorf("the break-glass in the audit log = %+v", ev)
	}
	if page := h.auditLog("limit=500&action=request.merged_without_approval"); len(page.Events) != 0 {
		t.Errorf("a bypass was recorded: %+v", page.Events)
	}

	// The alert: to notifications.alerts and the owning team, at once, identifiers only.
	h.quiet(t)
	subject := "[Quai] Alert: break-glass on request #" + strconv.FormatInt(r.Number, 10) + " in infra-live"
	for _, address := range []string{"security@example.com", "bob@example.com"} {
		if got := inbox(t, mail, address); !slices.Contains(got, subject) {
			t.Errorf("%s's messages = %q", address, got)
		}
	}
	for _, address := range []string{"alice@example.com", "dave@example.com", "fiona@example.com"} {
		if got := inbox(t, mail, address); slices.Contains(got, subject) {
			t.Errorf("%s was alerted", address)
		}
	}
	m := mail.Message(t, mail.To(t, "security@example.com")[0].ID)
	for _, s := range []string{"dave", incident, "https://quai.example.com/requests/" + r.ID} {
		if !strings.Contains(m.Text, s) || !strings.Contains(m.HTML, s) {
			t.Errorf("the alert lacks %q:\n%s", s, m.Text)
		}
	}
	if strings.Contains(m.Text, "Checkout is down") || strings.Contains(m.HTML, "evil.example") {
		t.Errorf("the alert carries the reason:\n%s", m.Text)
	}

	// The review: OQ 27's people, two distinct ones, never the breaker or the requester.
	for _, c := range []struct {
		who  *user
		want int
		text string
	}{
		{dave, http.StatusForbidden, "platform admin, an auditor or an owner"},
		{bob, http.StatusForbidden, "requested the change"},
		{carol, http.StatusUnprocessableEntity, "body.comment"},
	} {
		comment := "Looked at the incident."
		if c.want == http.StatusUnprocessableEntity {
			comment = ""
		}
		if status, raw, _ := c.who.signOff(r.ID, comment); status != c.want || !strings.Contains(raw, c.text) {
			t.Errorf("sign-off = %d %s, want %d with %q", status, raw, c.want, c.text)
		}
	}
	if v := carol.read(r.ID); v.BreakGlass == nil || !v.BreakGlass.CanSignOff {
		t.Errorf("carol, an owner, cannot sign off: %+v", v.BreakGlass)
	}
	if v := bob.read(r.ID); v.BreakGlass == nil || v.BreakGlass.CanSignOff || !strings.Contains(v.BreakGlass.CannotSignOff, "requested") {
		t.Errorf("bob's review: %+v", v.BreakGlass)
	}
	if status, raw := bearer(t, h, tok.Body.Token, http.MethodPost, Prefix+"/requests/"+r.ID+"/break-glass/sign-off", map[string]string{"comment": "ok"}); status != http.StatusForbidden {
		t.Errorf("sign-off with a token = %d %s", status, raw)
	}
	status, raw, v := carol.signOff(r.ID, "The checkout needed it: **fine**.")
	if status != http.StatusOK || v.BreakGlass == nil || !v.BreakGlass.Open || len(v.BreakGlass.SignOffs) != 1 || v.BreakGlass.SignOffs[0].Capacity != "owner" {
		t.Fatalf("carol's sign-off = %d %s", status, raw)
	}
	if status, raw, _ := carol.signOff(r.ID, "Again."); status != http.StatusConflict || !strings.Contains(raw, "signed it off already") {
		t.Errorf("carol's second sign-off = %d %s", status, raw)
	}
	var open []BreakGlassItemView
	if status, raw := erin.do(http.MethodGet, Prefix+"/break-glass?open=true", nil, &open); status != http.StatusOK || len(open) != 1 || open[0].Request != r.ID ||
		open[0].BreakGlass.By != "dave" || len(open[0].BreakGlass.SignOffs) != 1 || !open[0].BreakGlass.CanSignOff {
		t.Errorf("erin's open reviews = %d %s", status, raw)
	}
	for _, u := range []*user{bob, carol, dave} {
		if status, _ := u.do(http.MethodGet, Prefix+"/break-glass", nil, nil); status != http.StatusForbidden {
			t.Errorf("the reviews listed for someone neither admin nor auditor: %d", status)
		}
	}
	// An auditor's API token neither lists the reviews nor signs one off.
	var erins CreatedTokenOutput
	if status, raw := erin.do(http.MethodPost, Prefix+"/tokens", map[string]any{"name": "ci", "scopes": []string{"requests:read", "requests:write"}}, &erins.Body); status != http.StatusCreated {
		t.Fatalf("create token = %d %s", status, raw)
	}
	if status, raw := bearer(t, h, erins.Body.Token, http.MethodGet, Prefix+"/break-glass", nil); status != http.StatusForbidden {
		t.Errorf("the reviews listed with a token = %d %s", status, raw)
	}
	if status, raw := bearer(t, h, erins.Body.Token, http.MethodPost, Prefix+"/requests/"+r.ID+"/break-glass/sign-off", map[string]string{"comment": "With a token."}); status != http.StatusForbidden {
		t.Errorf("a sign-off with a token = %d %s", status, raw)
	}
	status, raw, v = erin.signOff(r.ID, "Reviewed with security: the change was the fix.")
	if status != http.StatusOK || v.BreakGlass.Open || v.BreakGlass.ClosedAt == nil || len(v.BreakGlass.SignOffs) != 2 || v.BreakGlass.SignOffs[1].Capacity != "auditor" {
		t.Fatalf("erin's sign-off = %d %s", status, raw)
	}
	if status, raw, _ := pat.signOff(r.ID, "Late."); status != http.StatusConflict || !strings.Contains(raw, "closed") {
		t.Errorf("a sign-off of a closed review = %d %s", status, raw)
	}
	if status, raw := erin.do(http.MethodGet, Prefix+"/break-glass?open=true", nil, &open); status != http.StatusOK || len(open) != 0 {
		t.Errorf("open reviews after the close = %d %s", status, raw)
	}
	var all []BreakGlassItemView
	if status, raw := alice.do(http.MethodGet, Prefix+"/break-glass", nil, &all); status != http.StatusOK || len(all) != 1 || all[0].BreakGlass.Open {
		t.Errorf("alice's reviews = %d %s", status, raw)
	}
	if ev, personal := h.audited("request.break_glass_signed_off", r.ID); len(ev) != 2 || personal[ev[0].Actor.ID] != "carol" || ev[0].Details["capacity"] != "owner" ||
		personal[ev[1].Actor.ID] != "erin" || ev[1].Details["capacity"] != "auditor" {
		t.Errorf("the sign-offs in the audit log = %+v", ev)
	}
	if ev, _ := h.audited("request.break_glass_review_closed", r.ID); len(ev) != 1 || ev[0].Details["incident"] != incident {
		t.Errorf("the close in the audit log = %+v", ev)
	}
	if kinds := h.eventKinds(r.ID); count(kinds, "break_glass_signed_off") != 2 || count(kinds, "break_glass_review_closed") != 1 {
		t.Errorf("events = %v", kinds)
	}
}

// TestBreakGlassChangeWindow: an approved request Quai holds outside its change windows is broken
// and merges at once, past its windows: quai/approval success with "Break-glass by dave", the
// hold lifted, no merge outside the window recorded. An approved request its windows do not
// hold, a rejected or a cancelled one, cannot be broken.
func TestBreakGlassChangeWindow(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarnessOver(t, "auto", "", nil, nil, giteaconn.New, withWindows(officeHours), withBreakGlass)
	alice, bob, dave := h.as("alice"), h.as("bob"), h.as("dave")
	h.clock.set(saturday)
	r := opened(t, h)
	alice.approve(r.ID)
	r = held(bob, r.ID)
	v := dave.read(r.ID)
	if !v.CanBreakGlass {
		t.Fatalf("dave cannot break glass for a held request: %q", v.CannotBreakGlass)
	}
	status, raw, broken := dave.breakGlass(v, "Payment outage needs the bigger VM", "INC-2002")
	if status != http.StatusOK || broken.BreakGlass == nil || broken.BreakGlass.From != "approved" {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	awaitApproval(t, h, remote, v.Delivery.Head, gitea.StatusSuccess, "Break-glass by dave")
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	kinds := h.eventKinds(r.ID)
	if count(kinds, "merged") != 1 || count(kinds, "merged_outside_window") != 0 || count(kinds, "break_glass") != 1 {
		t.Errorf("events = %v", kinds)
	}
	if ev, _ := h.audited("request.break_glass", r.ID); len(ev) != 1 || ev[0].Details["windowHeld"] != true || ev[0].Details["from"] != "approved" {
		t.Errorf("the break-glass in the audit log = %+v", ev)
	}
	if page := h.auditLog("limit=500&action=request.merged_outside_window"); len(page.Events) != 0 {
		t.Errorf("a merge outside the window was recorded: %+v", page.Events)
	}

	// A rejected request is decided: break-glass does not reopen it.
	h.clock.set(monday)
	other := bob.submit(newVM("prod", "glass03", 2))
	other = bob.awaitDelivery(other.ID, "opened", func(v RequestView) bool { return v.Delivery.PullRequest != "" })
	if status, raw := alice.do(http.MethodPost, Prefix+"/requests/"+other.ID+"/review", map[string]string{"decision": "deny", "comment": "not now"}, nil); status != http.StatusOK {
		t.Fatalf("deny = %d %s", status, raw)
	}
	other = dave.read(other.ID)
	if other.CanBreakGlass {
		t.Error("dave may break glass for a rejected request")
	}
	if status, raw, _ := dave.breakGlass(other, "x", "INC-2003"); status != http.StatusConflict || !strings.Contains(raw, "rejected") {
		t.Errorf("break-glass of a rejected request = %d %s", status, raw)
	}
}

// TestBreakGlassManualMerge: with merge: manual, break-glass turns quai/approval to success with
// "Break-glass by dave" and a person merges; Quai's comment on the pull request says it was let
// through by break-glass, naming who and the incident; the merge is not a bypass.
func TestBreakGlassManualMerge(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarnessOver(t, "manual", "", nil, nil, giteaconn.New, withBreakGlass)
	g := giteatest.Get(t)
	bob, dave := h.as("bob"), h.as("dave")
	r := opened(t, h)
	if status, raw, _ := dave.breakGlass(r, "Fix of the outage", "INC-2004"); status != http.StatusOK {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	awaitApproval(t, h, remote, r.Delivery.Head, gitea.StatusSuccess, "Break-glass by dave")
	if v := bob.read(r.ID); v.State != "approved" {
		t.Errorf("with merge: manual, the broken request is %s", v.State)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		merged, _, err := g.Client.MergePullRequest(remote.Owner, remote.Name, 1, gitea.MergePullRequestOption{Style: gitea.MergeStyleMerge})
		if merged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cannot merge: %v", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	r = bob.awaitDelivery(r.ID, "applied by the merge", func(v RequestView) bool { return v.State == "applied" })
	if kinds := h.eventKinds(r.ID); count(kinds, "merged") != 1 || count(kinds, "merged_without_approval") != 0 {
		t.Errorf("events = %v", kinds)
	}
	comments, _, err := g.Client.ListIssueComments(remote.Owner, remote.Name, 1, gitea.ListIssueCommentOptions{})
	if err != nil || len(comments) != 1 || !strings.Contains(comments[0].Body, "break-glass") || !strings.Contains(comments[0].Body, "Break-glass-by: dave") ||
		!strings.Contains(comments[0].Body, "Break-glass-incident: INC-2004") || strings.Contains(comments[0].Body, "outage") {
		t.Errorf("comments = %+v, %v", comments, err)
	}
}

// breakGlassHarness serves the example repository delivered by direct commits, its floor rule
// asking platform-admins for production, with the break-glass users, auth.stepUp at five minutes
// and the authentication's clock the test's.
func breakGlassHarness(t *testing.T, stepUp bool) *harness {
	t.Helper()
	configure := []func(*config.Config){withBreakGlass}
	if stepUp {
		configure = append(configure, func(c *config.Config) { c.Auth.StepUp = &config.StepUp{MaxAge: 5 * time.Minute} })
	}
	h := expiryHarness(t, nil, "", configure...)
	h.auth.UseClock(h.clock.now)
	return h
}

// TestBreakGlassDirect: in direct mode, a broken request is committed with the breaker and the
// incident as trailers. With auth.stepUp configured, break-glass and the review's sign-offs need a
// recent sign-in, the refusals audited; a deactivated person cannot break glass. An open review
// is reminded each day under the sweep, once however often it runs, not after its close.
func TestBreakGlassDirect(t *testing.T) {
	t.Parallel()
	h := breakGlassHarness(t, true)
	t0 := time.Now().UTC().Truncate(time.Second)
	h.clock.set(t0)
	bob, dave := h.as("bob"), h.as("dave")
	h.as("erin") // in the directory, for the alerts
	h.as("alice")
	r := bob.submit(newVM("prod", "glass01", 2))
	r = dave.read(r.ID)
	if r.State != "pending_approval" || !r.CanBreakGlass || !r.BreakGlassStepUp || r.Delivery != nil {
		t.Fatalf("dave reads %+v", r)
	}
	h.clock.set(t0.Add(10 * time.Minute))
	status, raw, _ := dave.breakGlass(r, "DNS outage", "INC-3001")
	stepUpRefused(t, "an old sign-in's break-glass", status, raw, "more than 5m0s ago")
	if ev, _ := h.audited("auth.step_up_refused", r.ID); len(ev) != 1 || ev[0].Details["attempted"] != "request.break_glass" {
		t.Errorf("the refusal in the audit log = %+v", ev)
	}
	dave = h.as("dave")
	status, raw, broken := dave.breakGlass(r, "DNS outage", "INC-3001")
	if status != http.StatusOK || broken.BreakGlass == nil || !broken.BreakGlass.StepUp {
		t.Fatalf("break-glass after a step-up = %d %s", status, raw)
	}
	bob.await(r.ID, "applied")
	out, err := exec.Command("git", "--git-dir", h.bare, "log", "-1", "--format=%B", "main").Output() // #nosec G204 -- test
	if err != nil || !strings.Contains(string(out), "Break-glass-by: dave\n") || !strings.Contains(string(out), "Break-glass-incident: INC-3001") ||
		strings.Contains(string(out), "Approved-by") || strings.Contains(string(out), "DNS outage") {
		t.Errorf("the commit = %q, %v", out, err)
	}

	// The review is reminded each day while open, once per day however often the sweep runs.
	h.sweep(t0.Add(23 * time.Hour))
	if kinds := h.eventKinds(r.ID); count(kinds, "break_glass_reminded") != 0 {
		t.Errorf("reminded within the day: %v", kinds)
	}
	h.sweep(t0.Add(24*time.Hour + 10*time.Minute))
	h.sweep(t0.Add(24*time.Hour + 11*time.Minute))
	if kinds := h.eventKinds(r.ID); count(kinds, "break_glass_reminded") != 1 {
		t.Errorf("after a day: %v", kinds)
	}
	if n := alertsOf(t, h, "alert_break_glass_review"); n != 1 {
		t.Errorf("%d review reminders alerted, want 1", n)
	}
	h.sweep(t0.Add(48*time.Hour + 9*time.Minute))
	if kinds := h.eventKinds(r.ID); count(kinds, "break_glass_reminded") != 1 {
		t.Errorf("reminded again within a day of the last reminder: %v", kinds)
	}
	h.sweep(t0.Add(48*time.Hour + 10*time.Minute))
	if kinds := h.eventKinds(r.ID); count(kinds, "break_glass_reminded") != 2 {
		t.Errorf("after two days: %v", kinds)
	}

	// Sign-offs need a recent sign-in too.
	at := t0.Add(49 * time.Hour)
	h.clock.set(at)
	erin, alice := h.as("erin"), h.as("alice")
	h.clock.set(at.Add(10 * time.Minute))
	status, raw, _ = erin.signOff(r.ID, "Reviewed.")
	stepUpRefused(t, "an old sign-in's sign-off", status, raw, "more than 5m0s ago")
	h.clock.set(at.Add(time.Minute))
	if status, raw, _ := erin.signOff(r.ID, "Reviewed."); status != http.StatusOK {
		t.Fatalf("erin's sign-off = %d %s", status, raw)
	}
	status, raw, v := alice.signOff(r.ID, "Agreed.\nThe incident report is INC-3001.")
	i := slices.IndexFunc(v.BreakGlass.SignOffs, func(s BreakGlassSignOffView) bool { return s.Signer == "alice" })
	if status != http.StatusOK || v.BreakGlass.Open || i < 0 || v.BreakGlass.SignOffs[i].Capacity != "platform-admin" || !v.BreakGlass.SignOffs[i].StepUp {
		t.Fatalf("alice's sign-off = %d %s", status, raw)
	}
	h.sweep(t0.Add(96 * time.Hour))
	if kinds := h.eventKinds(r.ID); count(kinds, "break_glass_reminded") != 2 {
		t.Errorf("a closed review was reminded: %v", kinds)
	}

	// A deactivated person no longer breaks glass, even in a session opened before.
	h.clock.set(t0.Add(100 * time.Hour))
	bob = h.as("bob")
	other := bob.submit(newVM("prod", "glass02", 2))
	dave = h.as("dave")
	other = dave.read(other.ID)
	if _, err := h.store.Pool.Exec(context.Background(), `UPDATE users SET deactivated_at = now() WHERE name = 'dave'`); err != nil {
		t.Fatal(err)
	}
	if status, raw, _ := dave.breakGlass(other, "x", "INC-3002"); status == http.StatusOK {
		t.Errorf("a deactivated person broke glass: %s", raw)
	}
	if v := bob.read(other.ID); v.State != "pending_approval" || v.BreakGlass != nil {
		t.Errorf("after a deactivated person's break-glass: %s %+v", v.State, v.BreakGlass)
	}
}

// alertsOf counts the alerts of a kind recorded.
func alertsOf(t *testing.T, h *harness, kind string) int {
	t.Helper()
	var n int
	if err := h.store.Pool.QueryRow(context.Background(), `SELECT count(*) FROM events WHERE kind = $1`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBreakGlassRace: break-glass and a denial of the same request at the same moment are decided
// under its row lock, one after the other: whichever comes first decides, the other is refused.
func TestBreakGlassRace(t *testing.T) {
	t.Parallel()
	h := breakGlassHarness(t, false)
	bob, alice, dave := h.as("bob"), h.as("alice"), h.as("dave")
	for i, breakFirst := range []bool{true, false} {
		r := bob.submit(newVM("prod", "race-glass0"+strconv.Itoa(i), 2))
		r = dave.read(r.ID)
		ctx := context.Background()
		tx, err := h.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM requests WHERE id = $1 FOR UPDATE`, r.ID); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var broke, denied int
		breaks := func() { defer wg.Done(); broke, _, _ = dave.breakGlass(r, "outage", "INC-4001") }
		denies := func() {
			defer wg.Done()
			denied, _ = alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "deny", "comment": "no"}, nil)
		}
		first, second := denies, breaks
		if breakFirst {
			first, second = breaks, denies
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
		kinds := h.eventKinds(r.ID)
		if breakFirst {
			if broke != http.StatusOK || denied != http.StatusConflict || count(kinds, "rejected") != 0 {
				t.Errorf("break-glass first: %d, the denial %d, events %v", broke, denied, kinds)
			}
			bob.await(r.ID, "applied")
		} else {
			v := bob.read(r.ID)
			if v.State != "rejected" || broke != http.StatusConflict || v.BreakGlass != nil || count(kinds, "break_glass") != 0 {
				t.Errorf("denied first: %s, break-glass %d, events %v", v.State, broke, kinds)
			}
		}
	}
}

// TestBreakGlassSlack: a break-glass alert goes on every channel: the repository's Slack channel,
// the owning team's, and a direct message to each owner Slack knows, with identifiers only, never
// the reason.
func TestBreakGlassSlack(t *testing.T) {
	t.Parallel()
	fake := slacktest.Start(t)
	const shop = "C0SHOP0001"
	h, ids := slackHarness(t, fake, func(c *config.Config) {
		c.Notifications.Slack.Channels = append(c.Notifications.Slack.Channels,
			config.SlackChannel{Name: "shop", Channel: shop, Teams: []string{"shop"}, Events: []string{"alert"}},
			config.SlackChannel{Name: "other", Channel: "C0OTHER001", Teams: []string{"finops"}, Events: []string{"alert"}})
		c.Auth.Dev.Users = append(c.Auth.Dev.Users, config.DevUser{Name: "dave", Email: "dave@example.com", Groups: []string{"oncall", "developers"}})
		c.Auth.Roles = append(c.Auth.Roles, config.RoleMapping{Group: "oncall", Role: "break-glass"})
	})
	fake.AddChannel(shop)
	fake.AddChannel("C0OTHER001")
	daveID := fake.Member("dave@example.com")
	bob, dave := h.as("bob"), h.as("dave")
	h.as("carol")
	// web01 becomes the shop team's: bob and carol own it.
	web01, err := os.ReadFile(filepath.Join(h.work, "envs/prod/vms/web01.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	h.push("envs/prod/vms/web01.yaml", strings.Replace(string(web01), "owner: bob", "owner: team:shop", 1))
	if err := h.ingest.Poll(context.Background(), "infra-live"); err != nil {
		t.Fatal(err)
	}
	r := bob.submit(resizeBody(6))
	if r.State != "pending_approval" {
		t.Fatalf("submitted %s", r.State)
	}
	h.quiet(t)
	before := len(fake.To(ids["bob"]))
	const reason = "Checkout is down: <!channel> *now*"
	status, raw, broken := dave.breakGlass(dave.read(r.ID), reason, "INC-3001")
	if status != http.StatusOK || broken.BreakGlass == nil {
		t.Fatalf("break-glass = %d %s", status, raw)
	}
	h.quiet(t)
	alert := func(m slacktest.Message) bool { return strings.Contains(m.PlainText(), "is merged by break-glass") }
	for name, channel := range map[string]string{"infra": slackInfra, "shop": shop} {
		posts := slices.DeleteFunc(fake.Posts(channel), func(m slacktest.Message) bool { return !alert(m) })
		if len(posts) != 1 {
			t.Errorf("%d break-glass alerts in the %s channel, want 1", len(posts), name)
			continue
		}
		text := posts[0].PlainText()
		if !strings.Contains(text, "INC-3001") || !strings.Contains(text, "dave") || strings.Contains(text, "Checkout") {
			t.Errorf("the %s channel's alert:\n%s", name, text)
		}
	}
	if posts := fake.Posts("C0OTHER001"); len(posts) != 0 {
		t.Errorf("another team's channel was posted: %+v", posts)
	}
	dms := fake.To(ids["bob"])[before:]
	if len(dms) == 0 || !slices.ContainsFunc(dms, alert) {
		t.Errorf("bob, an owner, got no direct message of the alert: %+v", dms)
	}
	if slices.ContainsFunc(fake.To(daveID), alert) {
		t.Error("dave, who broke glass and owns nothing, got the alert")
	}
	if slices.ContainsFunc(fake.To(ids["alice"]), alert) {
		t.Error("alice, neither an admin of the repository nor an owner, got the alert")
	}
}
