package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/connectors"
	giteaconn "github.com/damhau/quai/backend/internal/connectors/gitea"
	"github.com/damhau/quai/backend/internal/engine/window"
	"github.com/damhau/quai/backend/internal/giteatest"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/store/db"
)

// withWindows sets the change windows of Quai's configuration.
func withWindows(specs ...window.ChangeWindow) func(*config.Config) {
	return func(c *config.Config) { c.ChangeWindows = specs }
}

// The times the tests set the clock to.
var (
	saturday   = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC) // 11:00 in Zurich
	monday     = time.Date(2026, 11, 16, 7, 30, 0, 0, time.UTC) // 08:30 in Zurich
	inFreeze   = time.Date(2026, 12, 20, 10, 0, 0, 0, time.UTC)
	afterFreez = time.Date(2027, 1, 5, 8, 0, 0, 0, time.UTC)
)

// officeHours is a window of Quai's configuration: production changes on weekdays, 08:00 to
// 17:00 in Zurich.
var officeHours = window.ChangeWindow{Name: "office-hours", When: "params.env == 'prod'", Allow: []string{"Mon-Fri 08:00-17:00 Europe/Zurich"}}

// yearEnd is a freeze of Quai's configuration, over the year's end.
var yearEnd = window.ChangeWindow{Name: "year-end", Deny: []string{"2026-12-18 to 2027-01-05 Europe/Zurich"}}

// sweepWindows runs the windows' sweep with the clock at at, as its periodic job would, then
// waits for the jobs it queued.
func (h *harness) sweepWindows(at time.Time) {
	h.t.Helper()
	h.clock.set(at)
	if err := h.requests.SweepWindows(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.awaitJobs(h.t)
}

// awaitApproval waits until quai/approval on a commit is in state, its description containing
// text.
func awaitApproval(t *testing.T, h *harness, r *giteatest.Repo, sha string, want gitea.StatusState, text string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		combined, _, err := giteatest.Get(t).Client.GetCombinedStatus(r.Owner, r.Name, sha)
		if err == nil {
			for _, st := range combined.Statuses {
				if st.Context == "quai/approval" && st.State == want && strings.Contains(st.Description, text) {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("quai/approval on %.12s is not %s with %q: %+v, %v\nlog:\n%s", sha, want, text, combined, err, h.log.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// held waits until the request is approved and held for its change windows.
func held(u *user, id string) RequestView {
	return u.awaitDelivery(id, "held", func(v RequestView) bool {
		return v.State == "approved" && v.ChangeWindow != nil && v.ChangeWindow.Held
	})
}

// TestChangeWindowFreeze: in a freeze of Quai's configuration, an approved request waits,
// approved: quai/approval pending with the reason, the pull request not merged, its page saying
// when it may merge; the repository document's own window, open all the time, does not lift the
// freeze. When the freeze ends, the windows' sweep releases it and it merges, once.
func TestChangeWindowFreeze(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarnessOver(t, "auto", `
  enforcement:
    changeWindows:
      - { name: always, allow: ["Mon-Sun 00:00-24:00 UTC"] }`, nil, nil, giteaconn.New, withWindows(yearEnd))
	g := giteatest.Get(t)
	bob, alice := h.as("bob"), h.as("alice")
	h.clock.set(inFreeze)
	r := opened(t, h)
	opens := time.Date(2027, 1, 4, 23, 0, 0, 0, time.UTC) // 5 January, 00:00 in Zurich
	if w := r.ChangeWindow; w == nil || w.Open || w.Held || w.Opens == nil || !w.Opens.Equal(opens) || w.Zone != "Europe/Zurich" ||
		!slices.Equal(w.Windows, []string{"year-end", "always"}) || !slices.Equal(w.Holding, []string{"year-end"}) || w.Reason != "" {
		t.Fatalf("pending approval, the change window = %+v", w)
	}
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	r = held(bob, r.ID)
	reason := "Approved; waits for the change window, opens Tue 5 Jan 2027 00:00 Europe/Zurich"
	if w := r.ChangeWindow; w.Open || w.Opens == nil || !w.Opens.Equal(opens) || w.Reason != reason {
		t.Errorf("held, the change window = %+v", w)
	}
	sha := h.giteaHead(t, remote, r.Delivery.Branch)
	awaitApproval(t, h, remote, sha, gitea.StatusPending, reason)
	// Sweeps in the freeze change nothing.
	h.sweepWindows(inFreeze.Add(time.Hour))
	h.sweepWindows(inFreeze.Add(2 * time.Hour))
	if pr, _, err := g.Client.GetPullRequest(remote.Owner, remote.Name, 1); err != nil || pr.HasMerged {
		t.Fatalf("merged in the freeze: %+v, %v", pr, err)
	}
	kinds := h.eventKinds(r.ID)
	if count(kinds, "window_waiting") != 1 || count(kinds, "merging") != 0 || count(kinds, "window_opened") != 0 {
		t.Errorf("events in the freeze = %v", kinds)
	}
	if ev, _ := h.audited("request.window_waiting", r.ID); len(ev) != 1 || ev[0].Actor.Type != "system" || ev[0].Details["from"] != "approved" ||
		ev[0].Details["opens"] != "2027-01-04T23:00:00.000Z" {
		t.Errorf("the hold in the audit log = %+v", ev)
	}
	awaitApproval(t, h, remote, sha, gitea.StatusPending, reason)

	h.sweepWindows(afterFreez)
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if r.ChangeWindow != nil {
		t.Errorf("applied, the change window = %+v", r.ChangeWindow)
	}
	h.sweepWindows(afterFreez.Add(time.Minute))
	kinds = h.eventKinds(r.ID)
	if count(kinds, "window_waiting") != 1 || count(kinds, "window_opened") != 1 || count(kinds, "merging") != 1 || count(kinds, "merged") != 1 {
		t.Errorf("events = %v", kinds)
	}
	if ev, _ := h.audited("request.window_opened", r.ID); len(ev) != 1 || ev[0].Actor.Type != "system" {
		t.Errorf("the release in the audit log = %+v", ev)
	}
}

// autoCompleteConnector is the Gitea connector as a platform that merges on Quai's request
// (auto-complete, as Azure DevOps and GitHub do), with a hook before each request.
type autoCompleteConnector struct {
	connectors.Connector
	mu     sync.Mutex
	asks   []connectors.MergeOptions
	before func(n int) error // before the nth request: its error is the platform's answer
}

func (c *autoCompleteConnector) Info() connectors.Info {
	info := c.Connector.Info()
	info.Capabilities = append(slices.Clone(info.Capabilities), connectors.CapAutoComplete)
	return info
}

func (c *autoCompleteConnector) PullRequests() connectors.PullRequests {
	return autoCompletePullRequests{c.Connector.PullRequests(), c}
}

func (c *autoCompleteConnector) asked() []connectors.MergeOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asks)
}

type autoCompletePullRequests struct {
	connectors.PullRequests
	c *autoCompleteConnector
}

func (p autoCompletePullRequests) AutoComplete(ctx context.Context, id string, o connectors.MergeOptions) error {
	p.c.mu.Lock()
	p.c.asks = append(p.c.asks, o)
	n, before := len(p.c.asks), p.c.before
	p.c.mu.Unlock()
	if before != nil {
		if err := before(n); err != nil {
			return err
		}
	}
	_, err := p.Merge(ctx, id, o)
	return err
}

// TestChangeWindowAutoComplete: on a platform that merges on Quai's request, nothing is asked of
// it outside the request's windows. The windows close while the platform waits for a check (the
// request merging): the request goes back to approved and waits, quai/approval pending; it is
// asked again once they open.
func TestChangeWindowAutoComplete(t *testing.T) {
	t.Parallel()
	c := &autoCompleteConnector{}
	h, remote := giteaHarnessOver(t, "auto", "", nil, nil, func(cfg connectors.Config) (connectors.Connector, error) {
		conn, err := giteaconn.New(cfg)
		c.Connector = conn
		return c, err
	}, withWindows(officeHours))
	bob, alice := h.as("bob"), h.as("alice")
	h.clock.set(saturday)
	r := opened(t, h)
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	r = held(bob, r.ID)
	sha := h.giteaHead(t, remote, r.Delivery.Branch)
	awaitApproval(t, h, remote, sha, gitea.StatusPending, "Approved; waits for the change window, opens Mon 08:00 Europe/Zurich")
	h.awaitJobs(t)
	if asks := c.asked(); len(asks) != 0 {
		t.Fatalf("the platform was asked to merge outside the window: %+v", asks)
	}

	// Monday: the platform is asked, and answers that a check is pending; meanwhile, the window
	// closes (Saturday again, to the clock).
	c.mu.Lock()
	c.before = func(n int) error {
		if n == 1 {
			h.clock.set(saturday.Add(7 * 24 * time.Hour))
			return connectors.ErrBlocked
		}
		return nil
	}
	c.mu.Unlock()
	h.clock.set(monday)
	if err := h.requests.SweepWindows(context.Background()); err != nil {
		t.Fatal(err)
	}
	bob.awaitDelivery(r.ID, "merging", func(v RequestView) bool { return v.State == "merging" && len(c.asked()) == 1 })
	h.sweepWindows(saturday.Add(7 * 24 * time.Hour))
	r = held(bob, r.ID)
	awaitApproval(t, h, remote, sha, gitea.StatusPending, "Approved; waits for the change window, opens Mon 08:00 Europe/Zurich")
	if ev, _ := h.audited("request.window_waiting", r.ID); len(ev) != 2 || ev[1].Details["from"] != "merging" {
		t.Errorf("the holds in the audit log = %+v", ev)
	}
	if n := len(c.asked()); n != 1 {
		t.Errorf("asked %d times", n)
	}

	h.sweepWindows(monday.Add(7 * 24 * time.Hour))
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	asks := c.asked()
	if len(asks) != 2 || asks[1].Approval.State != connectors.StatusSuccess {
		t.Errorf("asked %+v", asks)
	}
}

// TestChangeWindowManualMerge: with merge: manual, quai/approval follows the windows, pending
// outside them, success in them; a person who merges while Quai holds the request (the protection
// removed) applies it, and the merge outside its windows is an alert.
func TestChangeWindowManualMerge(t *testing.T) {
	t.Parallel()
	h, remote := giteaHarnessOver(t, "manual", "", nil, nil, giteaconn.New, withWindows(officeHours))
	g := giteatest.Get(t)
	bob, alice := h.as("bob"), h.as("alice")
	h.clock.set(saturday)
	r := opened(t, h)
	if status, body := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, body)
	}
	r = held(bob, r.ID)
	sha := h.giteaHead(t, remote, r.Delivery.Branch)
	awaitApproval(t, h, remote, sha, gitea.StatusPending, "opens Mon 08:00 Europe/Zurich")
	h.sweepWindows(monday)
	awaitApproval(t, h, remote, sha, gitea.StatusSuccess, "Approved in Quai")
	r = bob.awaitDelivery(r.ID, "released", func(v RequestView) bool { return v.ChangeWindow != nil && !v.ChangeWindow.Held && v.ChangeWindow.Open })
	if w := r.ChangeWindow; w.Closes == nil || !w.Closes.Equal(time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)) || w.Zone != "Europe/Zurich" {
		t.Errorf("open, the change window = %+v", w)
	}
	// 17:00 passes: held again.
	h.sweepWindows(time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC))
	r = held(bob, r.ID)
	awaitApproval(t, h, remote, sha, gitea.StatusPending, "opens Tue 08:00 Europe/Zurich")

	if err := remote.Unprotect("main"); err != nil {
		t.Fatal(err)
	}
	h.awaitPullRequest(t, remote, 1, func(*gitea.PullRequest) bool {
		merged, _, _ := g.Client.MergePullRequest(remote.Owner, remote.Name, 1, gitea.MergePullRequestOption{Style: gitea.MergeStyleMerge})
		return merged
	})
	r = bob.awaitDelivery(r.ID, "applied", func(v RequestView) bool { return v.State == "applied" })
	if !hasEvent(r, "merged_outside_window", r.Delivery.PullRequest) {
		t.Errorf("events = %+v", r.Events)
	}
	if ev, _ := h.audited("request.merged_outside_window", r.ID); ev[0].Details["from"] != "approved" {
		t.Errorf("the merge in the audit log = %+v", ev[0])
	}
	alerts, err := h.store.ListTargetEvents(context.Background(), db.ListTargetEventsParams{Target: "repository:infra-live", MaxRows: 10})
	if err != nil || len(alerts) != 1 || alerts[0].Kind != "alert_bypass" {
		t.Fatalf("alerts = %+v, %v", alerts, err)
	}
	var data map[string]any
	_ = json.Unmarshal(alerts[0].Data, &data)
	if data["event"] != "merged_outside_window" || !strings.Contains(data["message"].(string), "outside its change windows") {
		t.Errorf("the alert = %v", data)
	}
}

// directSpec is the example repository's spec, delivered by direct commits, with more lines.
func directSpec(bare, more string) string {
	return `connector: generic
  url: ` + bare + `
  sync: { minInterval: 1ns }` + more
}

// TestChangeWindowDirect: delivered by direct commits, an approved request waits for its window
// as well, the commit not made; a window's when decides what it applies to (production here); a
// freeze added to the repository document while a request waits holds it, from the next decision
// on, and its page says so; when it ends, the commit is made.
func TestChangeWindowDirect(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := startRepo(t, dir, "{ name: infra-live, title: Infrastructure }", directSpec(bare, ""), repositories.Options{}, withWindows(officeHours))
	h.bare, h.work = bare, work
	bob, alice := h.as("bob"), h.as("alice")
	h.clock.set(saturday)

	// Development changes are not in the window's scope: committed.
	var dev RequestView
	body := map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "dev/build-agent-01", "action": "resize",
		"actionInputs": map[string]any{"cpu": 4, "memory_gb": 8}, "reason": "Builds are slow"}
	if status, b := bob.do(http.MethodPost, Prefix+"/requests/type", body, &dev); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, b)
	}
	if bob.await(dev.ID, "applied"); count(h.eventKinds(dev.ID), "window_waiting") != 0 {
		t.Errorf("the development change waited: %v", h.eventKinds(dev.ID))
	}

	var r RequestView
	if status, b := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, b)
	}
	if status, b := alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil); status != http.StatusOK {
		t.Fatalf("approve = %d %s", status, b)
	}
	r = held(bob, r.ID)
	if f, _ := h.file("envs/prod/vms/web01.yaml"); strings.Contains(f, "cpu: 6") {
		t.Fatal("committed outside the window")
	}

	// A maintenance freeze on Monday, added to the repository document.
	docs, err := repositories.Parse([]byte("apiVersion: quai.dev/v1beta1\nkind: Repository\nmetadata: { name: infra-live, title: Infrastructure }\nspec:\n  " + directSpec(bare, `
  enforcement:
    changeWindows:
      - { name: maintenance, deny: ["2026-11-16 to 2026-11-17 Europe/Zurich"] }`) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Reconcile(context.Background(), h.store, docs); err != nil {
		t.Fatal(err)
	}
	h.sweepWindows(monday)
	r = held(bob, r.ID)
	if w := r.ChangeWindow; !slices.Equal(w.Windows, []string{"office-hours", "maintenance"}) || !slices.Equal(w.Holding, []string{"maintenance"}) ||
		w.Reason != "Approved; waits for the change window, opens Tue 08:00 Europe/Zurich" {
		t.Errorf("in the freeze, the change window = %+v", w)
	}
	if f, _ := h.file("envs/prod/vms/web01.yaml"); strings.Contains(f, "cpu: 6") {
		t.Fatal("committed in the freeze")
	}

	h.sweepWindows(monday.Add(24 * time.Hour))
	bob.await(r.ID, "applied")
	if f, _ := h.file("envs/prod/vms/web01.yaml"); !strings.Contains(f, "cpu: 6") {
		t.Errorf("not committed: %s", f)
	}
	kinds := h.eventKinds(r.ID)
	if count(kinds, "window_waiting") != 1 || count(kinds, "window_opened") != 1 || count(kinds, "committed") != 1 {
		t.Errorf("events = %v", kinds)
	}
}

// TestChangeWindowConditionError: a window whose when cannot be decided for a request (a param
// it does not have) holds it, whatever the time, and says why: an error never lets it merge.
func TestChangeWindowConditionError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	bare, work := localRemote(t, dir)
	h := startRepo(t, dir, "{ name: infra-live, title: Infrastructure }", directSpec(bare, ""), repositories.Options{},
		withWindows(window.ChangeWindow{Name: "gold", When: "params.tier == 'gold'", Allow: []string{"Mon-Sun 00:00-24:00 UTC"}}))
	h.bare, h.work = bare, work
	bob := h.as("bob")
	var r RequestView
	body := map[string]any{"repository": "infra-live", "type": "vm", "kind": "action", "key": "dev/build-agent-01", "action": "resize",
		"actionInputs": map[string]any{"cpu": 4, "memory_gb": 8}, "reason": "Builds are slow"}
	if status, b := bob.do(http.MethodPost, Prefix+"/requests/type", body, &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, b)
	}
	r = held(bob, r.ID)
	if w := r.ChangeWindow; w.Open || !strings.Contains(w.Error, "change window gold") || !strings.Contains(w.Error, "tier") ||
		!strings.HasPrefix(w.Reason, "Approved; held: change window gold") || w.Opens != nil {
		t.Errorf("the change window = %+v", w)
	}
	h.sweepWindows(time.Now().Add(time.Hour))
	if r = bob.await(r.ID, "approved"); !r.ChangeWindow.Held {
		t.Errorf("released: %+v", r.ChangeWindow)
	}
	if f, _ := h.file("envs/dev/vms/build-agent-01.yaml"); strings.Contains(f, "cpu: 4") {
		t.Fatal("committed")
	}
}
