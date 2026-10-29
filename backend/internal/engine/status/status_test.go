package status

import "testing"

func TestDerive(t *testing.T) {
	// Commits c1 to c5 on main, in order; the resource last changed at c3.
	order := map[string]int{"c1": 1, "c2": 2, "c3": 3, "c4": 4, "c5": 5}
	after := func(c string) bool { return order[c] >= 3 }
	run := func(kind, status, commit string) Run { return Run{Kind: kind, Status: status, Commit: commit} }
	drift := func(commit string, drifted bool) Run {
		return Run{Kind: "drift-check", Status: "succeeded", Commit: commit, Drifted: drifted}
	}
	for _, c := range []struct {
		name string
		in   Input
		want string
		run  int
	}{
		{"an open request", Input{Pending: true, Runner: true, Runs: []Run{run("apply", "succeeded", "c4")}, After: after}, PendingChange, -1},
		{"no runner", Input{Runs: []Run{run("apply", "succeeded", "c4")}, After: after}, InRepo, -1},
		{"no run since the change", Input{Runner: true, Runs: []Run{run("apply", "succeeded", "c2")}, After: after}, InRepo, -1},
		{"applied at the change", Input{Runner: true, Runs: []Run{run("apply", "succeeded", "c3")}, After: after}, Applied, 0},
		{"one apply covering later merges", Input{Runner: true, Runs: []Run{run("apply", "succeeded", "c5"), run("apply", "failed", "c2")}, After: after}, Applied, 0},
		{"a failed apply", Input{Runner: true, Runs: []Run{run("apply", "failed", "c4"), run("apply", "succeeded", "c2")}, After: after}, Failed, 0},
		{"a fix applied after a failure", Input{Runner: true, Runs: []Run{run("apply", "succeeded", "c5"), run("apply", "failed", "c4")}, After: after}, Applied, 0},
		{"applying", Input{Runner: true, Runs: []Run{run("apply", "running", "c5"), run("apply", "succeeded", "c4")}, After: after}, Applying, 0},
		{"planning", Input{Runner: true, Runs: []Run{run("plan", "running", "c5"), run("apply", "failed", "c4")}, After: after}, Planning, 0},
		{"a plan done, the apply decides", Input{Runner: true, Runs: []Run{run("plan", "succeeded", "c5"), run("apply", "failed", "c4")}, After: after}, Failed, 1},
		{"an older run still running", Input{Runner: true, Runs: []Run{run("apply", "succeeded", "c5"), run("plan", "running", "c4")}, After: after}, Applied, 0},
		{"a running run before the change", Input{Runner: true, Runs: []Run{run("apply", "running", "c2")}, After: after}, InRepo, -1},
		{"drifted since the apply", Input{Runner: true, Runs: []Run{drift("c5", true), run("apply", "succeeded", "c4")}, After: after}, Drifted, 0},
		{"a clean drift check", Input{Runner: true, Runs: []Run{drift("c5", false), run("apply", "succeeded", "c4")}, After: after}, Applied, 1},
		{"an apply after the drift", Input{Runner: true, Runs: []Run{run("apply", "succeeded", "c5"), drift("c4", true)}, After: after}, Applied, 0},
		{"a drift check before the change", Input{Runner: true, Runs: []Run{drift("c2", true)}, After: after}, InRepo, -1},
	} {
		if got, i := Derive(c.in); got != c.want || i != c.run {
			t.Errorf("%s: %s, run %d", c.name, got, i)
		}
	}
}
