// Package status derives a resource's status from the runs of its stack
// (docs/design/06-runs-status-and-drift.md#resource-status). Status is never stored: it follows
// from the commit that last changed the resource's region and what the stack's runner reported
// since.
package status

// Statuses of a resource.
const (
	InRepo        = "in_repo"        // no run reported since its last change
	PendingChange = "pending_change" // an open request changes it
	Planning      = "planning"
	Applying      = "applying"
	Applied       = "applied"
	Failed        = "failed"
	Drifted       = "drifted"
)

// Run is a run of the resource's stack on the default branch.
type Run struct {
	Kind   string // plan, apply, sync, drift-check
	Status string // running, succeeded, failed
	Commit string
	// Drifted: a drift check that reports changes to the resource's Terraform addresses.
	Drifted bool
}

// Input is what a resource's status follows from.
type Input struct {
	Pending bool  // an open request changes the resource
	Runner  bool  // the repository's runs are reported (spec.runner)
	Runs    []Run // newest first
	// After reports whether a run's commit is at or after the commit that last changed the
	// resource: that commit or one of its descendants.
	After func(commit string) bool
}

// Derive returns a resource's status and the run it comes from, as an index in Runs (-1 for
// none). Only runs at or after the last change count: the latest plan or apply, when it is
// running; then a drift check reporting the resource, newer than the latest apply; then the
// latest apply, succeeded or failed. Without a runner, a resource is in the repository.
func Derive(in Input) (string, int) {
	if in.Pending {
		return PendingChange, -1
	}
	if !in.Runner {
		return InRepo, -1
	}
	seenRun := false   // a plan or an apply, newer than the one being looked at
	seenDrift := false // a finished drift check: only the latest one says whether it drifted
	for i, r := range in.Runs {
		if in.After == nil || !in.After(r.Commit) {
			continue
		}
		switch r.Kind {
		case "plan", "apply":
			if r.Status == "running" {
				if seenRun {
					continue
				}
				if r.Kind == "plan" {
					return Planning, i
				}
				return Applying, i
			}
			seenRun = true
			if r.Kind == "apply" {
				if r.Status == "succeeded" {
					return Applied, i
				}
				return Failed, i
			}
		case "drift-check":
			if r.Status == "running" || seenDrift {
				continue
			}
			seenDrift = true
			if r.Drifted {
				return Drifted, i
			}
		}
	}
	return InRepo, -1
}
