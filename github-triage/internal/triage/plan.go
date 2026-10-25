package triage

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Close reasons.
const (
	ClosePromotional   = "promotional"
	CloseNoLinkedIssue = "no_linked_issue"
)

// Action kinds the store records and the completion hook checks.
const (
	ActionLabels  = "labels"
	ActionClose   = "close"
	ActionApprove = "approve_ci"
	ActionNudge   = "nudge"
	ActionDupe    = "flag_duplicate"
	ActionFinish  = "finish"
)

// Plan is what the policy requires for one PR at one head commit. The tools
// refuse any close, CI approval or nudge the plan does not contain.
type Plan struct {
	Repo        string `json:"repo"`
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	HeadSHA     string `json:"head_sha"`
	Fingerprint string `json:"fingerprint"`
	Draft       bool   `json:"draft"`
	DryRun      bool   `json:"dry_run"`

	Exempt string `json:"exempt,omitempty"`

	// HostLabels are the status and triage labels the policy sets.
	HostLabels []string `json:"host_labels"`
	// ProposedType is the decision model's type label; CurrentType is the
	// type label already on the PR, if any.
	ProposedType string             `json:"proposed_type"`
	CurrentType  string             `json:"current_type,omitempty"`
	TypeScores   map[string]float64 `json:"type_scores,omitempty"`
	CurrentAreas []string           `json:"current_areas,omitempty"`

	// Status is RoleAwaitingAuthor, RoleAwaitingReview, RoleReadyForReview
	// or empty (draft).
	Status        string `json:"status,omitempty"`
	AwaitingSince string `json:"awaiting_since,omitempty"`
	Nudge         bool   `json:"nudge,omitempty"`
	// LastNudge is when the bot last nudged for the current wait, if it has.
	LastNudge string `json:"last_nudge,omitempty"`

	Close        string `json:"close,omitempty"`
	CloseComment string `json:"close_comment,omitempty"`

	LinkedIssues []string `json:"linked_issues,omitempty"`

	// DuplicateOf lists older or merged PRs this one likely duplicates.
	DuplicateOf      []int  `json:"duplicate_of,omitempty"`
	DuplicateComment string `json:"duplicate_comment,omitempty"`

	// CI is the head commit's checks and mergeability.
	CI MergeState `json:"ci_state"`

	PendingRuns []int64 `json:"pending_ci_runs,omitempty"`
	ApproveRuns []int64 `json:"approve_ci_runs,omitempty"`

	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Reasons       []string           `json:"reasons"`
}

// Required lists the actions the run must record before it may finish.
func (p *Plan) Required() []string {
	req := []string{ActionLabels}
	if p.Close != "" {
		req = append(req, ActionClose)
	}
	if len(p.ApproveRuns) > 0 {
		req = append(req, ActionApprove)
	}
	if p.Nudge {
		req = append(req, ActionNudge)
	}
	if p.DuplicateComment != "" {
		req = append(req, ActionDupe)
	}
	return req
}

// DecideFunc is RunScope.Decide.
type DecideFunc func(context.Context, bonnie.DecisionRequest) (*kit.DecisionResult, error)

// Analyze turns a snapshot into a plan. keyPrefix makes each decision call
// idempotent for the tool call that asked it, so a recovered turn gets the
// journalled answer instead of a fresh one.
func Analyze(ctx context.Context, c *Config, s *Snapshot, decide DecideFunc, keyPrefix string) (*Plan, error) {
	if decide == nil {
		return nil, errors.New("triage: no decision model is configured")
	}
	pr := s.PR
	p := &Plan{
		Repo: c.Repo, Number: pr.Number, Title: pr.Title, Author: pr.User.Login,
		HeadSHA: pr.Head.SHA, Fingerprint: s.Fingerprint(c), Draft: pr.Draft, DryRun: c.DryRun,
		Probabilities: map[string]float64{}, CI: s.Merge,
	}
	for _, l := range pr.LabelNames() {
		if lab, ok := c.Label(l); ok {
			switch lab.Group {
			case GroupType:
				p.CurrentType = l
			case GroupArea:
				p.CurrentAreas = append(p.CurrentAreas, l)
			}
		}
	}
	for _, r := range s.PendingRuns {
		p.PendingRuns = append(p.PendingRuns, r.ID)
	}

	switch {
	case IsBot(pr.User.Login, pr.User.Type):
		p.Exempt = "author is a bot"
	case (c.ExemptMaintainers == nil || *c.ExemptMaintainers) && c.IsMaintainer(pr.User.Login, pr.AuthorAssociation):
		p.Exempt = "author is a maintainer"
	case containsFold(c.ExemptAuthors, pr.User.Login):
		p.Exempt = "author is in exempt_authors"
	default:
		for _, l := range pr.LabelNames() {
			if containsFold(c.ExemptLabels, l) {
				p.Exempt = "PR has exempt label " + l
			}
		}
	}

	if err := classify(ctx, c, s, decide, keyPrefix, p); err != nil {
		return nil, err
	}
	if p.Close == "" && p.Exempt == "" && c.LinkedIssue.Enabled {
		if err := linkedIssue(ctx, c, s, p); err != nil {
			return nil, err
		}
	}
	if err := awaiting(ctx, c, s, decide, keyPrefix, p); err != nil {
		return nil, err
	}
	if err := ciGate(ctx, c, s, decide, keyPrefix, p); err != nil {
		return nil, err
	}
	if c.Duplicates.Enabled && p.Close == "" && !IsBot(pr.User.Login, pr.User.Type) {
		if err := duplicates(ctx, c, s, decide, keyPrefix, p); err != nil {
			return nil, err
		}
	}

	if p.Status != "" && p.Close == "" {
		p.HostLabels = append(p.HostLabels, c.RoleLabel(p.Status))
	}
	switch p.Close {
	case ClosePromotional:
		p.HostLabels = append(p.HostLabels, c.RoleLabel(RoleClosedPromotional))
		p.CloseComment = strings.TrimSpace(c.Promotional.Comment) + "\n\n" + MarkerClosePromotional
	case CloseNoLinkedIssue:
		p.HostLabels = append(p.HostLabels, c.RoleLabel(RoleClosedNoIssue))
		p.CloseComment = strings.TrimSpace(c.LinkedIssue.Comment) + "\n\n" + MarkerCloseNoIssue
	}
	sort.Strings(p.HostLabels)
	return p, nil
}

// classify asks whether the PR is promotional and what type it is.
func classify(ctx context.Context, c *Config, s *Snapshot, decide DecideFunc, keyPrefix string, p *Plan) error {
	pr := s.PR
	diff, _ := s.DiffText(8000)
	state := fmt.Sprintf("Pull request to %s\nTitle: %s\nAuthor: %s (%s)\n\nDescription:\n%s\n\nChanged files and diff:\n%s",
		c.Repo, pr.Title, pr.User.Login, strings.ToLower(pr.AuthorAssociation), truncate(pr.Body, 4000), diff)

	var opts []kit.DecisionOption
	for _, l := range c.TypeLabels() {
		opts = append(opts, kit.DecisionOption{Name: l.Name, Description: l.Description})
	}
	questions := []kit.DecisionQuestion{
		kit.ChoiceQuestion("type", "Which kind of change is this pull request?", opts...),
	}
	if p.Exempt == "" {
		questions = append(questions, kit.BooleanQuestion("promotional",
			"Is this pull request promotional or spam rather than a genuine contribution? "+
				"Promotional means it mainly advertises a product, service, website, course, company or the author's own project, "+
				"adds marketing, affiliate, SEO or sponsor links, or makes a trivial or meaningless change to get a link or profile into the repository. "+
				"A real fix, feature, documentation improvement or dependency update is not promotional, even if it is small."))
	}
	res, err := decide(ctx, bonnie.DecisionRequest{
		State:     kit.DecisionState{Text: state},
		Questions: questions,
		Key:       key(keyPrefix, "classify"),
	})
	if err != nil {
		return fmt.Errorf("triage: classify decision: %w", err)
	}
	t, err := res.Choice("type")
	if err != nil {
		return fmt.Errorf("triage: classify decision: %w", err)
	}
	p.ProposedType = t.Choice
	p.TypeScores = map[string]float64{}
	for _, d := range t.Distribution {
		p.TypeScores[d.Name] = round(d.Probability)
	}
	if p.Exempt != "" {
		p.Reasons = append(p.Reasons, "Promotional and linked-issue rules skipped: "+p.Exempt+".")
		return nil
	}
	a, err := res.Boolean("promotional")
	if err != nil {
		return fmt.Errorf("triage: classify decision: %w", err)
	}
	p.Probabilities["promotional"] = round(a.Probability)
	switch {
	case a.Probability >= c.Promotional.CloseThreshold:
		p.Close = ClosePromotional
		p.Reasons = append(p.Reasons, fmt.Sprintf("P(promotional)=%.2f is at or above %.2f: close.", a.Probability, c.Promotional.CloseThreshold))
	case a.Probability >= c.Promotional.LabelThreshold:
		p.HostLabels = append(p.HostLabels, c.RoleLabel(RolePossibleSpam))
		p.Reasons = append(p.Reasons, fmt.Sprintf("P(promotional)=%.2f is borderline: label %s for a human.", a.Probability, c.RoleLabel(RolePossibleSpam)))
	}
	return nil
}

var (
	reHashRef = regexp.MustCompile(`(?:^|[^\w/#&])#(\d{1,7})\b`)
	reRepoRef = regexp.MustCompile(`\b([\w.-]+/[\w.-]+)#(\d{1,7})\b`)
	reURLRef  = regexp.MustCompile(`https?://github\.com/([\w.-]+/[\w.-]+)/issues/(\d{1,7})\b`)
)

// linkedIssue applies the linked-issue rule.
func linkedIssue(ctx context.Context, c *Config, s *Snapshot, p *Plan) error {
	if s.PR.Draft {
		p.Reasons = append(p.Reasons, "Linked-issue rule waits until the PR is ready for review.")
		return nil
	}
	for _, li := range s.Graph.ClosingIssues {
		p.LinkedIssues = append(p.LinkedIssues, li.Repository.NameWithOwner+"#"+itoa(li.Number))
	}
	if len(p.LinkedIssues) == 0 && c.LinkedIssue.AcceptMentions {
		refs, err := mentionedIssues(ctx, c, s)
		if err != nil {
			return err
		}
		p.LinkedIssues = refs
	}
	if len(p.LinkedIssues) > 0 {
		p.Reasons = append(p.Reasons, "Linked issue: "+strings.Join(p.LinkedIssues, ", ")+".")
		return nil
	}
	p.Close = CloseNoLinkedIssue
	p.Reasons = append(p.Reasons, "No linked or referenced issue: close (linked_issue.enabled).")
	return nil
}

// mentionedIssues finds issue references in the PR body. Same-repository
// numbers are checked to be issues, not PRs; other repositories count only
// as explicit /issues/ URLs or owner/name#n references.
func mentionedIssues(ctx context.Context, c *Config, s *Snapshot) ([]string, error) {
	body := s.PR.Body
	seen := map[string]bool{}
	var local []int
	var out []string
	add := func(repo string, n int) {
		key := repo + "#" + itoa(n)
		if seen[key] {
			return
		}
		seen[key] = true
		if strings.EqualFold(repo, c.Repo) {
			local = append(local, n)
		} else {
			out = append(out, key)
		}
	}
	for _, m := range reURLRef.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(m[2])
		add(m[1], n)
	}
	for _, m := range reRepoRef.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(m[2])
		add(m[1], n)
	}
	for _, m := range reHashRef.FindAllStringSubmatch(body, -1) {
		n, _ := strconv.Atoi(m[1])
		add(c.Repo, n)
	}
	_, g, err := Client()
	if err != nil {
		return nil, err
	}
	for i, n := range local {
		if i >= 5 || n == s.PR.Number {
			continue
		}
		ok, err := g.IsIssue(ctx, n)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c.Repo+"#"+itoa(n))
		}
	}
	return out, nil
}

// awaiting sets the PR's status. It waits on its author when it has merge
// conflicts, failing CI, changes requested, or a maintainer or review-bot
// request the author has not answered. Otherwise it waits on the
// maintainers: "awaiting review" when the author has answered since the
// last maintainer message or CI is not green yet, and "ready for review"
// (if that role is configured) when CI is green, it merges cleanly and
// nothing is open. It also decides whether a nudge is due.
func awaiting(ctx context.Context, c *Config, s *Snapshot, decide DecideFunc, keyPrefix string, p *Plan) error {
	pr := s.PR
	if pr.Draft {
		p.Reasons = append(p.Reasons, "Draft: no status label.")
		return nil
	}
	author := pr.User.Login
	isMaint := func(login, assoc string) bool {
		return login != author && !IsBot(login, "") && c.IsMaintainer(login, assoc)
	}

	// The author's latest activity: opening the PR, a commit, a comment or a review.
	since := pr.CreatedAt
	later := func(t time.Time) {
		if t.After(since) {
			since = t
		}
	}
	later(s.Graph.LastCommitAt)
	for _, cm := range append(append([]Comment{}, s.IssueComments...), s.ReviewComments...) {
		if cm.User.Login == author {
			later(cm.CreatedAt)
		}
	}
	for _, r := range s.Reviews {
		if r.User.Login == author {
			later(r.SubmittedAt)
		}
	}

	// first is when the current wait on the author began.
	var first time.Time
	waitFrom := func(t time.Time) {
		if !t.IsZero() && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}

	// Maintainer messages after the author's latest activity, and the time
	// of the latest maintainer message at all.
	var msgs []string
	var lastMaint time.Time
	changesRequested := false
	note := func(t time.Time, who, kind, body string) {
		waitFrom(t)
		msgs = append(msgs, fmt.Sprintf("[%s] %s (%s):\n%s", t.Format(time.RFC3339), who, kind, truncate(body, 2000)))
	}
	seenMaint := func(t time.Time) {
		if t.After(lastMaint) {
			lastMaint = t
		}
	}
	for _, cm := range s.IssueComments {
		if IsOwn(s.Self, cm) || !isMaint(cm.User.Login, cm.AuthorAssociation) {
			continue
		}
		seenMaint(cm.CreatedAt)
		if cm.CreatedAt.After(since) {
			note(cm.CreatedAt, cm.User.Login, "maintainer comment", cm.Body)
		}
	}
	for _, r := range s.Reviews {
		if !isMaint(r.User.Login, r.AuthorAssociation) {
			continue
		}
		seenMaint(r.SubmittedAt)
		if !r.SubmittedAt.After(since) {
			continue
		}
		if r.State == "CHANGES_REQUESTED" {
			changesRequested = true
		}
		if strings.TrimSpace(r.Body) != "" || r.State == "CHANGES_REQUESTED" {
			note(r.SubmittedAt, r.User.Login, "maintainer review "+strings.ToLower(r.State), r.Body)
		}
	}
	botThreads := 0
	for _, th := range s.Graph.Threads {
		nodes := th.Comments.Nodes
		if th.IsResolved || len(nodes) == 0 {
			continue
		}
		head, last := nodes[0], nodes[len(nodes)-1]
		switch {
		case last.Author != nil && isMaint(last.Author.Login, last.AuthorAssociation):
			seenMaint(last.CreatedAt)
			if last.CreatedAt.After(since) {
				note(last.CreatedAt, last.Author.Login, "unresolved maintainer review thread on "+th.Path, last.Body)
			}
		case head.Author != nil && c.IsReviewBot(head.Author.Login) && !th.IsOutdated &&
			(last.Author == nil || last.Author.Login != author):
			// An automated finding the author has neither answered nor
			// made outdated with a new commit on those lines.
			botThreads++
			note(head.CreatedAt, head.Author.Login, "unresolved automated review thread on "+th.Path, head.Body)
		}
	}

	var blocking []string
	if s.Merge.Conflict {
		blocking = append(blocking, "it has merge conflicts with the base branch")
		waitFrom(s.PrevAwaitingSince)
		waitFrom(time.Now())
	}
	if s.Merge.CI == CIFailure {
		blocking = append(blocking, "CI is failing: "+strings.Join(s.Merge.Failing, ", "))
		waitFrom(s.Merge.FailedAt)
	}

	waiting := len(blocking) > 0
	for _, b := range blocking {
		p.Reasons = append(p.Reasons, "Waiting on the author: "+b+".")
	}
	switch {
	case changesRequested:
		waiting = true
		p.Probabilities["awaiting_author"] = 1
		p.Reasons = append(p.Reasons, "A maintainer requested changes after the author's last activity.")
	case len(msgs) > 0:
		state := fmt.Sprintf("Pull request #%d by %s: %s\n\nThe author's last activity was at %s. "+
			"Open messages from maintainers and automated reviewers:\n\n%s",
			pr.Number, author, pr.Title, since.Format(time.RFC3339), strings.Join(msgs, "\n\n"))
		res, err := decide(ctx, bonnie.DecisionRequest{
			State: kit.DecisionState{Text: truncate(state, 24000)},
			Questions: []kit.DecisionQuestion{kit.BooleanQuestion("awaiting_author",
				"Do these messages ask the pull request author a question, ask them to make a change, or point out a problem "+
					"they still have to fix, so that the next step is the author's? Approval, thanks, optional nitpicks, "+
					"or notes between maintainers are not.")},
			Key: key(keyPrefix, "awaiting"),
		})
		if err != nil {
			return fmt.Errorf("triage: awaiting-author decision: %w", err)
		}
		a, err := res.Boolean("awaiting_author")
		if err != nil {
			return fmt.Errorf("triage: awaiting-author decision: %w", err)
		}
		p.Probabilities["awaiting_author"] = round(a.Probability)
		if a.Probability >= c.AwaitingAuthor.Threshold {
			waiting = true
		}
		p.Reasons = append(p.Reasons, fmt.Sprintf("P(awaiting author)=%.2f over %d open message(s), %d from review bots.",
			a.Probability, len(msgs), botThreads))
	}

	if !waiting {
		ready := c.RoleLabel(RoleReadyForReview) != ""
		switch {
		case !lastMaint.IsZero() && since.After(lastMaint):
			p.Status = RoleAwaitingReview
			p.Reasons = append(p.Reasons, "The author responded after the last maintainer message.")
		case ready && s.Merge.Green():
			p.Status = RoleReadyForReview
			p.Reasons = append(p.Reasons, "CI is green, it merges cleanly and nothing is open.")
		default:
			p.Status = RoleAwaitingReview
			if ready {
				p.Reasons = append(p.Reasons, fmt.Sprintf("Not ready for review yet: CI is %s.", s.Merge.CI))
			}
		}
		return nil
	}
	p.Status = RoleAwaitingAuthor
	if first.IsZero() {
		first = time.Now()
	}
	p.AwaitingSince = first.UTC().Format(time.RFC3339)
	days := c.AwaitingAuthor.NudgeAfterDays
	if days <= 0 || time.Since(first) < time.Duration(days)*24*time.Hour {
		return nil
	}
	for _, cm := range s.IssueComments {
		if cm.User.Login == s.Self && strings.Contains(cm.Body, MarkerNudge) && cm.CreatedAt.After(first) {
			p.LastNudge = cm.CreatedAt.UTC().Format(time.RFC3339)
			return nil // already nudged for this wait
		}
	}
	p.Nudge = true
	p.Reasons = append(p.Reasons, fmt.Sprintf("Waiting on the author for more than %d days: nudge once.", days))
	return nil
}

// ciGate decides which pending workflow runs to approve.
func ciGate(ctx context.Context, c *Config, s *Snapshot, decide DecideFunc, keyPrefix string, p *Plan) error {
	if len(s.PendingRuns) == 0 || p.Close != "" {
		return nil
	}
	needsReview := func(why string) error {
		p.HostLabels = append(p.HostLabels, c.RoleLabel(RoleCINeedsReview))
		p.Reasons = append(p.Reasons, "CI waits for approval and was not auto-approved: "+why)
		return nil
	}
	if !c.CI.AutoApprove {
		return needsReview("ci.auto_approve is off.")
	}
	var blocked []string
	for _, f := range s.Files {
		if c.NeverApprove(f.Filename) {
			blocked = append(blocked, f.Filename)
		}
	}
	if len(blocked) > 0 {
		return needsReview("touches " + strings.Join(blocked, ", ") + " (ci.never_approve_paths).")
	}
	diff, complete := s.DiffText(c.CI.MaxDiffBytes)
	if !complete {
		return needsReview(fmt.Sprintf("the diff is larger than ci.max_diff_bytes (%d) or has files without patches.", c.CI.MaxDiffBytes))
	}
	state := fmt.Sprintf("A pull request from an outside contributor wants to run this repository's CI on its code.\n"+
		"Title: %s\nAuthor: %s\n\nDescription:\n%s\n\nComplete diff:\n%s",
		s.PR.Title, s.PR.User.Login, truncate(s.PR.Body, 2000), diff)
	res, err := decide(ctx, bonnie.DecisionRequest{
		State: kit.DecisionState{Text: state},
		Questions: []kit.DecisionQuestion{
			kit.BooleanQuestion("harmful",
				"Could running this change in CI be harmful? For example: reading or sending secrets, tokens or environment variables; "+
					"network calls to unknown hosts; downloading and running remote code; crypto mining; changing build, install or test "+
					"scripts to run unexpected commands; or abusing the CI runner."),
			kit.BooleanQuestion("obfuscated",
				"Does the change contain obfuscated, encoded, minified or otherwise hard-to-read code or data whose purpose is unclear?"),
		},
		Key: key(keyPrefix, "ci"),
	})
	if err != nil {
		// Fail closed: no answer, no approval.
		return needsReview("the safety decision failed: " + err.Error())
	}
	harm, err1 := res.Boolean("harmful")
	obf, err2 := res.Boolean("obfuscated")
	if err := errors.Join(err1, err2); err != nil {
		return needsReview("the safety decision failed: " + err.Error())
	}
	p.Probabilities["ci_harmful"] = round(harm.Probability)
	p.Probabilities["ci_obfuscated"] = round(obf.Probability)
	if harm.Probability >= c.CI.MaxRisk || obf.Probability >= c.CI.MaxRisk {
		return needsReview(fmt.Sprintf("P(harmful)=%.2f, P(obfuscated)=%.2f, limit %.2f.", harm.Probability, obf.Probability, c.CI.MaxRisk))
	}
	p.ApproveRuns = append(p.ApproveRuns, p.PendingRuns...)
	p.Reasons = append(p.Reasons, fmt.Sprintf("CI judged safe (P(harmful)=%.2f, P(obfuscated)=%.2f): approve %d run(s).",
		harm.Probability, obf.Probability, len(p.ApproveRuns)))
	return nil
}

// key builds a decision key; with no prefix the call is not keyed.
func key(prefix, name string) string {
	if prefix == "" {
		return ""
	}
	return prefix + ":" + name
}

func round(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }
