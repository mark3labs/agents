package triage

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/bonnie"
	kit "github.com/mark3labs/kit/pkg/kit"
)

// Candidate is another PR a duplicate check compares against.
type Candidate struct {
	Number    int
	Title     string
	Body      string
	Files     []string
	CreatedAt time.Time
	Merged    bool
	Author    string
}

// The duplicate pool is at most this many PRs: the most recently updated
// open ones, then recently merged ones.
const maxPool = 40

var (
	filesMu    sync.Mutex
	filesCache = map[string][]string{} // "number@sha" -> changed paths
)

func cachedFiles(ctx context.Context, g *GitHub, pr *PR) ([]string, error) {
	k := itoa(pr.Number) + "@" + pr.Head.SHA
	filesMu.Lock()
	f, ok := filesCache[k]
	filesMu.Unlock()
	if ok {
		return f, nil
	}
	files, err := g.Files(ctx, pr.Number)
	if err != nil {
		return nil, err
	}
	for _, fl := range files {
		f = append(f, fl.Filename)
	}
	filesMu.Lock()
	filesCache[k] = f
	filesMu.Unlock()
	return f, nil
}

// FetchCandidates reads the pool of PRs a duplicate could match: other
// open PRs, and PRs merged in the last merged_lookback_days.
func FetchCandidates(ctx context.Context, c *Config, g *GitHub, self *PR) ([]Candidate, error) {
	open, err := g.OpenPRs(ctx)
	if err != nil {
		return nil, err
	}
	pool := make([]PR, 0, maxPool)
	for _, pr := range open {
		if pr.Number != self.Number && len(pool) < maxPool {
			pool = append(pool, pr)
		}
	}
	if days := c.Duplicates.MergedLookbackDays; days > 0 && len(pool) < maxPool {
		closed, err := g.RecentlyClosedPRs(ctx)
		if err != nil {
			return nil, err
		}
		cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
		for _, pr := range closed {
			if pr.MergedAt != nil && pr.MergedAt.After(cutoff) && len(pool) < maxPool {
				pool = append(pool, pr)
			}
		}
	}
	out := make([]Candidate, 0, len(pool))
	for i := range pool {
		pr := &pool[i]
		files, err := cachedFiles(ctx, g, pr)
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate{Number: pr.Number, Title: pr.Title, Body: pr.Body, Files: files,
			CreatedAt: pr.CreatedAt, Merged: pr.MergedAt != nil, Author: pr.User.Login})
	}
	return out, nil
}

var (
	reConventional = regexp.MustCompile(`^\s*[a-zA-Z]+(\([^)]*\))?!?:\s*`)
	reWord         = regexp.MustCompile(`[a-z0-9_]+`)
	stopwords      = map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "from": true, "when": true, "into": true,
		"add": true, "fix": true, "use": true, "not": true, "don": true, "its": true, "this": true, "that": true,
	}
)

func titleWords(t string) map[string]bool {
	t = strings.ToLower(reConventional.ReplaceAllString(t, ""))
	out := map[string]bool{}
	for _, w := range reWord.FindAllString(t, -1) {
		if len(w) >= 3 && !stopwords[w] {
			out[w] = true
		}
	}
	return out
}

func bodyRefs(body string) map[int]bool {
	out := map[int]bool{}
	for _, m := range reHashRef.FindAllStringSubmatch(body, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out[n] = true
		}
	}
	return out
}

func jaccard[K comparable](a, b map[K]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return float64(inter) / float64(len(a)+len(b)-inter)
}

func set(xs []string) map[string]bool {
	out := map[string]bool{}
	for _, x := range xs {
		out[x] = true
	}
	return out
}

// shortlist ranks candidates by cheap overlap: a shared issue reference,
// changed files and title words. Only those that overlap at all go to the
// decision model.
func shortlist(s *Snapshot, others []Candidate, max int) []Candidate {
	files := map[string]bool{}
	for _, f := range s.Files {
		files[f.Filename] = true
	}
	words := titleWords(s.PR.Title)
	refs := bodyRefs(s.PR.Body)
	for _, li := range s.Graph.ClosingIssues {
		refs[li.Number] = true
	}
	type scored struct {
		c     Candidate
		score float64
	}
	var ranked []scored
	for _, o := range others {
		// Only an older PR, or one already merged, can be the original.
		if !o.Merged && !o.CreatedAt.Before(s.PR.CreatedAt) {
			continue
		}
		score := jaccard(files, set(o.Files)) + jaccard(words, titleWords(o.Title))
		for n := range bodyRefs(o.Body) {
			if refs[n] && n != o.Number && n != s.PR.Number {
				score += 2
				break
			}
		}
		if score >= 0.2 {
			ranked = append(ranked, scored{o, score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	out := make([]Candidate, 0, max)
	for _, r := range ranked {
		if len(out) == max {
			break
		}
		out = append(out, r.c)
	}
	return out
}

// duplicates asks the decision model whether the PR duplicates any
// shortlisted older or merged PR, and labels it for a human if so.
func duplicates(ctx context.Context, c *Config, s *Snapshot, decide DecideFunc, keyPrefix string, p *Plan) error {
	cands := shortlist(s, s.Others, c.Duplicates.MaxCandidates)
	if len(cands) == 0 {
		return nil
	}
	describe := func(n int, title, body string, files []string, extra string) string {
		if len(files) > 30 {
			files = append(files[:30:30], fmt.Sprintf("... and %d more", len(files)-30))
		}
		return fmt.Sprintf("#%d%s: %s\n%s\nFiles: %s", n, extra, title, truncate(body, 800), strings.Join(files, ", "))
	}
	var paths []string
	for _, f := range s.Files {
		paths = append(paths, f.Filename)
	}
	var b strings.Builder
	b.WriteString("Pull request under triage:\n")
	b.WriteString(describe(s.PR.Number, s.PR.Title, s.PR.Body, paths, ""))
	b.WriteString("\n\nEarlier pull requests:\n")
	var questions []kit.DecisionQuestion
	for _, o := range cands {
		extra := " (open)"
		if o.Merged {
			extra = " (already merged)"
		}
		b.WriteString("\n" + describe(o.Number, o.Title, o.Body, o.Files, extra) + "\n")
		questions = append(questions, kit.BooleanQuestion(fmt.Sprintf("dup_%d", o.Number), fmt.Sprintf(
			"Does #%d make the same change or fix the same problem as #%d, so that only one of them should be merged? "+
				"Touching the same files for a different reason is not a duplicate.", s.PR.Number, o.Number)))
	}
	res, err := decide(ctx, bonnie.DecisionRequest{
		State:     kit.DecisionState{Text: truncate(b.String(), 24000)},
		Questions: questions,
		Key:       key(keyPrefix, "duplicates"),
	})
	if err != nil {
		// A duplicate check is advisory: note the failure, do not stop the triage.
		p.Reasons = append(p.Reasons, "Duplicate check failed: "+err.Error())
		return nil
	}
	for _, o := range cands {
		a, err := res.Boolean(fmt.Sprintf("dup_%d", o.Number))
		if err != nil {
			continue
		}
		p.Probabilities[fmt.Sprintf("duplicate_of_%d", o.Number)] = round(a.Probability)
		if a.Probability >= c.Duplicates.Threshold {
			p.DuplicateOf = append(p.DuplicateOf, o.Number)
		}
	}
	if len(p.DuplicateOf) == 0 {
		return nil
	}
	refs := make([]string, len(p.DuplicateOf))
	for i, n := range p.DuplicateOf {
		refs[i] = "#" + itoa(n)
	}
	p.HostLabels = append(p.HostLabels, c.RoleLabel(RoleDuplicate))
	p.Reasons = append(p.Reasons, "Possible duplicate of "+strings.Join(refs, ", ")+": labelled for a maintainer to confirm.")
	if t := strings.TrimSpace(c.Duplicates.Comment); t != "" {
		p.DuplicateComment = strings.ReplaceAll(t, "{prs}", strings.Join(refs, ", ")) + "\n\n" + MarkerDuplicate
	}
	return nil
}
