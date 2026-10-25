package triage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Markers tag the bot's own comments, so a repeated action can see that it
// already happened, and so the bot's comments are told apart from a human's
// even when the token belongs to a maintainer's own account.
const (
	markerPrefix           = "<!-- github-triage:"
	MarkerNudge            = "<!-- github-triage:nudge -->"
	MarkerClosePromotional = "<!-- github-triage:close-promotional -->"
	MarkerCloseNoIssue     = "<!-- github-triage:close-no-issue -->"
	MarkerDuplicate        = "<!-- github-triage:duplicate -->"
)

// Snapshot is everything the policy reads about one PR.
type Snapshot struct {
	Self           string
	PR             *PR
	Files          []File
	IssueComments  []Comment
	ReviewComments []Comment
	Reviews        []Review
	Graph          *PRGraph
	PendingRuns    []WorkflowRun
	Merge          MergeState
	// PrevAwaitingSince is when the last triage saw the author's turn
	// begin, so a wait for a merge-conflict fix keeps its start time.
	PrevAwaitingSince time.Time
	// Others are the PRs a duplicate check compares against.
	Others []Candidate
}

// Fetch reads a PR's full snapshot, and the duplicate pool when duplicate
// detection is on.
func Fetch(ctx context.Context, c *Config, g *GitHub, n int) (*Snapshot, error) {
	self, err := g.Self(ctx)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Self: self}
	if s.PR, err = g.GetPR(ctx, n); err != nil {
		return nil, err
	}
	if s.Files, err = g.Files(ctx, n); err != nil {
		return nil, err
	}
	if s.IssueComments, err = g.IssueComments(ctx, n); err != nil {
		return nil, err
	}
	if s.ReviewComments, err = g.ReviewComments(ctx, n); err != nil {
		return nil, err
	}
	if s.Reviews, err = g.Reviews(ctx, n); err != nil {
		return nil, err
	}
	if s.Graph, err = g.Graph(ctx, n); err != nil {
		return nil, err
	}
	if s.PR.State == "open" && s.PR.Head.SHA != "" {
		if s.PendingRuns, err = g.PendingApprovalRuns(ctx, s.PR.Head.SHA); err != nil {
			return nil, err
		}
		if s.Merge, err = FetchMergeState(ctx, g, s.PR); err != nil {
			return nil, err
		}
		if c.Duplicates.Enabled {
			if s.Others, err = FetchCandidates(ctx, c, g, s.PR); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// FingerprintVersion prefixes every fingerprint. Bump it when Fingerprint
// changes what it covers: a poll then recomputes every stored fingerprint
// instead of trusting one made by the old formula.
const FingerprintVersion = "3"

// IsOwn reports whether a comment is one the triage posted.
func IsOwn(self string, cm Comment) bool {
	return cm.User.Login == self && strings.Contains(cm.Body, markerPrefix)
}

// Fingerprint hashes the activity that should trigger a triage. The bot's
// own comments, other bots' comments, and label changes other than the
// exempt labels are left out, so the bot's own writes never retrigger it.
// Reviews are never the bot's, so only bot accounts' reviews are left out.
// Review bots (review_bots) count like people: their findings can change
// the PR's status.
func Fingerprint(c *Config, self string, pr *PR, comments []Comment, reviews []Review) string {
	ignore := func(u User) bool { return IsBot(u.Login, u.Type) && !c.IsReviewBot(u.Login) }
	h := sha256.New()
	fmt.Fprintf(h, "sha=%s\ntitle=%s\nbody=%s\ndraft=%t\nbase=%s\n", pr.Head.SHA, pr.Title, pr.Body, pr.Draft, pr.Base.Ref)
	var ids []string
	for _, cm := range comments {
		if IsOwn(self, cm) || ignore(cm.User) {
			continue
		}
		ids = append(ids, fmt.Sprintf("c%d", cm.ID))
	}
	for _, r := range reviews {
		if ignore(r.User) {
			continue
		}
		ids = append(ids, fmt.Sprintf("r%d", r.ID))
	}
	for _, l := range pr.LabelNames() {
		if containsFold(c.ExemptLabels, l) {
			ids = append(ids, "l:"+l)
		}
	}
	sort.Strings(ids)
	fmt.Fprintf(h, "%s\n", strings.Join(ids, ","))
	return FingerprintVersion + ":" + hex.EncodeToString(h.Sum(nil))[:32]
}

// Combine joins the activity fingerprint with the CI and conflict state.
// The store keeps the two apart, because the poll refreshes CI on every PR
// but activity only on PRs whose updated_at moved.
func Combine(activity string, m MergeState) string { return activity + "|" + m.Key() }

// Fingerprint of a full snapshot, including CI and conflicts.
func (s *Snapshot) Fingerprint(c *Config) string {
	all := append(append([]Comment{}, s.IssueComments...), s.ReviewComments...)
	return Combine(Fingerprint(c, s.Self, s.PR, all, s.Reviews), s.Merge)
}

// DiffText returns the changed files and their patches, up to limit bytes,
// and whether everything fit. A file GitHub sends without a patch (binary
// or too large) counts as not fitting.
func (s *Snapshot) DiffText(limit int) (string, bool) {
	var b strings.Builder
	complete := len(s.Files) < 300 && len(s.Files) == s.PR.ChangedFiles
	for _, f := range s.Files {
		fmt.Fprintf(&b, "--- %s (%s, +%d -%d)\n", f.Filename, f.Status, f.Additions, f.Deletions)
		if f.Patch == "" && f.Status != "removed" && f.Additions+f.Deletions > 0 {
			complete = false
			b.WriteString("[no patch available]\n")
		}
		if b.Len()+len(f.Patch) > limit {
			complete = false
			continue
		}
		b.WriteString(f.Patch)
		b.WriteString("\n")
	}
	out := b.String()
	if len(out) > limit {
		out, complete = out[:limit], false
	}
	return out, complete
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n[truncated]"
}
