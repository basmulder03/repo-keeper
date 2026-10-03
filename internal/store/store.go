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
	"strings"
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
	`ALTER TABLE repos ADD COLUMN source TEXT NOT NULL DEFAULT '';
	ALTER TABLE repos ADD COLUMN full_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE repos ADD COLUMN remote_id TEXT NOT NULL DEFAULT '';
	ALTER TABLE repos ADD COLUMN clone_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE repos ADD COLUMN missing INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE repos ADD COLUMN archived INTEGER NOT NULL DEFAULT 0;
	CREATE TABLE accounts (
		name TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT '',
		login TEXT NOT NULL DEFAULT '',
		warnings TEXT NOT NULL DEFAULT '',
		expires_ms INTEGER NOT NULL DEFAULT 0,
		checked_ms INTEGER NOT NULL DEFAULT 0,
		discovered_ms INTEGER NOT NULL DEFAULT 0,
		next_discovery_ms INTEGER NOT NULL DEFAULT 0,
		repo_count INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT ''
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
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)" // busy_timeout first: switching to WAL itself needs the lock
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	db.SetMaxOpenConns(1) // single writer; the workload is tiny and this removes SQLITE_BUSY races
	s := &Store{db: db}
	// Several processes may open a brand-new database at once (daemon + `status`); SQLite can answer BUSY at once,
	// without consulting the busy handler, so setup is retried briefly.
	if err := retryBusy(func() error {
		if err := db.PingContext(context.Background()); err != nil {
			return err
		}
		return s.migrate(context.Background())
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	// One dedicated connection and BEGIN IMMEDIATE: the write lock is taken before the version is read, so two
	// processes opening a fresh database serialise (the loser waits, then finds the schema already current).
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	var v int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	if v > len(migrations) {
		return fmt.Errorf("store: database schema v%d is newer than this binary (v%d); upgrade repo-keeper", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		if _, err := conn.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	committed = true
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
	Source         string // "" = listed in config, otherwise the account that discovered it
	FullName       string
	RemoteID       string
	CloneURL       string
	Missing        bool // no longer listed by its account; the local clone is left alone
	Archived       bool
}

// Spec is the desired configuration of one repo.
type Spec struct {
	Path     string
	Remote   string
	Interval time.Duration
	// Fields below are set for repos discovered through an account.
	FullName string
	RemoteID string
	CloneURL string
	Archived bool
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

// SyncRepos makes the active config-listed (manual) repos equal specs: upserts, schedules new repos at firstDue,
// deactivates the rest. History of removed repos is kept so re-adding one does not lose it.
func (s *Store) SyncRepos(ctx context.Context, specs []Spec, firstDue func(i int) time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// #nosec G202 -- in holds only question-mark placeholders; values are bound
	if _, err := tx.ExecContext(ctx, "UPDATE repos SET active = 0 WHERE source = ''"); err != nil {
		return err
	}
	for i, sp := range specs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO repos(path, remote, interval_s, active, next_sync_ms) VALUES(?, ?, ?, 1, ?)
			ON CONFLICT(path) DO UPDATE SET remote=excluded.remote, interval_s=excluded.interval_s, active=1 WHERE repos.source = ''`,
			sp.Path, sp.Remote, int64(sp.Interval/time.Second), ms(firstDue(i))); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ManagedResult reports what SyncManaged changed.
type ManagedResult struct {
	Added     int
	Missing   int      // previously known repos the account no longer lists
	Conflicts []string // paths already owned by another source; skipped
}

// SyncManaged makes the repos discovered by account equal specs. Repos that disappeared are marked missing and
// deactivated (never deleted); a path owned by another source is reported, not taken over.
func (s *Store) SyncManaged(ctx context.Context, account string, specs []Spec, firstDue func(i int) time.Time) (ManagedResult, error) {
	var res ManagedResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	var before int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos WHERE source = ?", account).Scan(&before); err != nil {
		return res, err
	}
	// #nosec G202 -- in holds only question-mark placeholders; values are bound
	if _, err := tx.ExecContext(ctx, "UPDATE repos SET active = 0, missing = 1 WHERE source = ?", account); err != nil {
		return res, err
	}
	for i, sp := range specs {
		r, err := tx.ExecContext(ctx, `
			INSERT INTO repos(path, remote, interval_s, active, next_sync_ms, source, full_name, remote_id, clone_url, archived)
			VALUES(?, ?, ?, 1, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET remote=excluded.remote, interval_s=excluded.interval_s, active=1, missing=0,
				full_name=excluded.full_name, remote_id=excluded.remote_id, clone_url=excluded.clone_url, archived=excluded.archived
			WHERE repos.source = excluded.source`,
			sp.Path, sp.Remote, int64(sp.Interval/time.Second), ms(firstDue(i)), account, sp.FullName, sp.RemoteID, sp.CloneURL, b2i(sp.Archived))
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			res.Conflicts = append(res.Conflicts, sp.Path)
		}
	}
	var active int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos WHERE source = ? AND active = 1", account).Scan(&active); err != nil {
		return res, err
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM repos WHERE source = ? AND missing = 1", account).Scan(&res.Missing); err != nil {
		return res, err
	}
	res.Added = active - (before - res.Missing)
	if res.Added < 0 {
		res.Added = 0
	}
	return res, tx.Commit()
}

const repoCols = `id, path, remote, host, interval_s, active, default_branch, digest, next_sync_ms, last_sync_ms,
	last_status, last_reason, last_error, last_ff, failures, needs_attention, source, full_name, remote_id, clone_url, missing, archived`

func scanRepo(sc interface{ Scan(...any) error }) (Repo, error) {
	var r Repo
	var interval, next, last int64
	var active, attn, missing, archived int
	err := sc.Scan(&r.ID, &r.Path, &r.Remote, &r.Host, &interval, &active, &r.DefaultBranch, &r.Digest, &next, &last,
		&r.LastStatus, &r.LastReason, &r.LastError, &r.LastFF, &r.Failures, &attn,
		&r.Source, &r.FullName, &r.RemoteID, &r.CloneURL, &missing, &archived)
	r.Interval, r.Active, r.NeedsAttention = time.Duration(interval)*time.Second, active == 1, attn == 1
	r.Missing, r.Archived = missing == 1, archived == 1
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

// Account is the persisted health of a configured account.
type Account struct {
	Name          string
	Provider      string
	Status        string // ok | auth-failed | error | ""
	Login         string
	Warnings      string
	Expires       time.Time
	Checked       time.Time
	Discovered    time.Time
	NextDiscovery time.Time
	RepoCount     int
	Error         string
}

// SaveAccount upserts an account's state.
func (s *Store) SaveAccount(ctx context.Context, a Account) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts(name, provider, status, login, warnings, expires_ms, checked_ms, discovered_ms, next_discovery_ms, repo_count, error)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET provider=excluded.provider, status=excluded.status, login=excluded.login, warnings=excluded.warnings,
			expires_ms=excluded.expires_ms, checked_ms=excluded.checked_ms, discovered_ms=excluded.discovered_ms,
			next_discovery_ms=excluded.next_discovery_ms, repo_count=excluded.repo_count, error=excluded.error`,
		a.Name, a.Provider, a.Status, a.Login, a.Warnings, ms(a.Expires), ms(a.Checked), ms(a.Discovered), ms(a.NextDiscovery), a.RepoCount, a.Error)
	return err
}

// ListAccounts returns all known accounts ordered by name.
func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, provider, status, login, warnings, expires_ms, checked_ms, discovered_ms, next_discovery_ms, repo_count, error FROM accounts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Account
	for rows.Next() {
		var a Account
		var exp, chk, disc, next int64
		if err := rows.Scan(&a.Name, &a.Provider, &a.Status, &a.Login, &a.Warnings, &exp, &chk, &disc, &next, &a.RepoCount, &a.Error); err != nil {
			return nil, err
		}
		a.Expires, a.Checked, a.Discovered, a.NextDiscovery = fromMS(exp), fromMS(chk), fromMS(disc), fromMS(next)
		out = append(out, a)
	}
	return out, rows.Err()
}

// LatestRuns returns the newest run of every repo that has one, keyed by repo id.
func (s *Store) LatestRuns(ctx context.Context) (map[int64]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, repo_id, started_ms, finished_ms, status, reason, ff, fetched, error, detail
		FROM runs WHERE id IN (SELECT MAX(id) FROM runs GROUP BY repo_id)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]Run{}
	for rows.Next() {
		var r Run
		var st, fin int64
		var fetched int
		if err := rows.Scan(&r.ID, &r.RepoID, &st, &fin, &r.Status, &r.Reason, &r.FF, &fetched, &r.Error, &r.Detail); err != nil {
			return nil, err
		}
		r.Started, r.Finished, r.Fetched = fromMS(st), fromMS(fin), fetched == 1
		out[r.RepoID] = r
	}
	return out, rows.Err()
}

// retryBusy runs fn, retrying for up to ~10 s while SQLite reports the database as busy/locked.
func retryBusy(fn func() error) error {
	var err error
	for attempt := 0; attempt < 100; attempt++ {
		if err = fn(); err == nil || !isBusy(err) {
			return err
		}
		time.Sleep(time.Duration(10+attempt*2) * time.Millisecond) //nolint:forbidigo // real-time backoff against another process
	}
	return err
}

func isBusy(err error) bool {
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") || strings.Contains(s, "database is locked") || strings.Contains(s, "SQLITE_LOCKED")
}

// ForgetRemovedAccounts deactivates repos discovered by accounts that are no longer configured (their local clones
// are never touched) and drops those accounts' saved state. keep lists the account names still in the config.
func (s *Store) ForgetRemovedAccounts(ctx context.Context, keep []string) error {
	args := make([]any, len(keep))
	marks := make([]string, len(keep))
	for i, k := range keep {
		args[i], marks[i] = k, "?"
	}
	in := "(" + strings.Join(marks, ",") + ")"
	if len(keep) == 0 {
		in = "('')"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// #nosec G202 -- in holds only question-mark placeholders; values are bound
	if _, err := tx.ExecContext(ctx, "UPDATE repos SET active = 0 WHERE source <> '' AND source NOT IN "+in, args...); err != nil {
		return err
	}
	// #nosec G202 -- in holds only question-mark placeholders; values are bound
	if _, err := tx.ExecContext(ctx, "DELETE FROM accounts WHERE name NOT IN "+in, args...); err != nil {
		return err
	}
	return tx.Commit()
}
