package triage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GitHub is a small REST and GraphQL client bound to one repository. Every
// path it builds starts at /repos/{owner}/{name}, so no caller can reach
// another repository through it.
type GitHub struct {
	owner, name, token string
	base               string
	http               *http.Client

	selfOnce  sync.Once
	self      string
	selfErr   error
	labelOnce sync.Mutex
	labelsOK  bool
}

// NewGitHub builds a client from GITHUB_TOKEN. GITHUB_API_URL overrides the
// API root for GitHub Enterprise.
func NewGitHub(c *Config) (*GitHub, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, errors.New("triage: GITHUB_TOKEN is not set")
	}
	base := strings.TrimRight(os.Getenv("GITHUB_API_URL"), "/")
	if base == "" {
		base = "https://api.github.com"
	}
	return &GitHub{
		owner: c.Owner(), name: c.Name(), token: token, base: base,
		http: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

var (
	ghOnce sync.Once
	ghInst *GitHub
	ghErr  error
)

// Client returns the process-wide client for the configured repository.
func Client() (*Config, *GitHub, error) {
	c, err := Get()
	if err != nil {
		return nil, nil, err
	}
	ghOnce.Do(func() { ghInst, ghErr = NewGitHub(c) })
	return c, ghInst, ghErr
}

// User is a GitHub account.
type User struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

// PR is the part of a pull request the triage reads.
type PR struct {
	Number            int       `json:"number"`
	Title             string    `json:"title"`
	Body              string    `json:"body"`
	State             string    `json:"state"`
	Draft             bool      `json:"draft"`
	User              User      `json:"user"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Labels            []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Head struct {
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Additions    int `json:"additions"`
	Deletions    int `json:"deletions"`
	ChangedFiles int `json:"changed_files"`
	// Mergeable is nil while GitHub is still computing it. Only GetPR
	// returns it, not the list endpoint.
	Mergeable      *bool      `json:"mergeable"`
	MergeableState string     `json:"mergeable_state"`
	MergedAt       *time.Time `json:"merged_at"`
}

// LabelNames returns the PR's current label names.
func (p *PR) LabelNames() []string {
	out := make([]string, 0, len(p.Labels))
	for _, l := range p.Labels {
		out = append(out, l.Name)
	}
	return out
}

// File is one changed file.
type File struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// Comment is an issue comment or an inline review comment.
type Comment struct {
	ID                int64     `json:"id"`
	User              User      `json:"user"`
	Body              string    `json:"body"`
	AuthorAssociation string    `json:"author_association"`
	CreatedAt         time.Time `json:"created_at"`
}

// Review is a submitted pull request review.
type Review struct {
	ID                int64     `json:"id"`
	User              User      `json:"user"`
	Body              string    `json:"body"`
	State             string    `json:"state"`
	AuthorAssociation string    `json:"author_association"`
	SubmittedAt       time.Time `json:"submitted_at"`
}

// WorkflowRun is an Actions run.
type WorkflowRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Event      string `json:"event"`
}

// Thread is a review thread from GraphQL.
type Thread struct {
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Comments   struct {
		Nodes []struct {
			Author *struct {
				Login    string `json:"login"`
				Typename string `json:"__typename"`
			} `json:"author"`
			AuthorAssociation string    `json:"authorAssociation"`
			Body              string    `json:"body"`
			CreatedAt         time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"comments"`
}

// LinkedIssue is an issue GitHub links to a PR.
type LinkedIssue struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	State      string `json:"state"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

// PRGraph is the GraphQL part of a snapshot.
type PRGraph struct {
	ClosingIssues []LinkedIssue
	Threads       []Thread
	LastCommitAt  time.Time
}

func (g *GitHub) repoPath(format string, args ...any) string {
	return "/repos/" + url.PathEscape(g.owner) + "/" + url.PathEscape(g.name) + fmt.Sprintf(format, args...)
}

// APIError is a non-2xx GitHub response.
type APIError struct {
	Status int
	Path   string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s: %d %s", e.Path, e.Status, e.Msg)
}

func (g *GitHub) do(ctx context.Context, method, p string, body, out any) (http.Header, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := p
	if !strings.HasPrefix(p, "https://") {
		u = g.base + p
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &m)
		// Strip the query so a token-free path is all an error ever shows.
		short, _, _ := strings.Cut(strings.TrimPrefix(u, g.base), "?")
		return resp.Header, &APIError{Status: resp.StatusCode, Path: method + " " + short, Msg: m.Message}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.Header, fmt.Errorf("github %s: decode: %w", p, err)
		}
	}
	return resp.Header, nil
}

// getAll follows Link rel="next" up to max pages of 100.
func getAll[T any](ctx context.Context, g *GitHub, p string, maxPages int) ([]T, error) {
	sep := "?"
	if strings.Contains(p, "?") {
		sep = "&"
	}
	next := p + sep + "per_page=100"
	var all []T
	for page := 0; next != "" && page < maxPages; page++ {
		var batch []T
		h, err := g.do(ctx, http.MethodGet, next, nil, &batch)
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
		next = nextLink(h.Get("Link"))
	}
	return all, nil
}

func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 || strings.TrimSpace(seg[1]) != `rel="next"` {
			continue
		}
		return strings.Trim(strings.TrimSpace(seg[0]), "<>")
	}
	return ""
}

// Self returns the token's login, so the triage can tell its own comments
// from everyone else's.
func (g *GitHub) Self(ctx context.Context) (string, error) {
	g.selfOnce.Do(func() {
		var u User
		if _, err := g.do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
			// A GitHub App installation token cannot read /user; fall back
			// to the configured name.
			if v := os.Getenv("GITHUB_BOT_LOGIN"); v != "" {
				g.self = v
				return
			}
			g.selfErr = fmt.Errorf("triage: identify token owner (set GITHUB_BOT_LOGIN for app tokens): %w", err)
			return
		}
		g.self = u.Login
	})
	return g.self, g.selfErr
}

// OpenPRs lists open pull requests, most recently updated first.
func (g *GitHub) OpenPRs(ctx context.Context) ([]PR, error) {
	return getAll[PR](ctx, g, g.repoPath("/pulls?state=open&sort=updated&direction=desc"), 10)
}

// RecentlyClosedPRs lists up to 100 closed pull requests, most recently
// updated first.
func (g *GitHub) RecentlyClosedPRs(ctx context.Context) ([]PR, error) {
	return getAll[PR](ctx, g, g.repoPath("/pulls?state=closed&sort=updated&direction=desc"), 1)
}

// CheckRun is one check on a commit.
type CheckRun struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	CompletedAt time.Time `json:"completed_at"`
}

// CommitStatus is one legacy commit status.
type CommitStatus struct {
	Context   string    `json:"context"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Checks returns the check runs and legacy statuses on a commit.
func (g *GitHub) Checks(ctx context.Context, sha string) ([]CheckRun, []CommitStatus, error) {
	var runs struct {
		CheckRuns []CheckRun `json:"check_runs"`
	}
	if _, err := g.do(ctx, http.MethodGet, g.repoPath("/commits/%s/check-runs?per_page=100", url.PathEscape(sha)), nil, &runs); err != nil {
		return nil, nil, err
	}
	var st struct {
		Statuses []CommitStatus `json:"statuses"`
	}
	if _, err := g.do(ctx, http.MethodGet, g.repoPath("/commits/%s/status?per_page=100", url.PathEscape(sha)), nil, &st); err != nil {
		return nil, nil, err
	}
	return runs.CheckRuns, st.Statuses, nil
}

// GetPR returns one pull request with its line counts.
func (g *GitHub) GetPR(ctx context.Context, n int) (*PR, error) {
	var p PR
	if _, err := g.do(ctx, http.MethodGet, g.repoPath("/pulls/%d", n), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Files returns up to 300 changed files with patches.
func (g *GitHub) Files(ctx context.Context, n int) ([]File, error) {
	return getAll[File](ctx, g, g.repoPath("/pulls/%d/files", n), 3)
}

// IssueComments returns the PR's conversation comments.
func (g *GitHub) IssueComments(ctx context.Context, n int) ([]Comment, error) {
	return getAll[Comment](ctx, g, g.repoPath("/issues/%d/comments", n), 10)
}

// ReviewComments returns the PR's inline review comments.
func (g *GitHub) ReviewComments(ctx context.Context, n int) ([]Comment, error) {
	return getAll[Comment](ctx, g, g.repoPath("/pulls/%d/comments", n), 10)
}

// Reviews returns the PR's submitted reviews.
func (g *GitHub) Reviews(ctx context.Context, n int) ([]Review, error) {
	return getAll[Review](ctx, g, g.repoPath("/pulls/%d/reviews", n), 5)
}

// IsIssue reports whether #n in the repository is an issue, not a PR.
func (g *GitHub) IsIssue(ctx context.Context, n int) (bool, error) {
	var v struct {
		PullRequest *struct{} `json:"pull_request"`
	}
	_, err := g.do(ctx, http.MethodGet, g.repoPath("/issues/%d", n), nil, &v)
	var ae *APIError
	if errors.As(err, &ae) && (ae.Status == 404 || ae.Status == 410) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v.PullRequest == nil, nil
}

// Graph reads closing issue references, review threads and the last
// commit time in one GraphQL query.
func (g *GitHub) Graph(ctx context.Context, n int) (*PRGraph, error) {
	const q = `query($owner:String!,$name:String!,$number:Int!){
  repository(owner:$owner,name:$name){
    pullRequest(number:$number){
      closingIssuesReferences(first:20){nodes{number title state repository{nameWithOwner}}}
      reviewThreads(first:100){nodes{isResolved isOutdated path
        comments(last:5){nodes{author{login __typename} authorAssociation body createdAt}}}}
      commits(last:1){nodes{commit{committedDate}}}
    }
  }
}`
	var resp struct {
		Data struct {
			Repository struct {
				PullRequest *struct {
					ClosingIssuesReferences struct {
						Nodes []LinkedIssue `json:"nodes"`
					} `json:"closingIssuesReferences"`
					ReviewThreads struct {
						Nodes []Thread `json:"nodes"`
					} `json:"reviewThreads"`
					Commits struct {
						Nodes []struct {
							Commit struct {
								CommittedDate time.Time `json:"committedDate"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": q, "variables": map[string]any{"owner": g.owner, "name": g.name, "number": n}}
	if _, err := g.do(ctx, http.MethodPost, graphqlURL(g.base), body, &resp); err != nil {
		return nil, err
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("github graphql: %s", resp.Errors[0].Message)
	}
	pr := resp.Data.Repository.PullRequest
	if pr == nil {
		return nil, fmt.Errorf("github graphql: PR #%d not found", n)
	}
	out := &PRGraph{ClosingIssues: pr.ClosingIssuesReferences.Nodes, Threads: normalizeThreads(pr.ReviewThreads.Nodes)}
	if len(pr.Commits.Nodes) > 0 {
		out.LastCommitAt = pr.Commits.Nodes[0].Commit.CommittedDate
	}
	return out, nil
}

// normalizeThreads gives bot authors the login REST uses. GraphQL reports a
// GitHub App as "coderabbitai" where REST, and so review_bots and IsBot, say
// "coderabbitai[bot]".
func normalizeThreads(threads []Thread) []Thread {
	for i := range threads {
		for j := range threads[i].Comments.Nodes {
			a := threads[i].Comments.Nodes[j].Author
			if a != nil && a.Typename == "Bot" && !strings.HasSuffix(a.Login, "[bot]") {
				a.Login += "[bot]"
			}
		}
	}
	return threads
}

func graphqlURL(base string) string {
	if strings.HasSuffix(base, "/api/v3") {
		return strings.TrimSuffix(base, "/v3") + "/graphql"
	}
	return base + "/graphql"
}

// PendingApprovalRuns lists workflow runs for a head SHA that wait for a
// maintainer to approve them.
func (g *GitHub) PendingApprovalRuns(ctx context.Context, sha string) ([]WorkflowRun, error) {
	var resp struct {
		Runs []WorkflowRun `json:"workflow_runs"`
	}
	p := g.repoPath("/actions/runs?status=action_required&head_sha=%s&per_page=100", url.QueryEscape(sha))
	if _, err := g.do(ctx, http.MethodGet, p, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Runs, nil
}

// GetRun returns one workflow run.
func (g *GitHub) GetRun(ctx context.Context, id int64) (*WorkflowRun, error) {
	var r WorkflowRun
	if _, err := g.do(ctx, http.MethodGet, g.repoPath("/actions/runs/%d", id), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ApproveRun approves a fork pull request's workflow run.
func (g *GitHub) ApproveRun(ctx context.Context, id int64) error {
	_, err := g.do(ctx, http.MethodPost, g.repoPath("/actions/runs/%d/approve", id), nil, nil)
	return err
}

// AddLabels adds labels to an issue or PR.
func (g *GitHub) AddLabels(ctx context.Context, n int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	_, err := g.do(ctx, http.MethodPost, g.repoPath("/issues/%d/labels", n), map[string]any{"labels": labels}, nil)
	return err
}

// RemoveLabel removes one label; a label that is already gone is fine.
func (g *GitHub) RemoveLabel(ctx context.Context, n int, label string) error {
	_, err := g.do(ctx, http.MethodDelete, g.repoPath("/issues/%d/labels/%s", n, url.PathEscape(label)), nil, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 404 {
		return nil
	}
	return err
}

// EnsureLabels creates the configured labels that the repository lacks.
// It succeeds once per process and retries after a failure.
func (g *GitHub) EnsureLabels(ctx context.Context, c *Config) error {
	g.labelOnce.Lock()
	defer g.labelOnce.Unlock()
	if g.labelsOK {
		return nil
	}
	type repoLabel struct {
		Name string `json:"name"`
	}
	have, err := getAll[repoLabel](ctx, g, g.repoPath("/labels"), 10)
	if err != nil {
		return err
	}
	exists := map[string]bool{}
	for _, l := range have {
		exists[strings.ToLower(l.Name)] = true
	}
	for _, l := range c.Labels {
		if exists[strings.ToLower(l.Name)] {
			continue
		}
		body := map[string]any{"name": l.Name, "color": strings.TrimPrefix(l.Color, "#"), "description": l.Description}
		if l.Color == "" {
			body["color"] = "ededed"
		}
		if _, err := g.do(ctx, http.MethodPost, g.repoPath("/labels"), body, nil); err != nil {
			return fmt.Errorf("create label %q: %w", l.Name, err)
		}
	}
	g.labelsOK = true
	return nil
}

// Comment posts a PR conversation comment.
func (g *GitHub) Comment(ctx context.Context, n int, body string) error {
	_, err := g.do(ctx, http.MethodPost, g.repoPath("/issues/%d/comments", n), map[string]any{"body": body}, nil)
	return err
}

// Close closes a pull request.
func (g *GitHub) Close(ctx context.Context, n int) error {
	_, err := g.do(ctx, http.MethodPatch, g.repoPath("/pulls/%d", n), map[string]any{"state": "closed"}, nil)
	return err
}

// itoa is strconv.Itoa, kept short for message building.
func itoa(n int) string { return strconv.Itoa(n) }
