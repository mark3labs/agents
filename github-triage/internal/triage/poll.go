package triage

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/mark3labs/bonnie/runtime"
	"github.com/mark3labs/bonnie/schedule"
)

// ScheduleName is the poll schedule's name.
const ScheduleName = "poll-prs"

// Prepare is the poll schedule's Prepare callback. It reads the open PRs,
// compares each with the store, and dispatches one triage run per PR whose
// fingerprint changed since its last finished triage, or whose nudge is due.
// It only reads GitHub and writes the store, so a repeated call is safe.
//
// Comments and reviews are re-read only for PRs whose updated_at moved. CI
// results and merge conflicts do not move updated_at, so they are read for
// every open PR on every poll: about three API calls per open PR.
func Prepare(ctx context.Context, fire schedule.Fire) ([]schedule.Dispatch, error) {
	c, g, err := Client()
	if err != nil {
		return nil, err
	}
	st, err := OpenStore(c)
	if err != nil {
		return nil, err
	}
	self, err := g.Self(ctx)
	if err != nil {
		return nil, err
	}
	prs, err := g.OpenPRs(ctx)
	if err != nil {
		return nil, fmt.Errorf("triage: list PRs: %w", err)
	}

	open := map[int]bool{}
	type due struct {
		pr     PR
		reason string
	}
	var work []due
	for _, pr := range prs {
		open[pr.Number] = true
		row, err := st.PR(ctx, c.Repo, pr.Number)
		if err != nil {
			return nil, err
		}
		updated := pr.UpdatedAt.UTC().Format(time.RFC3339)
		fp := row.Fingerprint
		if updated != row.UpdatedAt || fp == "" {
			// Something changed on GitHub; recompute the fingerprint, which
			// ignores the bot's own writes.
			if fp, err = lightFingerprint(ctx, c, g, self, &pr); err != nil {
				return nil, fmt.Errorf("triage: PR #%d: %w", pr.Number, err)
			}
			if err := st.Observe(ctx, c.Repo, pr.Number, updated, fp); err != nil {
				return nil, err
			}
		}
		full, err := g.GetPR(ctx, pr.Number)
		if err != nil {
			return nil, fmt.Errorf("triage: PR #%d: %w", pr.Number, err)
		}
		merge, err := FetchMergeState(ctx, g, full)
		if err != nil {
			return nil, fmt.Errorf("triage: PR #%d checks: %w", pr.Number, err)
		}
		switch combined := Combine(fp, merge); {
		case row.TriagedFingerprint == "":
			work = append(work, due{pr, "new pull request"})
		case combined != row.TriagedFingerprint:
			work = append(work, due{pr, "new activity, CI results or merge conflicts since the last triage"})
		case row.TriagedDry && !c.DryRun:
			work = append(work, due{pr, "last triage was a dry run"})
		case nudgeDue(c, row):
			work = append(work, due{pr, "the author has not responded; a nudge is due"})
		}
	}

	known, err := st.OpenNumbers(ctx, c.Repo)
	if err != nil {
		return nil, err
	}
	for n := range known {
		if !open[n] {
			if err := st.MarkClosed(ctx, c.Repo, n); err != nil {
				return nil, err
			}
		}
	}

	// Oldest first, so a backlog drains in order.
	sort.Slice(work, func(i, j int) bool { return work[i].pr.Number < work[j].pr.Number })
	if len(work) > c.MaxPRsPerPoll {
		work = work[:c.MaxPRsPerPoll]
	}
	out := make([]schedule.Dispatch, 0, len(work))
	for _, w := range work {
		out = append(out, schedule.Dispatch{
			Key: fmt.Sprintf("pr-%d", w.pr.Number),
			Input: runtime.Input{
				Title:   fmt.Sprintf("Triage %s#%d: %s", c.Repo, w.pr.Number, truncate(w.pr.Title, 80)),
				Text:    fmt.Sprintf("Triage pull request #%d in %s.", w.pr.Number, c.Repo),
				Context: []string{"Why this PR is up for triage: " + w.reason + "."},
			},
		})
	}
	return out, nil
}

func lightFingerprint(ctx context.Context, c *Config, g *GitHub, self string, pr *PR) (string, error) {
	ic, err := g.IssueComments(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	rc, err := g.ReviewComments(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	rv, err := g.Reviews(ctx, pr.Number)
	if err != nil {
		return "", err
	}
	return Fingerprint(c, self, pr, append(ic, rc...), rv), nil
}

func nudgeDue(c *Config, row PRState) bool {
	days := c.AwaitingAuthor.NudgeAfterDays
	if days <= 0 || row.Status != RoleAwaitingAuthor || row.AwaitingSince == "" {
		return false
	}
	since, err := time.Parse(time.RFC3339, row.AwaitingSince)
	if err != nil || time.Since(since) < time.Duration(days)*24*time.Hour {
		return false
	}
	return row.LastNudge == "" || row.LastNudge < row.AwaitingSince
}
