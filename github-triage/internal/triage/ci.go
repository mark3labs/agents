package triage

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// CI states of a head commit.
const (
	CISuccess = "success" // every check passed
	CIFailure = "failure" // at least one check failed
	CIPending = "pending" // checks are running or waiting
	CINone    = "none"    // no checks yet, for example while runs wait for approval
)

// MergeState is what the policy needs to know about a PR's commit and its
// base branch.
type MergeState struct {
	CI       string    `json:"ci"`
	Failing  []string  `json:"failing_checks,omitempty"`
	FailedAt time.Time `json:"-"`
	// Conflict is true when GitHub reports merge conflicts with the base.
	// While GitHub is still computing mergeability it is false.
	Conflict bool `json:"conflict"`
}

// Key is the part of the fingerprint that tracks CI and conflicts. Neither
// changes a PR's updated_at, so the poll checks them on every PR.
func (m MergeState) Key() string {
	return fmt.Sprintf("ci=%s;conflict=%t", m.CI, m.Conflict)
}

// Green reports whether CI passed and the PR merges cleanly.
func (m MergeState) Green() bool { return m.CI == CISuccess && !m.Conflict }

// FetchMergeState reads mergeability and the head commit's checks. pr must
// come from GetPR, which is the only endpoint that reports mergeability.
func FetchMergeState(ctx context.Context, g *GitHub, pr *PR) (MergeState, error) {
	runs, statuses, err := g.Checks(ctx, pr.Head.SHA)
	if err != nil {
		return MergeState{}, err
	}
	m := summarizeChecks(runs, statuses)
	m.Conflict = pr.Mergeable != nil && !*pr.Mergeable && pr.MergeableState == "dirty"
	return m, nil
}

func summarizeChecks(runs []CheckRun, statuses []CommitStatus) MergeState {
	m := MergeState{CI: CINone}
	pending, passed := false, false
	fail := func(name string, at time.Time) {
		m.Failing = append(m.Failing, name)
		if at.After(m.FailedAt) {
			m.FailedAt = at
		}
	}
	for _, r := range runs {
		switch {
		case r.Status != "completed":
			pending = true
		case r.Conclusion == "failure", r.Conclusion == "timed_out", r.Conclusion == "cancelled", r.Conclusion == "startup_failure":
			fail(r.Name, r.CompletedAt)
		case r.Conclusion == "action_required":
			pending = true
		default: // success, neutral, skipped, stale
			passed = true
		}
	}
	for _, s := range statuses {
		switch s.State {
		case "failure", "error":
			fail(s.Context, s.UpdatedAt)
		case "pending":
			pending = true
		case "success":
			passed = true
		}
	}
	sort.Strings(m.Failing)
	switch {
	case len(m.Failing) > 0:
		m.CI = CIFailure
	case pending:
		m.CI = CIPending
	case passed:
		m.CI = CISuccess
	}
	return m
}
