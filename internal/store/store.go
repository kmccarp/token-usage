// Package store is the SQLite index of everything the sources have scanned.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kmccarp/token-usage/internal/model"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS files (
	path       TEXT PRIMARY KEY,
	source     TEXT NOT NULL,
	size       INTEGER NOT NULL,
	mtime      INTEGER NOT NULL,
	offset     INTEGER NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	agent_id   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS sessions (
	source      TEXT NOT NULL,
	id          TEXT NOT NULL,
	kind        TEXT NOT NULL DEFAULT 'session',
	parent_id   TEXT NOT NULL DEFAULT '',
	cwd         TEXT NOT NULL DEFAULT '',
	workspace   TEXT NOT NULL DEFAULT '',
	project_dir TEXT NOT NULL DEFAULT '',
	title       TEXT NOT NULL DEFAULT '',
	git_branch  TEXT NOT NULL DEFAULT '',
	version     TEXT NOT NULL DEFAULT '',
	started_at  INTEGER NOT NULL DEFAULT 0,
	last_at     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (source, id)
);
CREATE TABLE IF NOT EXISTS agents (
	source      TEXT NOT NULL,
	session_id  TEXT NOT NULL,
	id          TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	agent_type  TEXT NOT NULL DEFAULT '',
	model       TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (source, session_id, id)
);
CREATE TABLE IF NOT EXISTS tool_uses (
	source      TEXT NOT NULL,
	id          TEXT NOT NULL,
	agent_type  TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (source, id)
);
CREATE TABLE IF NOT EXISTS events (
	source       TEXT NOT NULL,
	key          TEXT NOT NULL,
	session_id   TEXT NOT NULL,
	agent_id     TEXT NOT NULL DEFAULT '',
	ts           INTEGER NOT NULL,
	model        TEXT NOT NULL DEFAULT '',
	cwd          TEXT NOT NULL DEFAULT '',
	workspace    TEXT NOT NULL DEFAULT '',
	input        INTEGER NOT NULL DEFAULT 0,
	cache_create INTEGER NOT NULL DEFAULT 0,
	cache_read   INTEGER NOT NULL DEFAULT 0,
	output       INTEGER NOT NULL DEFAULT 0,
	reasoning    INTEGER NOT NULL DEFAULT 0,
	cache_1h     INTEGER NOT NULL DEFAULT 0,
	cache_5m     INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (source, key)
);
CREATE INDEX IF NOT EXISTS events_ts ON events (ts);
CREATE INDEX IF NOT EXISTS events_session ON events (source, session_id);
CREATE INDEX IF NOT EXISTS events_workspace ON events (workspace, ts);
CREATE TABLE IF NOT EXISTS limit_snapshots (
	source TEXT NOT NULL,
	ts     INTEGER NOT NULL,
	raw    TEXT NOT NULL,
	PRIMARY KEY (source, ts)
);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(OFF)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: serialize writers, keep it simple
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Reset drops every scanned row so the next scan rebuilds from scratch.
func (s *Store) Reset() error {
	for _, t := range []string{"files", "sessions", "agents", "tool_uses", "events", "limit_snapshots"} {
		if _, err := s.db.Exec("DELETE FROM " + t); err != nil {
			return err
		}
	}
	return nil
}

// FileState is the scan cursor for one file.
type FileState struct {
	Path      string
	Source    string
	Size      int64
	MTime     int64
	Offset    int64
	SessionID string
	AgentID   string
}

// FileStates returns the cursors for every file of the given source.
func (s *Store) FileStates(source string) (map[string]FileState, error) {
	rows, err := s.db.Query("SELECT path, source, size, mtime, offset, session_id, agent_id FROM files WHERE source = ?", source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FileState{}
	for rows.Next() {
		var f FileState
		if err := rows.Scan(&f.Path, &f.Source, &f.Size, &f.MTime, &f.Offset, &f.SessionID, &f.AgentID); err != nil {
			return nil, err
		}
		out[f.Path] = f
	}
	return out, rows.Err()
}

// Batch is a write transaction with prepared statements for the hot paths.
type Batch struct {
	tx        *sql.Tx
	insEvent  *sql.Stmt
	upSession *sql.Stmt
	upAgent   *sql.Stmt
	upTool    *sql.Stmt
	upFile    *sql.Stmt
	insLimit  *sql.Stmt
}

// Begin starts a write batch.
func (s *Store) Begin() (*Batch, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	b := &Batch{tx: tx}
	prep := func(q string) *sql.Stmt {
		if err != nil {
			return nil
		}
		var st *sql.Stmt
		st, err = tx.Prepare(q)
		return st
	}
	b.insEvent = prep(`INSERT INTO events (source, key, session_id, agent_id, ts, model, cwd, workspace, input, cache_create, cache_read, output, reasoning, cache_1h, cache_5m)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source, key) DO UPDATE SET session_id=excluded.session_id, agent_id=excluded.agent_id, ts=excluded.ts, model=excluded.model, cwd=excluded.cwd, workspace=excluded.workspace,
		input=excluded.input, cache_create=excluded.cache_create, cache_read=excluded.cache_read, output=excluded.output, reasoning=excluded.reasoning, cache_1h=excluded.cache_1h, cache_5m=excluded.cache_5m`)
	b.upSession = prep(`INSERT INTO sessions (source, id, kind, parent_id, cwd, workspace, project_dir, title, git_branch, version, started_at, last_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(source, id) DO UPDATE SET
		kind = CASE WHEN excluded.kind <> '' THEN excluded.kind ELSE kind END,
		parent_id = CASE WHEN excluded.parent_id <> '' THEN excluded.parent_id ELSE parent_id END,
		cwd = CASE WHEN excluded.cwd <> '' THEN excluded.cwd ELSE cwd END,
		workspace = CASE WHEN excluded.workspace <> '' THEN excluded.workspace ELSE workspace END,
		project_dir = CASE WHEN excluded.project_dir <> '' THEN excluded.project_dir ELSE project_dir END,
		title = CASE WHEN excluded.title <> '' THEN excluded.title ELSE title END,
		git_branch = CASE WHEN excluded.git_branch <> '' THEN excluded.git_branch ELSE git_branch END,
		version = CASE WHEN excluded.version <> '' THEN excluded.version ELSE version END,
		started_at = CASE WHEN started_at = 0 OR (excluded.started_at > 0 AND excluded.started_at < started_at) THEN excluded.started_at ELSE started_at END,
		last_at = MAX(last_at, excluded.last_at)`)
	b.upAgent = prep(`INSERT INTO agents (source, session_id, id, description, agent_type, model) VALUES (?,?,?,?,?,?)
		ON CONFLICT(source, session_id, id) DO UPDATE SET
		description = CASE WHEN excluded.description <> '' THEN excluded.description ELSE description END,
		agent_type = CASE WHEN excluded.agent_type <> '' THEN excluded.agent_type ELSE agent_type END,
		model = CASE WHEN excluded.model <> '' THEN excluded.model ELSE model END`)
	b.upTool = prep(`INSERT INTO tool_uses (source, id, agent_type, description) VALUES (?,?,?,?)
		ON CONFLICT(source, id) DO UPDATE SET agent_type=excluded.agent_type, description=excluded.description`)
	b.upFile = prep(`INSERT INTO files (path, source, size, mtime, offset, session_id, agent_id) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, offset=excluded.offset, session_id=excluded.session_id, agent_id=excluded.agent_id`)
	b.insLimit = prep(`INSERT OR IGNORE INTO limit_snapshots (source, ts, raw) VALUES (?,?,?)`)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	return b, nil
}

// Event upserts one event.
func (b *Batch) Event(e model.Event) error {
	_, err := b.insEvent.Exec(e.Source, e.Key, e.SessionID, e.AgentID, e.TS, e.Model, e.Cwd, e.Workspace,
		e.Input, e.CacheCreate, e.CacheRead, e.Output, e.Reasoning, e.Cache1h, e.Cache5m)
	return err
}

// Session upserts session metadata; empty fields never overwrite known ones.
func (b *Batch) Session(s model.Session) error {
	_, err := b.upSession.Exec(s.Source, s.ID, s.Kind, s.ParentID, s.Cwd, s.Workspace, s.ProjectDir, s.Title, s.GitBranch, s.Version, s.StartedAt, s.LastAt)
	return err
}

// Agent upserts agent metadata.
func (b *Batch) Agent(a model.Agent) error {
	_, err := b.upAgent.Exec(a.Source, a.SessionID, a.ID, a.Description, a.AgentType, a.Model)
	return err
}

// ToolUse records an agent-launching tool call so a later result can be typed.
func (b *Batch) ToolUse(source, id, agentType, description string) error {
	_, err := b.upTool.Exec(source, id, agentType, description)
	return err
}

// LookupToolUse returns a recorded tool call, if any.
func (b *Batch) LookupToolUse(source, id string) (agentType, description string, ok bool) {
	err := b.tx.QueryRow("SELECT agent_type, description FROM tool_uses WHERE source=? AND id=?", source, id).Scan(&agentType, &description)
	return agentType, description, err == nil
}

// File records the scan cursor for a file.
func (b *Batch) File(f FileState) error {
	_, err := b.upFile.Exec(f.Path, f.Source, f.Size, f.MTime, f.Offset, f.SessionID, f.AgentID)
	return err
}

// LimitSnapshot stores one rate-limit observation.
func (b *Batch) LimitSnapshot(source string, ts int64, raw string) error {
	_, err := b.insLimit.Exec(source, ts, raw)
	return err
}

// Commit commits the batch.
func (b *Batch) Commit() error {
	for _, st := range []*sql.Stmt{b.insEvent, b.upSession, b.upAgent, b.upTool, b.upFile, b.insLimit} {
		st.Close()
	}
	return b.tx.Commit()
}

// Rollback abandons the batch.
func (b *Batch) Rollback() error { return b.tx.Rollback() }

// LatestLimitSnapshot returns the most recent snapshot for a source.
func (s *Store) LatestLimitSnapshot(source string) (model.LimitSnapshot, error) {
	var snap model.LimitSnapshot
	err := s.db.QueryRow("SELECT source, ts, raw FROM limit_snapshots WHERE source=? ORDER BY ts DESC LIMIT 1", source).Scan(&snap.Source, &snap.TS, &snap.Raw)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, nil
	}
	return snap, err
}

// SaveLimitSnapshot stores a snapshot outside a batch (used by API pollers).
func (s *Store) SaveLimitSnapshot(source string, ts int64, raw string) error {
	_, err := s.db.Exec("INSERT OR IGNORE INTO limit_snapshots (source, ts, raw) VALUES (?,?,?)", source, ts, raw)
	return err
}

// Filter narrows queries. Zero values mean "no constraint".
type Filter struct {
	From      int64 // unix ms inclusive
	To        int64 // unix ms exclusive
	Source    string
	Workspace string
	SessionID string
	AgentID   string // "" = any; "-" = main thread only
	Model     string
	ModelLike string // substring match on model (case-insensitive), e.g. a model family
	Dir       string // cwd equal to Dir or beneath it
}

func (f Filter) where() (string, []any) {
	var w []string
	var args []any
	if f.From > 0 {
		w = append(w, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To > 0 {
		w = append(w, "ts < ?")
		args = append(args, f.To)
	}
	if f.Source != "" {
		w = append(w, "source = ?")
		args = append(args, f.Source)
	}
	if f.Workspace != "" {
		w = append(w, "workspace = ?")
		args = append(args, f.Workspace)
	}
	if f.SessionID != "" {
		w = append(w, "session_id = ?")
		args = append(args, f.SessionID)
	}
	if f.AgentID == "-" {
		w = append(w, "agent_id = ''")
	} else if f.AgentID != "" {
		w = append(w, "agent_id = ?")
		args = append(args, f.AgentID)
	}
	if f.Model != "" {
		w = append(w, "model = ?")
		args = append(args, f.Model)
	}
	if f.ModelLike != "" {
		w = append(w, "instr(lower(model), ?) > 0")
		args = append(args, strings.ToLower(f.ModelLike))
	}
	if f.Dir != "" {
		d := strings.TrimRight(f.Dir, "/")
		if d == "" {
			d = "/"
			w = append(w, "cwd LIKE '/%'")
		} else {
			w = append(w, "(cwd = ? OR cwd LIKE ? ESCAPE '\\')")
			args = append(args, d, escapeLike(d)+"/%")
		}
	}
	if len(w) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(w, " AND "), args
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const sumCols = "COUNT(*), COALESCE(SUM(input),0), COALESCE(SUM(cache_create),0), COALESCE(SUM(cache_read),0), COALESCE(SUM(output),0), COALESCE(SUM(reasoning),0), COALESCE(MIN(ts),0), COALESCE(MAX(ts),0)"

func scanTotals(sc interface{ Scan(...any) error }, extra ...any) (model.Totals, error) {
	var t model.Totals
	dest := append(extra, &t.Requests, &t.Input, &t.CacheCreate, &t.CacheRead, &t.Output, &t.Reasoning, &t.FirstTS, &t.LastTS)
	if err := sc.Scan(dest...); err != nil {
		return t, err
	}
	t.Total = t.Input + t.CacheCreate + t.CacheRead + t.Output
	return t, nil
}

// Totals aggregates every event matching the filter.
func (s *Store) Totals(ctx context.Context, f Filter) (model.Totals, error) {
	w, args := f.where()
	return scanTotals(s.db.QueryRowContext(ctx, "SELECT "+sumCols+" FROM events"+w, args...))
}

// Group is one row of a breakdown.
type Group struct {
	Key    string       `json:"key"`
	Label  string       `json:"label,omitempty"`
	Meta   any          `json:"meta,omitempty"`
	Totals model.Totals `json:"totals"`
}

var groupCols = map[string]string{
	"source":    "source",
	"workspace": "workspace",
	"model":     "model",
	"session":   "session_id",
	"agent":     "agent_id",
	"cwd":       "cwd",
}

// GroupBy aggregates events by one dimension.
func (s *Store) GroupBy(ctx context.Context, dim string, f Filter, limit int) ([]Group, error) {
	col, ok := groupCols[dim]
	if !ok {
		return nil, fmt.Errorf("unknown dimension %q", dim)
	}
	w, args := f.where()
	q := "SELECT " + col + ", " + sumCols + " FROM events" + w + " GROUP BY " + col + " ORDER BY (SUM(input)+SUM(cache_create)+SUM(cache_read)+SUM(output)) DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		t, err := scanTotals(rows, &g.Key)
		if err != nil {
			return nil, err
		}
		g.Totals = t
		out = append(out, g)
	}
	return out, rows.Err()
}

// Bucket is one time-series point for one series.
type Bucket struct {
	TS     int64        `json:"ts"`
	Series string       `json:"series"`
	Totals model.Totals `json:"totals"`
}

// TimeSeries buckets events by time and one optional dimension.
func (s *Store) TimeSeries(ctx context.Context, bucketMs int64, dim string, f Filter) ([]Bucket, error) {
	col := "''"
	if dim != "" {
		c, ok := groupCols[dim]
		if !ok {
			return nil, fmt.Errorf("unknown dimension %q", dim)
		}
		col = c
	}
	w, args := f.where()
	q := fmt.Sprintf("SELECT (ts / %d) * %d AS b, %s, %s FROM events%s GROUP BY b, %s ORDER BY b", bucketMs, bucketMs, col, sumCols, w, col)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Bucket
	for rows.Next() {
		var b Bucket
		t, err := scanTotals(rows, &b.TS, &b.Series)
		if err != nil {
			return nil, err
		}
		b.Totals = t
		out = append(out, b)
	}
	return out, rows.Err()
}

// Sessions returns metadata for the given session ids of one source.
func (s *Store) Sessions(ctx context.Context, source string, ids []string) (map[string]model.Session, error) {
	out := map[string]model.Session{}
	if len(ids) == 0 {
		return out, nil
	}
	for i := 0; i < len(ids); i += 500 {
		end := min(i+500, len(ids))
		chunk := ids[i:end]
		ph := strings.Repeat("?,", len(chunk))
		args := make([]any, 0, len(chunk)+1)
		args = append(args, source)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, "SELECT source, id, kind, parent_id, cwd, workspace, project_dir, title, git_branch, version, started_at, last_at FROM sessions WHERE source=? AND id IN ("+ph[:len(ph)-1]+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var m model.Session
			if err := rows.Scan(&m.Source, &m.ID, &m.Kind, &m.ParentID, &m.Cwd, &m.Workspace, &m.ProjectDir, &m.Title, &m.GitBranch, &m.Version, &m.StartedAt, &m.LastAt); err != nil {
				rows.Close()
				return nil, err
			}
			out[m.ID] = m
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Session returns one session.
func (s *Store) Session(ctx context.Context, source, id string) (model.Session, bool, error) {
	m, err := s.Sessions(ctx, source, []string{id})
	if err != nil {
		return model.Session{}, false, err
	}
	v, ok := m[id]
	return v, ok, nil
}

// Agents returns the agents of one session keyed by id.
func (s *Store) Agents(ctx context.Context, source, sessionID string) (map[string]model.Agent, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT source, session_id, id, description, agent_type, model FROM agents WHERE source=? AND session_id=?", source, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.Agent{}
	for rows.Next() {
		var a model.Agent
		if err := rows.Scan(&a.Source, &a.SessionID, &a.ID, &a.Description, &a.AgentType, &a.Model); err != nil {
			return nil, err
		}
		out[a.ID] = a
	}
	return out, rows.Err()
}

// Stats is a quick health view of the index.
type Stats struct {
	Files    int64            `json:"files"`
	Events   int64            `json:"events"`
	Sessions int64            `json:"sessions"`
	Agents   int64            `json:"agents"`
	BySource map[string]int64 `json:"events_by_source"`
	FirstTS  int64            `json:"first_ts"`
	LastTS   int64            `json:"last_ts"`
}

// Stats counts rows.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	st.BySource = map[string]int64{}
	if err := s.db.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM files), (SELECT COUNT(*) FROM events), (SELECT COUNT(*) FROM sessions), (SELECT COUNT(*) FROM agents), (SELECT COALESCE(MIN(ts),0) FROM events), (SELECT COALESCE(MAX(ts),0) FROM events)").
		Scan(&st.Files, &st.Events, &st.Sessions, &st.Agents, &st.FirstTS, &st.LastTS); err != nil {
		return st, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT source, COUNT(*) FROM events GROUP BY source")
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var n int64
		if err := rows.Scan(&src, &n); err != nil {
			return st, err
		}
		st.BySource[src] = n
	}
	return st, rows.Err()
}

// SetMeta stores a key/value pair.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec("INSERT INTO meta (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

// GetMeta reads a key.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Now is a hook for tests.
var Now = time.Now
