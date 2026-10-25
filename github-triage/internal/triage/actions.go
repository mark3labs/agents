package triage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// The functions here back the agent's tools. Each one re-reads the run's
// stored plan and refuses anything the plan does not allow, so the model
// cannot close, approve or nudge on its own say-so. Every write is checked
// against GitHub first, so a repeated call after a crash does not repeat it.

// errResult is a tool error the model can read and act on.
type errResult string

func (e errResult) Error() string { return string(e) }

// ToolResult turns a helper's outcome into a tool output.
func ToolResult(text string, err error) (kit.ToolOutput, error) {
	var er errResult
	if errors.As(err, &er) {
		return kit.ErrorResult(string(er)), nil
	}
	if err != nil {
		return kit.ToolOutput{}, err
	}
	return kit.TextResult(text), nil
}

type runCtx struct {
	c    *Config
	g    *GitHub
	st   *Store
	plan *Plan
}

func open(ctx context.Context, runID string, pr int, needPlan bool) (*runCtx, error) {
	c, g, err := Client()
	if err != nil {
		return nil, err
	}
	st, err := OpenStore(c)
	if err != nil {
		return nil, err
	}
	rc := &runCtx{c: c, g: g, st: st}
	if !needPlan {
		return rc, nil
	}
	if rc.plan, err = st.Plan(ctx, runID); err != nil {
		return nil, err
	}
	if rc.plan == nil {
		return nil, errResult("call inspect_pr first; this run has no plan")
	}
	if rc.plan.Number != pr {
		return nil, errResult(fmt.Sprintf("this run triages PR #%d, not #%d", rc.plan.Number, pr))
	}
	return rc, nil
}

// Inspect fetches a PR, runs the policy and its decisions, stores the plan
// for the run and returns it as JSON for the model.
func Inspect(ctx context.Context, scope bonnie.ToolScope, pr int) (string, error) {
	rc, err := open(ctx, scope.RunID, pr, false)
	if err != nil {
		return "", err
	}
	if !rc.c.DryRun {
		if err := rc.g.EnsureLabels(ctx, rc.c); err != nil {
			return "", fmt.Errorf("triage: sync labels: %w", err)
		}
	}
	snap, err := Fetch(ctx, rc.c, rc.g, pr)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 404 {
			return "", errResult(fmt.Sprintf("PR #%d does not exist in %s", pr, rc.c.Repo))
		}
		return "", err
	}
	if snap.PR.State != "open" {
		return "", errResult(fmt.Sprintf("PR #%d is %s; there is nothing to triage. Report that and stop; finish_triage is not needed.", pr, snap.PR.State))
	}
	if row, err := rc.st.PR(ctx, rc.c.Repo, pr); err == nil && row.Status == RoleAwaitingAuthor {
		snap.PrevAwaitingSince, _ = time.Parse(time.RFC3339, row.AwaitingSince)
	}
	var decide DecideFunc
	if scope.Decide != nil {
		decide = scope.Decide
	}
	prefix := ""
	if id := kit.ToolCallIDFromContext(ctx); id != "" {
		prefix = "inspect:" + id
	}
	plan, err := Analyze(ctx, rc.c, snap, decide, prefix)
	if err != nil {
		return "", err
	}
	if err := rc.st.SavePlan(ctx, scope.RunID, plan); err != nil {
		return "", err
	}
	type view struct {
		*Plan
		Body          string   `json:"body"`
		Files         []string `json:"changed_files"`
		CurrentLabels []string `json:"current_labels"`
		AreaChoices   []Label  `json:"area_label_choices,omitempty"`
		TypeChoices   []string `json:"type_label_choices"`
		Required      []string `json:"required_actions"`
	}
	v := view{Plan: plan, Body: truncate(snap.PR.Body, 3000), CurrentLabels: snap.PR.LabelNames(),
		AreaChoices: rc.c.AreaLabels(), Required: plan.Required()}
	for _, f := range snap.Files {
		if len(v.Files) == 50 {
			v.Files = append(v.Files, fmt.Sprintf("... and %d more", len(snap.Files)-50))
			break
		}
		v.Files = append(v.Files, f.Filename)
	}
	for _, l := range rc.c.TypeLabels() {
		v.TypeChoices = append(v.TypeChoices, l.Name)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	return string(data), err
}

// SetLabels applies the plan's host labels plus the agent's type and area
// choices, and removes managed labels that no longer apply. Area labels are
// only ever added: one a human put on stays.
func SetLabels(ctx context.Context, runID string, pr int, typ string, areas []string) (string, error) {
	rc, err := open(ctx, runID, pr, true)
	if err != nil {
		return "", err
	}
	c, p := rc.c, rc.plan
	if typ == "" {
		typ = p.ProposedType
	}
	if l, ok := c.Label(typ); !ok || l.Group != GroupType {
		return "", errResult(fmt.Sprintf("%q is not a type label; choose one of the type_label_choices", typ))
	}
	want := append(slices.Clone(p.HostLabels), typ)
	for _, a := range areas {
		if l, ok := c.Label(a); !ok || l.Group != GroupArea {
			return "", errResult(fmt.Sprintf("%q is not an area label; choose from area_label_choices", a))
		}
		want = append(want, a)
	}
	// Re-read the labels: a human may have changed them since inspect_pr.
	cur, err := rc.g.GetPR(ctx, pr)
	if err != nil {
		return "", err
	}
	have := cur.LabelNames()
	var add, remove []string
	for _, w := range want {
		if !slices.Contains(have, w) && !slices.Contains(add, w) {
			add = append(add, w)
		}
	}
	for _, h := range have {
		l, ok := c.Label(h)
		// Area labels and the duplicate label are only ever added: a human
		// may have put them on.
		if !ok || !c.Managed(h) || l.Group == GroupArea || l.Role == RoleDuplicate || slices.Contains(want, h) {
			continue
		}
		remove = append(remove, h)
	}
	detail := fmt.Sprintf("add %v, remove %v", add, remove)
	if !c.DryRun {
		if err := rc.g.AddLabels(ctx, pr, add); err != nil {
			return "", err
		}
		for _, r := range remove {
			if err := rc.g.RemoveLabel(ctx, pr, r); err != nil {
				return "", err
			}
		}
	}
	if err := rc.st.RecordAction(ctx, runID, ActionLabels, detail, c.DryRun); err != nil {
		return "", err
	}
	return dry(c, "Labels on #%d: %s.", pr, detail), nil
}

// ClosePR posts the configured close comment, plus an optional note from
// the agent, and closes the PR. The plan must call for this close.
func ClosePR(ctx context.Context, runID string, pr int, reason, note string) (string, error) {
	rc, err := open(ctx, runID, pr, true)
	if err != nil {
		return "", err
	}
	c, p := rc.c, rc.plan
	if p.Close == "" {
		return "", errResult("the plan does not close this PR; closing is refused")
	}
	if reason != p.Close {
		return "", errResult(fmt.Sprintf("the plan closes this PR as %q, not %q", p.Close, reason))
	}
	if len(note) > 1000 {
		return "", errResult("note must be at most 1000 characters")
	}
	body := p.CloseComment
	if strings.TrimSpace(note) != "" {
		body = strings.TrimSpace(note) + "\n\n" + body
	}
	marker := MarkerClosePromotional
	if p.Close == CloseNoLinkedIssue {
		marker = MarkerCloseNoIssue
	}
	if !c.DryRun {
		if err := rc.commentOnce(ctx, pr, marker, body); err != nil {
			return "", err
		}
		if err := rc.g.Close(ctx, pr); err != nil {
			return "", err
		}
	}
	if err := rc.st.RecordAction(ctx, runID, ActionClose, reason, c.DryRun); err != nil {
		return "", err
	}
	return dry(c, "Closed #%d (%s) with comment:\n%s", pr, reason, body), nil
}

// ApproveCI approves the plan's pending workflow runs, after checking each
// one still waits for approval on the same head commit.
func ApproveCI(ctx context.Context, runID string, pr int) (string, error) {
	rc, err := open(ctx, runID, pr, true)
	if err != nil {
		return "", err
	}
	c, p := rc.c, rc.plan
	if len(p.ApproveRuns) == 0 {
		return "", errResult("the plan approves no CI runs; approval is refused")
	}
	cur, err := rc.g.GetPR(ctx, pr)
	if err != nil {
		return "", err
	}
	if cur.Head.SHA != p.HeadSHA {
		return "", errResult("new commits arrived since inspect_pr; call inspect_pr again before approving")
	}
	var done []string
	for _, id := range p.ApproveRuns {
		run, err := rc.g.GetRun(ctx, id)
		if err != nil {
			return "", err
		}
		if run.HeadSHA != p.HeadSHA || run.Conclusion != "action_required" && run.Status != "action_required" {
			done = append(done, fmt.Sprintf("%d (%s, skipped: no longer waiting)", id, run.Name))
			continue
		}
		if !c.DryRun {
			if err := rc.g.ApproveRun(ctx, id); err != nil {
				return "", err
			}
		}
		done = append(done, fmt.Sprintf("%d (%s)", id, run.Name))
	}
	detail := strings.Join(done, ", ")
	if err := rc.st.RecordAction(ctx, runID, ActionApprove, detail, c.DryRun); err != nil {
		return "", err
	}
	return dry(c, "Approved CI runs on #%d: %s.", pr, detail), nil
}

// Nudge posts one reminder to the PR author. The agent writes the message;
// the plan must call for a nudge.
func Nudge(ctx context.Context, runID string, pr int, message string) (string, error) {
	rc, err := open(ctx, runID, pr, true)
	if err != nil {
		return "", err
	}
	c, p := rc.c, rc.plan
	if !p.Nudge {
		return "", errResult("the plan does not call for a nudge; refused")
	}
	message = strings.TrimSpace(message)
	if message == "" || len(message) > 1500 {
		return "", errResult("message must be 1 to 1500 characters")
	}
	if !strings.Contains(message, "@"+p.Author) {
		message = "@" + p.Author + " " + message
	}
	body := message + "\n\n" + MarkerNudge
	if !c.DryRun {
		if err := rc.commentOnce(ctx, pr, MarkerNudge, body); err != nil {
			return "", err
		}
	}
	if err := rc.st.RecordAction(ctx, runID, ActionNudge, message, c.DryRun); err != nil {
		return "", err
	}
	return dry(c, "Nudged #%d:\n%s", pr, message), nil
}

// FlagDuplicate posts the configured possible-duplicate comment, with an
// optional note from the agent. The plan must have found duplicates.
func FlagDuplicate(ctx context.Context, runID string, pr int, note string) (string, error) {
	rc, err := open(ctx, runID, pr, true)
	if err != nil {
		return "", err
	}
	c, p := rc.c, rc.plan
	if p.DuplicateComment == "" {
		return "", errResult("the plan flags no duplicate comment; refused")
	}
	if len(note) > 800 {
		return "", errResult("note must be at most 800 characters")
	}
	body := p.DuplicateComment
	if n := strings.TrimSpace(note); n != "" {
		body = n + "\n\n" + body
	}
	if !c.DryRun {
		if err := rc.commentOnce(ctx, pr, MarkerDuplicate, body); err != nil {
			return "", err
		}
	}
	if err := rc.st.RecordAction(ctx, runID, ActionDupe, fmt.Sprint(p.DuplicateOf), c.DryRun); err != nil {
		return "", err
	}
	return dry(c, "Flagged #%d as a possible duplicate of %v:\n%s", pr, p.DuplicateOf, body), nil
}

// Finish records the triage as done once every required action is recorded.
func Finish(ctx context.Context, runID string, pr int, summary string) (string, error) {
	rc, err := open(ctx, runID, pr, true)
	if err != nil {
		return "", err
	}
	missing, err := Missing(ctx, rc.st, runID, rc.plan)
	if err != nil {
		return "", err
	}
	if len(missing) > 0 {
		return "", errResult("required actions not done yet: " + strings.Join(missing, ", "))
	}
	acts, err := rc.st.Actions(ctx, runID)
	if err != nil {
		return "", err
	}
	summary = truncate(strings.TrimSpace(summary), 2000)
	if err := rc.st.FinishTriage(ctx, rc.plan, summary, rc.c.DryRun, acts[ActionNudge]); err != nil {
		return "", err
	}
	if err := rc.st.RecordAction(ctx, runID, ActionFinish, summary, rc.c.DryRun); err != nil {
		return "", err
	}
	return dry(rc.c, "Triage of #%d recorded.", pr), nil
}

// Missing returns the plan's required actions that the run has not recorded.
func Missing(ctx context.Context, st *Store, runID string, p *Plan) ([]string, error) {
	acts, err := st.Actions(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range p.Required() {
		if !acts[r] {
			out = append(out, r)
		}
	}
	return out, nil
}

// commentOnce posts body unless the bot already left a comment with marker
// since the PR's head commit was checked, so a retried action does not post
// twice.
func (rc *runCtx) commentOnce(ctx context.Context, pr int, marker, body string) error {
	self, err := rc.g.Self(ctx)
	if err != nil {
		return err
	}
	comments, err := rc.g.IssueComments(ctx, pr)
	if err != nil {
		return err
	}
	for _, cm := range comments {
		if cm.User.Login == self && strings.Contains(cm.Body, marker) {
			if marker != MarkerNudge || rc.plan.AwaitingSince == "" || cm.CreatedAt.UTC().Format(time.RFC3339) >= rc.plan.AwaitingSince {
				return nil
			}
		}
	}
	return rc.g.Comment(ctx, pr, body)
}

func dry(c *Config, format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	if c.DryRun {
		return "DRY RUN, nothing was written. Would have done: " + msg
	}
	return msg
}
