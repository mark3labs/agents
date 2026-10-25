package triage

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

func testConfig(t *testing.T) *Config {
	t.Helper()
	data, err := os.ReadFile("../../triage.yaml")
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "triage.yaml")
	data = []byte(strings.Replace(string(data), "repo: owner/name", "repo: acme/widgets", 1))
	if err := os.WriteFile(f, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_REPO", "")
	t.Setenv("TRIAGE_DRY_RUN", "")
	c, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// fakeDecide answers by question ID and records the questions it saw.
type fakeDecide struct {
	p     map[string]float64
	typ   string
	seen  []string
	calls int
	state string
}

func (f *fakeDecide) decide(_ context.Context, req bonnie.DecisionRequest) (*kit.DecisionResult, error) {
	f.calls++
	f.state = req.State.Text
	res := &kit.DecisionResult{}
	for _, q := range req.Questions {
		f.seen = append(f.seen, q.ID)
		a := kit.DecisionAnswer{ID: q.ID, Type: q.Type, Probability: f.p[q.ID]}
		if q.Type == kit.DecisionChoice {
			a.Choice = f.typ
		}
		res.Answers = append(res.Answers, a)
	}
	return res, nil
}

func snapshot(mut func(s *Snapshot)) *Snapshot {
	created := time.Now().Add(-30 * 24 * time.Hour)
	s := &Snapshot{
		Self: "triage-bot",
		PR: &PR{Number: 7, Title: "Fix crash on empty list", Body: "Fixes the crash.", State: "open",
			User: User{Login: "alice", Type: "User"}, AuthorAssociation: "CONTRIBUTOR",
			CreatedAt: created, Additions: 20, Deletions: 5, ChangedFiles: 1},
		Files: []File{{Filename: "store/list.go", Status: "modified", Additions: 20, Deletions: 5, Patch: "@@ -1 +1 @@\n-a\n+b"}},
		Graph: &PRGraph{LastCommitAt: created,
			ClosingIssues: []LinkedIssue{{Number: 3, Repository: struct {
				NameWithOwner string `json:"nameWithOwner"`
			}{"acme/widgets"}}}},
	}
	s.PR.Head.SHA = "abc123"
	if mut != nil {
		mut(s)
	}
	return s
}

func TestCleanPRGetsLabelsOnly(t *testing.T) {
	c := testConfig(t)
	f := &fakeDecide{typ: "type: bug", p: map[string]float64{"promotional": 0.02}}
	p, err := Analyze(context.Background(), c, snapshot(nil), f.decide, "k")
	if err != nil {
		t.Fatal(err)
	}
	if p.Close != "" || p.Nudge || len(p.ApproveRuns) > 0 {
		t.Fatalf("unexpected actions: %+v", p)
	}
	if want := []string{"status: need maintainer response"}; !slices.Equal(p.HostLabels, want) {
		t.Fatalf("host labels %v, want %v", p.HostLabels, want)
	}
	if p.ProposedType != "type: bug" {
		t.Fatalf("type %q", p.ProposedType)
	}
}

func TestPromotionalThresholds(t *testing.T) {
	c := testConfig(t)
	for _, tc := range []struct {
		p         float64
		close     string
		spamLabel bool
	}{{0.95, ClosePromotional, false}, {0.7, "", true}, {0.3, "", false}} {
		f := &fakeDecide{typ: "type: documentation", p: map[string]float64{"promotional": tc.p}}
		p, err := Analyze(context.Background(), c, snapshot(nil), f.decide, "k")
		if err != nil {
			t.Fatal(err)
		}
		if p.Close != tc.close || slices.Contains(p.HostLabels, "triage: possible spam") != tc.spamLabel {
			t.Errorf("p=%.2f: close=%q labels=%v", tc.p, p.Close, p.HostLabels)
		}
		if tc.close != "" && (!strings.Contains(p.CloseComment, MarkerClosePromotional) || slices.Contains(p.HostLabels, "status: need maintainer response")) {
			t.Errorf("close plan wrong: %+v", p)
		}
	}
}

func TestNoLinkedIssueCloses(t *testing.T) {
	c := testConfig(t)
	c.LinkedIssue.AcceptMentions = false
	f := &fakeDecide{typ: "type: enhancement"}
	p, err := Analyze(context.Background(), c, snapshot(func(s *Snapshot) { s.Graph.ClosingIssues = nil }), f.decide, "k")
	if err != nil {
		t.Fatal(err)
	}
	if p.Close != CloseNoLinkedIssue || !slices.Contains(p.HostLabels, "triage: closed no issue") {
		t.Fatalf("want no-issue close, got %+v", p)
	}
	c.LinkedIssue.Enabled = false
	if p, _ = Analyze(context.Background(), c, snapshot(func(s *Snapshot) { s.Graph.ClosingIssues = nil }), f.decide, "k"); p.Close != "" {
		t.Fatalf("rule disabled but closed: %+v", p)
	}
}

func TestExemptAuthorsSkipCloseRules(t *testing.T) {
	c := testConfig(t)
	f := &fakeDecide{typ: "type: chore", p: map[string]float64{"promotional": 0.99}}
	p, err := Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		s.PR.User = User{Login: "dependabot[bot]", Type: "Bot"}
		s.Graph.ClosingIssues = nil
	}), f.decide, "k")
	if err != nil {
		t.Fatal(err)
	}
	if p.Close != "" || p.Exempt == "" || slices.Contains(f.seen, "promotional") {
		t.Fatalf("bot PR not exempt: %+v seen=%v", p, f.seen)
	}
}

func TestAwaitingAuthorAndNudge(t *testing.T) {
	c := testConfig(t)
	asked := time.Now().Add(-10 * 24 * time.Hour)
	withQuestion := func(s *Snapshot) {
		s.IssueComments = []Comment{{ID: 1, User: User{Login: "maint"}, AuthorAssociation: "MEMBER",
			Body: "Can you add a test for this?", CreatedAt: asked}}
	}
	f := &fakeDecide{typ: "type: bug", p: map[string]float64{"awaiting_author": 0.9}}
	p, err := Analyze(context.Background(), c, snapshot(withQuestion), f.decide, "k")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != RoleAwaitingAuthor || !p.Nudge {
		t.Fatalf("want awaiting author with nudge, got status=%q nudge=%v", p.Status, p.Nudge)
	}

	// The author answered afterwards: back to the maintainers, no decision needed.
	f = &fakeDecide{typ: "type: bug"}
	p, _ = Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		withQuestion(s)
		s.IssueComments = append(s.IssueComments, Comment{ID: 2, User: User{Login: "alice"}, Body: "Done", CreatedAt: asked.Add(time.Hour)})
	}), f.decide, "k")
	if p.Status != RoleAwaitingReview || slices.Contains(f.seen, "awaiting_author") {
		t.Fatalf("want awaiting review, got %q seen=%v", p.Status, f.seen)
	}

	// Already nudged since the question: no second nudge.
	f = &fakeDecide{typ: "type: bug", p: map[string]float64{"awaiting_author": 0.9}}
	p, _ = Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		withQuestion(s)
		s.IssueComments = append(s.IssueComments, Comment{ID: 3, User: User{Login: "triage-bot"}, Body: "ping " + MarkerNudge, CreatedAt: asked.Add(48 * time.Hour)})
	}), f.decide, "k")
	if p.Nudge || p.LastNudge == "" {
		t.Fatalf("nudged twice: %+v", p)
	}
}

func TestCIGate(t *testing.T) {
	c := testConfig(t)
	pending := func(s *Snapshot) { s.PendingRuns = []WorkflowRun{{ID: 99, HeadSHA: "abc123"}} }

	f := &fakeDecide{typ: "type: bug", p: map[string]float64{"ci_harmful": 0.01, "ci_obfuscated": 0.02}}
	p, _ := Analyze(context.Background(), c, snapshot(pending), f.decide, "k")
	if !slices.Equal(p.ApproveRuns, []int64{99}) {
		t.Fatalf("safe PR not approved: %+v", p)
	}

	f = &fakeDecide{typ: "type: bug", p: map[string]float64{"ci_harmful": 0.4}}
	p, _ = Analyze(context.Background(), c, snapshot(pending), f.decide, "k")
	if len(p.ApproveRuns) > 0 || !slices.Contains(p.HostLabels, "triage: ci needs review") {
		t.Fatalf("risky PR approved: %+v", p)
	}

	f = &fakeDecide{typ: "type: chore"}
	p, _ = Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		pending(s)
		s.Files = append(s.Files, File{Filename: ".github/workflows/ci.yml", Additions: 1, Patch: "+x"})
		s.PR.ChangedFiles = 2
	}), f.decide, "k")
	if len(p.ApproveRuns) > 0 || slices.Contains(f.seen, "ci_harmful") {
		t.Fatalf("workflow change approved or asked: %+v seen=%v", p, f.seen)
	}
}

func TestGlobAndMentions(t *testing.T) {
	for _, tc := range []struct {
		pat, name string
		want      bool
	}{
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", "src/.github", false},
		{"**/Makefile", "Makefile", true},
		{"**/Makefile", "a/b/Makefile", true},
		{"*.sh", "a/x.sh", false},
	} {
		if got := globMatch(tc.pat, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v", tc.pat, tc.name, got)
		}
	}
	body := "Closes #12, see other/repo#4 and https://github.com/x/y/issues/9. Not color: &#39; or a/b#c"
	var got []string
	for _, m := range reHashRef.FindAllStringSubmatch(body, -1) {
		got = append(got, m[1])
	}
	if !slices.Equal(got, []string{"12"}) {
		t.Errorf("hash refs %v", got)
	}
	if m := reRepoRef.FindAllStringSubmatch(body, -1); len(m) != 1 || m[0][1] != "other/repo" {
		t.Errorf("repo refs %v", m)
	}
}

func TestFingerprintIgnoresBotActivity(t *testing.T) {
	c := testConfig(t)
	s := snapshot(nil)
	base := s.Fingerprint(c)
	s.IssueComments = []Comment{{ID: 5, User: User{Login: "triage-bot"}, Body: "closing " + MarkerCloseNoIssue}, {ID: 6, User: User{Login: "codecov[bot]"}}}
	s.PR.Labels = append(s.PR.Labels, struct {
		Name string `json:"name"`
	}{"triage: possible spam"})
	if s.Fingerprint(c) != base {
		t.Fatal("bot comments or managed labels changed the fingerprint")
	}
	s.IssueComments = append(s.IssueComments, Comment{ID: 7, User: User{Login: "alice"}})
	if s.Fingerprint(c) == base {
		t.Fatal("a human comment did not change the fingerprint")
	}
	// The token can belong to a maintainer: their unmarked comments count.
	withHuman := s.Fingerprint(c)
	s.IssueComments = append(s.IssueComments, Comment{ID: 8, User: User{Login: "triage-bot"}, Body: "Could you add a test?"})
	if s.Fingerprint(c) == withHuman {
		t.Fatal("the token owner's own comment was ignored")
	}
}

func TestRenamedRoleLabels(t *testing.T) {
	c := testConfig(t)
	for i, l := range c.Labels {
		switch l.Role {
		case RoleAwaitingReview:
			c.Labels[i].Name = "needs review"
		case RolePossibleSpam:
			c.Labels[i].Name = "spam?"
		}
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	f := &fakeDecide{typ: "type: bug"}
	p, err := Analyze(context.Background(), c, snapshot(nil), f.decide, "k")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"needs review"}; !slices.Equal(p.HostLabels, want) {
		t.Fatalf("host labels %v, want %v", p.HostLabels, want)
	}
	if !c.Managed("needs review") {
		t.Fatal("a role label must be managed whatever its group")
	}
}

func TestRoleValidation(t *testing.T) {
	c := testConfig(t)
	for i, l := range c.Labels {
		if l.Role == RolePossibleSpam {
			c.Labels[i].Role = ""
		}
	}
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), RolePossibleSpam) {
		t.Fatalf("missing role not reported: %v", err)
	}
	c = testConfig(t)
	c.Labels = append(c.Labels, Label{Name: "spam?", Group: "triage", Role: RolePossibleSpam})
	if err := c.validate(); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("duplicate role not reported: %v", err)
	}
}

func green(s *Snapshot) { s.Merge = MergeState{CI: CISuccess} }

func TestStatusFromCIAndConflicts(t *testing.T) {
	c := testConfig(t)
	for _, tc := range []struct {
		name   string
		merge  MergeState
		status string
	}{
		{"green", MergeState{CI: CISuccess}, RoleReadyForReview},
		{"pending", MergeState{CI: CIPending}, RoleAwaitingReview},
		{"no checks yet", MergeState{CI: CINone}, RoleAwaitingReview},
		{"failing", MergeState{CI: CIFailure, Failing: []string{"lint"}, FailedAt: time.Now().Add(-time.Hour)}, RoleAwaitingAuthor},
		{"conflict", MergeState{CI: CISuccess, Conflict: true}, RoleAwaitingAuthor},
	} {
		f := &fakeDecide{typ: "type: bug"}
		p, err := Analyze(context.Background(), c, snapshot(func(s *Snapshot) { s.Merge = tc.merge }), f.decide, "k")
		if err != nil {
			t.Fatal(err)
		}
		if p.Status != tc.status {
			t.Errorf("%s: status %q, want %q (reasons %v)", tc.name, p.Status, tc.status, p.Reasons)
		}
		if slices.Contains(f.seen, "awaiting_author") {
			t.Errorf("%s: asked the model with no open messages", tc.name)
		}
	}

	// Without a ready-for-review label, a green PR gets awaiting-review.
	for i, l := range c.Labels {
		if l.Role == RoleReadyForReview {
			c.Labels[i].Role = ""
		}
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	p, _ := Analyze(context.Background(), c, snapshot(green), (&fakeDecide{typ: "type: bug"}).decide, "k")
	if p.Status != RoleAwaitingReview {
		t.Fatalf("no ready role: status %q", p.Status)
	}
}

func TestAuthorRepliedWaitsOnMaintainer(t *testing.T) {
	c := testConfig(t)
	asked := time.Now().Add(-48 * time.Hour)
	p, _ := Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		green(s)
		s.IssueComments = []Comment{
			{ID: 1, User: User{Login: "maint"}, AuthorAssociation: "MEMBER", Body: "Why this approach?", CreatedAt: asked},
			{ID: 2, User: User{Login: "alice"}, Body: "Because of X.", CreatedAt: asked.Add(time.Hour)},
		}
	}), (&fakeDecide{typ: "type: bug"}).decide, "k")
	if p.Status != RoleAwaitingReview {
		t.Fatalf("status %q, want awaiting-review after the author answered", p.Status)
	}
}

func thread(t *testing.T, js string) Thread {
	t.Helper()
	var th Thread
	if err := json.Unmarshal([]byte(js), &th); err != nil {
		t.Fatal(err)
	}
	return th
}

func TestReviewBotThreads(t *testing.T) {
	c := testConfig(t)
	c.ReviewBots = []string{"coderabbitai[bot]"}
	open := `{"isResolved":false,"isOutdated":false,"path":"server/x.go","comments":{"nodes":[
		{"author":{"login":"coderabbitai[bot]"},"body":"Potential nil dereference here.","createdAt":"2026-10-01T00:00:00Z"}]}}`
	answered := `{"isResolved":false,"isOutdated":false,"path":"server/x.go","comments":{"nodes":[
		{"author":{"login":"coderabbitai[bot]"},"body":"Potential nil dereference here.","createdAt":"2026-10-01T00:00:00Z"},
		{"author":{"login":"alice"},"body":"Not reachable, see the guard above.","createdAt":"2026-10-02T00:00:00Z"}]}}`

	f := &fakeDecide{typ: "type: bug", p: map[string]float64{"awaiting_author": 0.85}}
	p, _ := Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		green(s)
		s.Graph.Threads = []Thread{thread(t, open)}
	}), f.decide, "k")
	if p.Status != RoleAwaitingAuthor || !slices.Contains(f.seen, "awaiting_author") {
		t.Fatalf("open bot thread: status %q seen %v", p.Status, f.seen)
	}

	f = &fakeDecide{typ: "type: bug", p: map[string]float64{"awaiting_author": 0.85}}
	p, _ = Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		green(s)
		s.Graph.Threads = []Thread{thread(t, answered)}
	}), f.decide, "k")
	if p.Status != RoleReadyForReview || slices.Contains(f.seen, "awaiting_author") {
		t.Fatalf("answered bot thread: status %q seen %v", p.Status, f.seen)
	}

	// A bot that is not in review_bots is ignored.
	c.ReviewBots = nil
	f = &fakeDecide{typ: "type: bug", p: map[string]float64{"awaiting_author": 0.85}}
	p, _ = Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		green(s)
		s.Graph.Threads = []Thread{thread(t, open)}
	}), f.decide, "k")
	if p.Status != RoleReadyForReview {
		t.Fatalf("unlisted bot counted: status %q", p.Status)
	}
}

func TestDuplicates(t *testing.T) {
	c := testConfig(t)
	now := time.Now()
	others := []Candidate{
		{Number: 3, Title: "fix(store): crash on empty list", Files: []string{"store/list.go"}, CreatedAt: now.Add(-60 * 24 * time.Hour)},
		{Number: 9, Title: "fix(store): crash on empty list", Files: []string{"store/list.go"}, CreatedAt: now}, // newer: never the original
		{Number: 4, Title: "docs: update README", Files: []string{"README.md"}, CreatedAt: now.Add(-60 * 24 * time.Hour)},
	}
	f := &fakeDecide{typ: "type: bug", p: map[string]float64{"dup_3": 0.92}}
	p, err := Analyze(context.Background(), c, snapshot(func(s *Snapshot) { s.Others = others }), f.decide, "k")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.DuplicateOf, []int{3}) || !slices.Contains(p.HostLabels, "status: duplicate") {
		t.Fatalf("duplicate not flagged: %+v", p)
	}
	if slices.Contains(f.seen, "dup_9") || slices.Contains(f.seen, "dup_4") {
		t.Fatalf("asked about a newer or unrelated PR: %v", f.seen)
	}
	if !strings.Contains(p.DuplicateComment, "#3") || !slices.Contains(p.Required(), ActionDupe) {
		t.Fatalf("comment or required action missing: %+v", p)
	}

	f = &fakeDecide{typ: "type: bug", p: map[string]float64{"dup_3": 0.3}}
	if p, _ = Analyze(context.Background(), c, snapshot(func(s *Snapshot) { s.Others = others }), f.decide, "k"); len(p.DuplicateOf) > 0 {
		t.Fatalf("flagged below threshold: %v", p.DuplicateOf)
	}

	for i, l := range c.Labels {
		if l.Role == RoleDuplicate {
			c.Labels[i].Role = ""
		}
	}
	if err := c.validate(); err == nil {
		t.Fatal("duplicates.enabled without a duplicate label validated")
	}
}

func TestSummarizeChecks(t *testing.T) {
	at := time.Now()
	for _, tc := range []struct {
		runs []CheckRun
		st   []CommitStatus
		want string
	}{
		{nil, nil, CINone},
		{[]CheckRun{{Name: "a", Status: "completed", Conclusion: "success"}, {Name: "b", Status: "completed", Conclusion: "skipped"}}, nil, CISuccess},
		{[]CheckRun{{Name: "a", Status: "in_progress"}}, nil, CIPending},
		{[]CheckRun{{Name: "a", Status: "completed", Conclusion: "success"}}, []CommitStatus{{Context: "ext", State: "pending"}}, CIPending},
		{[]CheckRun{{Name: "lint", Status: "completed", Conclusion: "failure", CompletedAt: at}, {Name: "b", Status: "in_progress"}}, nil, CIFailure},
		{nil, []CommitStatus{{Context: "ext", State: "error"}}, CIFailure},
	} {
		if got := summarizeChecks(tc.runs, tc.st); got.CI != tc.want {
			t.Errorf("summarizeChecks(%v, %v) = %q, want %q", tc.runs, tc.st, got.CI, tc.want)
		}
	}
}

func TestModelDefaults(t *testing.T) {
	c := testConfig(t)
	if c.Model != DefaultModel || c.DecisionModel != DefaultDecisionModel {
		t.Fatalf("triage.yaml models: %q, %q", c.Model, c.DecisionModel)
	}
	c.Model, c.DecisionModel = "", ""
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	if c.Model != DefaultModel || c.DecisionModel != DefaultDecisionModel {
		t.Fatalf("empty models not defaulted: %q, %q", c.Model, c.DecisionModel)
	}
}

func TestGraphQLBotLoginsMatchReviewBots(t *testing.T) {
	c := testConfig(t)
	c.ReviewBots = []string{"coderabbitai[bot]"}
	// GraphQL's shape: no [bot] suffix, __typename Bot.
	raw := thread(t, `{"isResolved":false,"isOutdated":false,"path":"x.go","comments":{"nodes":[
		{"author":{"login":"coderabbitai","__typename":"Bot"},"body":"Possible race here.","createdAt":"2026-10-01T00:00:00Z"}]}}`)
	threads := normalizeThreads([]Thread{raw})
	if got := threads[0].Comments.Nodes[0].Author.Login; got != "coderabbitai[bot]" {
		t.Fatalf("login %q", got)
	}
	f := &fakeDecide{typ: "type: bug", p: map[string]float64{"awaiting_author": 0.9}}
	p, _ := Analyze(context.Background(), c, snapshot(func(s *Snapshot) { green(s); s.Graph.Threads = threads }), f.decide, "k")
	if p.Status != RoleAwaitingAuthor {
		t.Fatalf("GraphQL-shaped bot thread ignored: status %q, reasons %v", p.Status, p.Reasons)
	}
	// A human named like a bot is left alone.
	h := thread(t, `{"isResolved":false,"comments":{"nodes":[{"author":{"login":"alice","__typename":"User"},"body":"x","createdAt":"2026-10-01T00:00:00Z"}]}}`)
	if got := normalizeThreads([]Thread{h})[0].Comments.Nodes[0].Author.Login; got != "alice" {
		t.Fatalf("user login changed to %q", got)
	}
}

func TestOneDecisionCallPerPR(t *testing.T) {
	c := testConfig(t)
	c.ReviewBots = []string{"coderabbitai[bot]"}
	now := time.Now()
	f := &fakeDecide{typ: "type: bug", p: map[string]float64{"awaiting_author": 0.9, "ci_harmful": 0.01, "ci_obfuscated": 0.01, "dup_3": 0.95}}
	p, err := Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		s.PendingRuns = []WorkflowRun{{ID: 99, HeadSHA: "abc123"}}
		s.IssueComments = []Comment{{ID: 1, User: User{Login: "maint"}, AuthorAssociation: "MEMBER", Body: "Please add a test.", CreatedAt: now.Add(-time.Hour)}}
		s.Others = []Candidate{{Number: 3, Title: "fix(store): crash on empty list", Files: []string{"store/list.go"}, CreatedAt: now.Add(-60 * 24 * time.Hour)}}
	}), f.decide, "k")
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 {
		t.Fatalf("%d decision calls, want 1", f.calls)
	}
	for _, q := range []string{"type", "promotional", "awaiting_author", "ci_harmful", "ci_obfuscated", "dup_3"} {
		if !slices.Contains(f.seen, q) {
			t.Errorf("question %q not asked; asked %v", q, f.seen)
		}
	}
	for _, sec := range []string{"## Pull request under triage", "## Open messages to the author", "## Earlier pull requests"} {
		if !strings.Contains(f.state, sec) {
			t.Errorf("state lacks section %q", sec)
		}
	}
	if p.Status != RoleAwaitingAuthor || len(p.ApproveRuns) != 1 || !slices.Equal(p.DuplicateOf, []int{3}) {
		t.Fatalf("answers not all applied: status=%q approve=%v dup=%v", p.Status, p.ApproveRuns, p.DuplicateOf)
	}

	// A promotional close still makes one call and ignores the CI and duplicate answers.
	f = &fakeDecide{typ: "type: docs", p: map[string]float64{"promotional": 0.99, "ci_harmful": 0.01, "ci_obfuscated": 0.01, "dup_3": 0.95}}
	p, _ = Analyze(context.Background(), c, snapshot(func(s *Snapshot) {
		s.PendingRuns = []WorkflowRun{{ID: 99, HeadSHA: "abc123"}}
		s.Others = []Candidate{{Number: 3, Title: "fix(store): crash on empty list", Files: []string{"store/list.go"}, CreatedAt: now.Add(-60 * 24 * time.Hour)}}
	}), f.decide, "k")
	if f.calls != 1 || p.Close != ClosePromotional || len(p.ApproveRuns) > 0 || len(p.DuplicateOf) > 0 {
		t.Fatalf("closing PR: calls=%d close=%q approve=%v dup=%v", f.calls, p.Close, p.ApproveRuns, p.DuplicateOf)
	}
}
