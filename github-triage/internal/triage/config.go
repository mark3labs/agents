// Package triage holds the host side of the github-triage agent: its
// configuration, the GitHub client, the PR state store, the policy that turns
// a PR snapshot and decision answers into a plan, and the gates the tools
// enforce. Everything here runs in the BONNIE process, not the sandbox.
package triage

import (
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Label groups. The policy finds its own labels by role, not group; groups
// say what else the bot may touch. Type labels are proposed by the decision
// model and can be overridden by the agent; area labels are chosen by the
// agent and only ever added; labels in status and triage, and any
// label with a role, are removed when they no longer apply; any other group
// (such as "manual") is never touched.
const (
	GroupType   = "type"
	GroupStatus = "status"
	GroupTriage = "triage"
	GroupArea   = "area"
)

// Label is one managed label.
type Label struct {
	Name  string `yaml:"name"`
	Group string `yaml:"group"`
	// Role marks the label the policy applies for one situation, such as
	// awaiting-author. The policy finds labels by role, never by name.
	Role        string `yaml:"role"`
	Color       string `yaml:"color"`
	Description string `yaml:"description"`
}

// Config is triage.yaml after environment overrides.
type Config struct {
	Repo          string   `yaml:"repo"`
	PollCron      string   `yaml:"poll_cron"`
	DryRun        bool     `yaml:"dry_run"`
	MaxPRsPerPoll int      `yaml:"max_prs_per_poll"`
	StatePath     string   `yaml:"state_path"`
	Maintainers   []string `yaml:"maintainers"`
	ExemptAuthors []string `yaml:"exempt_authors"`
	// ExemptMaintainers keeps maintainers' PRs out of the close rules. It
	// is a pointer so an absent key means true.
	ExemptMaintainers *bool    `yaml:"exempt_maintainers"`
	ExemptLabels      []string `yaml:"exempt_labels"`
	Promotional       struct {
		CloseThreshold float64 `yaml:"close_threshold"`
		LabelThreshold float64 `yaml:"label_threshold"`
		Comment        string  `yaml:"comment"`
	} `yaml:"promotional"`
	LinkedIssue struct {
		Enabled        bool   `yaml:"enabled"`
		AcceptMentions bool   `yaml:"accept_mentions"`
		Comment        string `yaml:"comment"`
	} `yaml:"linked_issue"`
	AwaitingAuthor struct {
		Threshold      float64 `yaml:"threshold"`
		NudgeAfterDays int     `yaml:"nudge_after_days"`
	} `yaml:"awaiting_author"`
	CI struct {
		AutoApprove       bool     `yaml:"auto_approve"`
		MaxRisk           float64  `yaml:"max_risk"`
		MaxDiffBytes      int      `yaml:"max_diff_bytes"`
		NeverApprovePaths []string `yaml:"never_approve_paths"`
	} `yaml:"ci"`
	// ReviewBots are bot accounts whose review threads count as feedback
	// the author may still owe, such as coderabbitai[bot].
	ReviewBots []string `yaml:"review_bots"`
	Duplicates struct {
		Enabled            bool    `yaml:"enabled"`
		Threshold          float64 `yaml:"threshold"`
		MaxCandidates      int     `yaml:"max_candidates"`
		MergedLookbackDays int     `yaml:"merged_lookback_days"`
		Comment            string  `yaml:"comment"`
	} `yaml:"duplicates"`
	Labels []Label `yaml:"labels"`

	owner, name string
	byName      map[string]Label
	byRole      map[string]string
}

// DefaultPollCron is used when the config cannot be read at startup, so the
// binary can still print help; the first poll then reports the real error.
const DefaultPollCron = "*/10 * * * *"

var (
	loadOnce sync.Once
	loaded   *Config
	loadErr  error
)

// ConfigPath is the config file. TRIAGE_CONFIG overrides it.
func ConfigPath() string {
	if p := os.Getenv("TRIAGE_CONFIG"); p != "" {
		return p
	}
	return "triage.yaml"
}

// Get loads the config once per process.
func Get() (*Config, error) {
	loadOnce.Do(func() { loaded, loadErr = Load(ConfigPath()) })
	return loaded, loadErr
}

// Load reads and validates a config file and applies environment overrides.
func Load(file string) (*Config, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("triage: read config: %w", err)
	}
	c := &Config{}
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("triage: parse %s: %w", file, err)
	}
	if v := os.Getenv("GITHUB_REPO"); v != "" {
		c.Repo = v
	}
	if v := os.Getenv("TRIAGE_DB"); v != "" {
		c.StatePath = v
	}
	if v := os.Getenv("TRIAGE_DRY_RUN"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("triage: TRIAGE_DRY_RUN: %w", err)
		}
		c.DryRun = b
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("triage: %s: %w", file, err)
	}
	return c, nil
}

func (c *Config) validate() error {
	owner, name, ok := strings.Cut(c.Repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || c.Repo == "owner/name" {
		return fmt.Errorf("repo must be owner/name, got %q (set it in the file or GITHUB_REPO)", c.Repo)
	}
	c.owner, c.name = owner, name
	if c.PollCron == "" {
		c.PollCron = DefaultPollCron
	}
	if c.MaxPRsPerPoll <= 0 {
		c.MaxPRsPerPoll = 10
	}
	if c.StatePath == "" {
		c.StatePath = ".bonnie/triage.db"
	}
	if c.CI.MaxDiffBytes <= 0 {
		c.CI.MaxDiffBytes = 24000
	}
	if c.Duplicates.MaxCandidates <= 0 {
		c.Duplicates.MaxCandidates = 8
	}
	if c.Duplicates.MaxCandidates > 15 {
		return errors.New("duplicates.max_candidates must be at most 15")
	}
	if c.Duplicates.Threshold == 0 {
		c.Duplicates.Threshold = 0.8
	}
	for _, p := range []float64{c.Promotional.CloseThreshold, c.Promotional.LabelThreshold, c.AwaitingAuthor.Threshold, c.CI.MaxRisk, c.Duplicates.Threshold} {
		if p < 0 || p > 1 {
			return errors.New("thresholds must be between 0 and 1")
		}
	}
	if c.Promotional.LabelThreshold > c.Promotional.CloseThreshold {
		return errors.New("promotional.label_threshold must not exceed close_threshold")
	}
	for _, g := range c.CI.NeverApprovePaths {
		if _, err := path.Match(strings.ReplaceAll(g, "**", "*"), ""); err != nil {
			return fmt.Errorf("ci.never_approve_paths %q: %w", g, err)
		}
	}
	c.byName = map[string]Label{}
	c.byRole = map[string]string{}
	for _, l := range c.Labels {
		if l.Name == "" || l.Group == "" {
			return fmt.Errorf("label %+v needs a name and a group", l)
		}
		if _, dup := c.byName[l.Name]; dup {
			return fmt.Errorf("label %q is listed twice", l.Name)
		}
		c.byName[l.Name] = l
		if l.Role == "" {
			continue
		}
		if !slices.Contains(Roles, l.Role) && !slices.Contains(OptionalRoles, l.Role) {
			return fmt.Errorf("label %q has unknown role %q (known: %s)", l.Name, l.Role,
				strings.Join(append(slices.Clone(Roles), OptionalRoles...), ", "))
		}
		if other, dup := c.byRole[l.Role]; dup {
			return fmt.Errorf("role %q is on both %q and %q", l.Role, other, l.Name)
		}
		if l.Group == GroupType || l.Group == GroupArea {
			return fmt.Errorf("label %q: a role label cannot be in the %s group", l.Name, l.Group)
		}
		c.byRole[l.Role] = l.Name
	}
	for _, r := range Roles {
		if _, ok := c.byRole[r]; !ok {
			return fmt.Errorf("no label has role %q; add role: %s to the label the policy should use", r, r)
		}
	}
	if _, ok := c.byRole[RoleDuplicate]; c.Duplicates.Enabled && !ok {
		return fmt.Errorf("duplicates.enabled needs a label with role: %s", RoleDuplicate)
	}
	if len(c.TypeLabels()) == 0 {
		return errors.New("at least one label in the type group is required")
	}
	return nil
}

// Roles of the labels the policy applies. Each must be on exactly one label
// in triage.yaml; the label's name is free.
const (
	RoleAwaitingAuthor    = "awaiting-author"
	RoleAwaitingReview    = "awaiting-review"
	RolePossibleSpam      = "possible-spam"
	RoleCINeedsReview     = "ci-needs-review"
	RoleClosedPromotional = "closed-promotional"
	RoleClosedNoIssue     = "closed-no-issue"

	// Optional roles. Without a ready-for-review label, a PR that waits on
	// the maintainers always gets the awaiting-review label.
	RoleReadyForReview = "ready-for-review"
	RoleDuplicate      = "duplicate"
)

// Roles lists every role the policy needs.
var Roles = []string{
	RoleAwaitingAuthor, RoleAwaitingReview,
	RolePossibleSpam, RoleCINeedsReview, RoleClosedPromotional, RoleClosedNoIssue,
}

// OptionalRoles may be on a label; see RoleReadyForReview and RoleDuplicate.
var OptionalRoles = []string{RoleReadyForReview, RoleDuplicate}

// RoleLabel returns the name of the label with a role, or "" for an
// optional role no label has. validate guarantees every role in Roles has
// one.
func (c *Config) RoleLabel(role string) string { return c.byRole[role] }

// IsReviewBot reports whether a login is one of review_bots.
func (c *Config) IsReviewBot(login string) bool { return containsFold(c.ReviewBots, login) }

// Owner and Name split Repo.
func (c *Config) Owner() string { return c.owner }
func (c *Config) Name() string  { return c.name }

// Label returns a managed label by name.
func (c *Config) Label(name string) (Label, bool) {
	l, ok := c.byName[name]
	return l, ok
}

// Managed reports whether the bot may add or remove a label: every label
// with a role, and every label in a group it manages.
func (c *Config) Managed(name string) bool {
	l, ok := c.byName[name]
	if !ok {
		return false
	}
	if l.Role != "" {
		return true
	}
	switch l.Group {
	case GroupType, GroupStatus, GroupTriage, GroupArea:
		return true
	}
	return false
}

// TypeLabels returns the type/* labels in file order.
func (c *Config) TypeLabels() []Label { return c.group(GroupType) }

// AreaLabels returns the area labels in file order.
func (c *Config) AreaLabels() []Label { return c.group(GroupArea) }

func (c *Config) group(g string) []Label {
	var out []Label
	for _, l := range c.Labels {
		if l.Group == g {
			out = append(out, l)
		}
	}
	return out
}

// IsMaintainer reports whether a login with a GitHub author association is
// a maintainer.
func (c *Config) IsMaintainer(login, association string) bool {
	switch association {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	}
	return containsFold(c.Maintainers, login)
}

// IsBot reports whether a login is a GitHub App or bot account.
func IsBot(login, typ string) bool {
	return typ == "Bot" || strings.HasSuffix(login, "[bot]")
}

// NeverApprove reports whether a changed path matches never_approve_paths.
func (c *Config) NeverApprove(file string) bool {
	for _, g := range c.CI.NeverApprovePaths {
		if globMatch(g, file) {
			return true
		}
	}
	return false
}

// globMatch matches slash-separated paths where a "**" segment matches any
// number of segments and other segments use path.Match.
func globMatch(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], name[0]); !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}
