package triage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Store keeps what the triage knows about each PR between polls, the plan
// each run computed, and the actions each run took. The BONNIE journal
// keeps the conversation; this keeps the facts the policy compares.
type Store struct{ db *sql.DB }

// PRState is one PR's row.
type PRState struct {
	Number int
	// UpdatedAt is GitHub's updated_at at the last poll that looked at it.
	UpdatedAt string
	// Fingerprint covers the activity that should trigger a triage: head
	// commit, title, body, draft flag and comments and reviews by anyone
	// but this bot. The bot's own labels and comments never retrigger it.
	Fingerprint string
	// TriagedFingerprint is the fingerprint the last finished triage saw.
	TriagedFingerprint string
	TriagedDry         bool
	Status             string
	AwaitingSince      string
	LastNudge          string
	Summary            string
	TriagedAt          string
}

const schema = `
CREATE TABLE IF NOT EXISTS prs (
  repo TEXT NOT NULL, number INTEGER NOT NULL,
  updated_at TEXT NOT NULL DEFAULT '', fingerprint TEXT NOT NULL DEFAULT '',
  triaged_fingerprint TEXT NOT NULL DEFAULT '', triaged_dry INTEGER NOT NULL DEFAULT 0,
  status TEXT NOT NULL DEFAULT '', awaiting_since TEXT NOT NULL DEFAULT '',
  last_nudge TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
  triaged_at TEXT NOT NULL DEFAULT '', closed INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (repo, number));
CREATE TABLE IF NOT EXISTS plans (
  run_id TEXT PRIMARY KEY, repo TEXT NOT NULL, number INTEGER NOT NULL,
  head_sha TEXT NOT NULL, plan TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS actions (
  run_id TEXT NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL,
  dry INTEGER NOT NULL, at TEXT NOT NULL,
  PRIMARY KEY (run_id, kind));
`

var (
	storeMu   sync.Mutex
	storeInst *Store
)

// OpenStore opens the process-wide store, creating it on first use.
func OpenStore(c *Config) (*Store, error) {
	storeMu.Lock()
	defer storeMu.Unlock()
	if storeInst != nil {
		return storeInst, nil
	}
	if err := os.MkdirAll(filepath.Dir(c.StatePath), 0o750); err != nil {
		return nil, fmt.Errorf("triage: state dir: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+c.StatePath+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, fmt.Errorf("triage: open state: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("triage: state schema: %w", err)
	}
	storeInst = &Store{db: db}
	return storeInst, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// PR returns a PR's row, or a zero row with Number set.
func (s *Store) PR(ctx context.Context, repo string, n int) (PRState, error) {
	st := PRState{Number: n}
	var dry int
	err := s.db.QueryRowContext(ctx, `SELECT updated_at, fingerprint, triaged_fingerprint, triaged_dry,
		status, awaiting_since, last_nudge, summary, triaged_at FROM prs WHERE repo=? AND number=?`, repo, n).
		Scan(&st.UpdatedAt, &st.Fingerprint, &st.TriagedFingerprint, &dry,
			&st.Status, &st.AwaitingSince, &st.LastNudge, &st.Summary, &st.TriagedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	st.TriagedDry = dry != 0
	return st, err
}

// OpenNumbers returns the PRs the store believes are open.
func (s *Store) OpenNumbers(ctx context.Context, repo string) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT number FROM prs WHERE repo=? AND closed=0`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// Observe records what a poll saw for an open PR.
func (s *Store) Observe(ctx context.Context, repo string, n int, updatedAt, fingerprint string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO prs (repo, number, updated_at, fingerprint) VALUES (?,?,?,?)
		ON CONFLICT(repo, number) DO UPDATE SET updated_at=excluded.updated_at, fingerprint=excluded.fingerprint, closed=0`,
		repo, n, updatedAt, fingerprint)
	return err
}

// MarkClosed records that a PR is no longer open.
func (s *Store) MarkClosed(ctx context.Context, repo string, n int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE prs SET closed=1 WHERE repo=? AND number=?`, repo, n)
	return err
}

// SavePlan stores the plan a run computed, replacing an earlier one.
func (s *Store) SavePlan(ctx context.Context, runID string, p *Plan) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO plans (run_id, repo, number, head_sha, plan, created_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET repo=excluded.repo, number=excluded.number, head_sha=excluded.head_sha,
		plan=excluded.plan, created_at=excluded.created_at`, runID, p.Repo, p.Number, p.HeadSHA, string(data), now())
	return err
}

// Plan returns the run's plan, or nil.
func (s *Store) Plan(ctx context.Context, runID string) (*Plan, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT plan FROM plans WHERE run_id=?`, runID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p := &Plan{}
	return p, json.Unmarshal([]byte(data), p)
}

// RecordAction notes that a run performed (or, dry, would perform) an action.
func (s *Store) RecordAction(ctx context.Context, runID, kind, detail string, dry bool) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions (run_id, kind, detail, dry, at) VALUES (?,?,?,?,?)
		ON CONFLICT(run_id, kind) DO UPDATE SET detail=excluded.detail, dry=excluded.dry, at=excluded.at`,
		runID, kind, detail, boolInt(dry), now())
	return err
}

// Actions returns the kinds of action a run recorded.
func (s *Store) Actions(ctx context.Context, runID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind FROM actions WHERE run_id=?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// FinishTriage records a finished triage of a plan's fingerprint.
func (s *Store) FinishTriage(ctx context.Context, p *Plan, summary string, dry bool, nudged bool) error {
	nudge := p.LastNudge
	if nudged {
		nudge = now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO prs (repo, number, fingerprint, triaged_fingerprint, triaged_dry,
		status, awaiting_since, last_nudge, summary, triaged_at, closed) VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(repo, number) DO UPDATE SET triaged_fingerprint=excluded.triaged_fingerprint,
		triaged_dry=excluded.triaged_dry, status=excluded.status, awaiting_since=excluded.awaiting_since,
		last_nudge=CASE WHEN excluded.last_nudge<>'' THEN excluded.last_nudge ELSE prs.last_nudge END,
		summary=excluded.summary, triaged_at=excluded.triaged_at, closed=excluded.closed`,
		p.Repo, p.Number, p.Fingerprint, p.Fingerprint, boolInt(dry), p.Status, p.AwaitingSince, nudge,
		summary, now(), boolInt(p.Close != ""))
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
