// SPDX-License-Identifier: Apache-2.0

// Package store persists repo state, run history and events in SQLite (pure Go, WAL).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// migrations are forward-only; index+1 is the schema version.
var migrations = []string{
	`CREATE TABLE repos (
		id INTEGER PRIMARY KEY,
		path TEXT NOT NULL UNIQUE,
		remote TEXT NOT NULL,
		host TEXT NOT NULL DEFAULT '',
		interval_s INTEGER NOT NULL,
		active INTEGER NOT NULL DEFAULT 1,
		default_branch TEXT NOT NULL DEFAULT '',
		digest TEXT NOT NULL DEFAULT '',
		next_sync_ms INTEGER NOT NULL DEFAULT 0,
		last_sync_ms INTEGER NOT NULL DEFAULT 0,
		last_status TEXT NOT NULL DEFAULT '',
		last_reason TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		last_ff TEXT NOT NULL DEFAULT '',
		failures INTEGER NOT NULL DEFAULT 0,
		needs_attention INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX repos_due ON repos(active, next_sync_ms);
	CREATE TABLE runs (
		id INTEGER PRIMARY KEY,
		repo_id INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
		started_ms INTEGER NOT NULL,
		finished_ms INTEGER NOT NULL,
		status TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		ff TEXT NOT NULL DEFAULT '',
		fetched INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX runs_repo ON runs(repo_id, id DESC);
	CREATE TABLE events (
		id INTEGER PRIMARY KEY,
		ts_ms INTEGER NOT NULL,
		level TEXT NOT NULL,
		repo_id INTEGER NOT NULL DEFAULT 0,
		code TEXT NOT NULL,
		message TEXT NOT NULL
	);`,
}

const (
	keepRunsPerRepo = 50
	keepEvents      = 2000
)

// Store is the SQLite-backed state.
type Store struct{ db *sql.DB }

// Open opens (creating and migrating) the database at path; the file is private to the user.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	db.SetMaxOpenConns(1) // single writer; the workload is tiny and this removes SQLITE_BUSY races
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("store: database schema v%d is newer than this binary (v%d); upgrade repo-keeper", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("store: %w", err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	return nil
}

// Repo is a tracked repository and its latest state.
type Repo struct {
	ID             int64
	Path           string
	Remote         string
	Host           string
	Interval       time.Duration
	Active         bool
	DefaultBranch  string
	Digest         string
	NextSync       time.Time
	LastSync       time.Time
	LastStatus     string
	LastReason     string
	LastError      string
	LastFF         string
	Failures       int
	NeedsAttention bool
}

// Spec is the desired configuration of one repo.
type Spec struct {
	Path     string
	Remote   string
	Interval time.Duration
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// SyncRepos makes the active set equal specs: upserts, schedules new repos at firstDue, deactivates the rest.
// History of removed repos is kept so re-adding one does not lose it.
func (s *Store) SyncRepos(ctx context.Context, specs []Spec, firstDue func(i int) time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "UPDATE repos SET active = 0"); err != nil {
		return err
	}
	for i, sp := range specs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO repos(path, remote, interval_s, active, next_sync_ms) VALUES(?, ?, ?, 1, ?)
			ON CONFLICT(path) DO UPDATE SET remote=excluded.remote, interval_s=excluded.interval_s, active=1`,
			sp.Path, sp.Remote, int64(sp.Interval/time.Second), ms(firstDue(i))); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const repoCols = `id, path, remote, host, interval_s, active, default_branch, digest, next_sync_ms, last_sync_ms,
	last_status, last_reason, last_error, last_ff, failures, needs_attention`

func scanRepo(sc interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	var interval, next, last int64
	var active, attn int
	err := sc.Scan(&r.ID, &r.Path, &r.Remote, &r.Host, &interval, &active, &r.DefaultBranch, &r.Digest, &next, &last,
		&r.LastStatus, &r.LastReason, &r.LastError, &r.LastFF, &r.Failures, &attn)
	r.Interval, r.Active, r.NeedsAttention = time.Duration(interval)*time.Second, active == 1, attn == 1
	r.NextSync, r.LastSync = fromMS(next), fromMS(last)
	return r, err
}

func (s *Store) queryRepos(ctx context.Context, q string, args ...any) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRepos returns active repos ordered by path.
func (s *Store) ListRepos(ctx context.Context) ([]Repo, error) {
	return s.queryRepos(ctx, "SELECT "+repoCols+" FROM repos WHERE active = 1 ORDER BY path")
}

// Due returns active repos whose next sync is at or before now, oldest first.
func (s *Store) Due(ctx context.Context, now time.Time, limit int) ([]Repo, error) {
	return s.queryRepos(ctx, "SELECT "+repoCols+" FROM repos WHERE active = 1 AND next_sync_ms <= ? ORDER BY next_sync_ms, id LIMIT ?", ms(now), limit)
}

// Get returns one repo by id.
func (s *Store) Get(ctx context.Context, id int64) (Repo, error) {
	r, err := scanRepo(s.db.QueryRowContext(ctx, "SELECT "+repoCols+" FROM repos WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Repo{}, fmt.Errorf("store: repo %d not found", id)
	}
	return r, err
}

// Defer reschedules a repo without recording a run (e.g. its host is cooling down).
func (s *Store) Defer(ctx context.Context, id int64, until time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE repos SET next_sync_ms = ? WHERE id = ?", ms(until), id)
	return err
}

// SetHost records the remote host used for rate limiting.
func (s *Store) SetHost(ctx context.Context, id int64, host string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE repos SET host = ? WHERE id = ?", host, id)
	return err
}

// Run is one completed sync attempt plus the scheduling decision that follows it.
type Run struct {
	ID             int64
	RepoID         int64
	Started        time.Time
	Finished       time.Time
	Status         string
	Reason         string
	FF             string
	Fetched        bool
	Error          string
	Detail         string // JSON cleanup report, may be empty
	DefaultBranch  string // "" keeps the stored value
	Digest         string // "" keeps the stored value
	NextSync       time.Time
	Failures       int
	NeedsAttention bool
}

// Record stores a run and updates the repo atomically, trimming old history.
func (s *Store) Record(ctx context.Context, r Run) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(repo_id, started_ms, finished_ms, status, reason, ff, fetched, error, detail)
		VALUES(?,?,?,?,?,?,?,?,?)`, r.RepoID, ms(r.Started), ms(r.Finished), r.Status, r.Reason, r.FF, b2i(r.Fetched), r.Error, r.Detail); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repos SET
			last_sync_ms=?, last_status=?, last_reason=?, last_error=?, last_ff=?, failures=?, needs_attention=?, next_sync_ms=?,
			default_branch = CASE WHEN ? <> '' THEN ? ELSE default_branch END,
			digest = CASE WHEN ? <> '' THEN ? ELSE digest END
		WHERE id=?`, ms(r.Finished), r.Status, r.Reason, r.Error, r.FF, r.Failures, b2i(r.NeedsAttention), ms(r.NextSync),
		r.DefaultBranch, r.DefaultBranch, r.Digest, r.Digest, r.RepoID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE repo_id = ? AND id NOT IN
		(SELECT id FROM runs WHERE repo_id = ? ORDER BY id DESC LIMIT ?)`, r.RepoID, r.RepoID, keepRunsPerRepo); err != nil {
		return err
	}
	return tx.Commit()
}

// RecentRuns returns the newest runs of a repo first.
func (s *Store) RecentRuns(ctx context.Context, repoID int64, n int) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_id, started_ms, finished_ms, status, reason, ff, fetched, error, detail
		FROM runs WHERE repo_id = ? ORDER BY id DESC LIMIT ?`, repoID, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		var r Run
		var st, fin int64
		var fetched int
		if err := rows.Scan(&r.ID, &r.RepoID, &st, &fin, &r.Status, &r.Reason, &r.FF, &fetched, &r.Error, &r.Detail); err != nil {
			return nil, err
		}
		r.Started, r.Finished, r.Fetched = fromMS(st), fromMS(fin), fetched == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// Event is a notable occurrence for the UI log.
type Event struct {
	ID      int64
	Time    time.Time
	Level   string
	RepoID  int64
	Code    string
	Message string
}

// AddEvent appends an event, keeping the table bounded.
func (s *Store) AddEvent(ctx context.Context, e Event) error {
	if _, err := s.db.ExecContext(ctx, "INSERT INTO events(ts_ms, level, repo_id, code, message) VALUES(?,?,?,?,?)",
		ms(e.Time), e.Level, e.RepoID, e.Code, e.Message); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE id <= (SELECT MAX(id) FROM events) - ?", keepEvents)
	return err
}

// RecentEvents returns the newest events first.
func (s *Store) RecentEvents(ctx context.Context, n int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, ts_ms, level, repo_id, code, message FROM events ORDER BY id DESC LIMIT ?", n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Level, &e.RepoID, &e.Code, &e.Message); err != nil {
			return nil, err
		}
		e.Time = fromMS(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
