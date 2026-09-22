// Package model holds the types shared between sources, the store, the limit
// providers, and the HTTP server.
package model

import "time"

// Event is one model request (one API call) attributed to a session and,
// optionally, a sub-agent within that session. Token counts follow the
// Anthropic split: Input excludes anything served from or written to the
// prompt cache. Sources that report cached tokens as part of input (Codex)
// are normalized at ingest so Total is comparable across sources.
type Event struct {
	Source    string `json:"source"`
	Key       string `json:"key"` // unique within source; used to dedupe
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	TS        int64  `json:"ts"` // unix milliseconds
	Model     string `json:"model"`
	Cwd       string `json:"cwd"`
	Workspace string `json:"workspace"`

	Input       int64 `json:"input"`
	CacheCreate int64 `json:"cache_create"`
	CacheRead   int64 `json:"cache_read"`
	Output      int64 `json:"output"`
	Reasoning   int64 `json:"reasoning"` // subset of Output when the source reports it
	Cache1h     int64 `json:"cache_1h"`  // subset of CacheCreate (1-hour TTL writes)
	Cache5m     int64 `json:"cache_5m"`  // subset of CacheCreate (5-minute TTL writes)
}

// Total is the number of tokens that passed through the model for this call.
func (e Event) Total() int64 { return e.Input + e.CacheCreate + e.CacheRead + e.Output }

// Session is a top-level conversation with a tool. Sub-agents hang off a
// session as Agent rows; their events keep the parent's SessionID.
type Session struct {
	Source     string `json:"source"`
	ID         string `json:"id"`
	Kind       string `json:"kind"` // "session" or "subagent" (a subagent that could not be linked to a parent)
	ParentID   string `json:"parent_id,omitempty"`
	Cwd        string `json:"cwd"`
	Workspace  string `json:"workspace"`
	ProjectDir string `json:"project_dir,omitempty"`
	Title      string `json:"title,omitempty"`
	GitBranch  string `json:"git_branch,omitempty"`
	Version    string `json:"version,omitempty"`
	StartedAt  int64  `json:"started_at,omitempty"`
	LastAt     int64  `json:"last_at,omitempty"`
}

// Agent is a sub-agent launched from within a session.
type Agent struct {
	Source      string `json:"source"`
	SessionID   string `json:"session_id"`
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	AgentType   string `json:"agent_type,omitempty"`
	Model       string `json:"model,omitempty"`
}

// LimitSnapshot is one observation of a provider's rate-limit state, either
// fetched from an API or scraped from a transcript.
type LimitSnapshot struct {
	Source string `json:"source"`
	TS     int64  `json:"ts"`
	Raw    string `json:"raw"`
}

// Window is one rate-limit window normalized across providers.
type Window struct {
	Key           string    `json:"key"`   // e.g. "five_hour", "seven_day", "primary"
	Label         string    `json:"label"` // human label, e.g. "5-hour"
	Utilization   float64   `json:"utilization"`
	ResetsAt      time.Time `json:"resets_at"`
	StartedAt     time.Time `json:"started_at"` // when the provider says the window opened (zero if unknown)
	WindowMinutes int       `json:"window_minutes"`
	Scope         string    `json:"scope,omitempty"` // e.g. model scope for per-model weekly caps
	Active        bool      `json:"active"`          // provider flags this as the binding limit right now
}

// Share is one row of a provider's own breakdown of a window (e.g. which
// product consumed the weekly allowance).
type Share struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Percent float64 `json:"percent"`
}

// Limits is the normalized rate-limit view for one source.
type Limits struct {
	Source    string    `json:"source"`
	Plan      string    `json:"plan,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	Windows   []Window  `json:"windows"`
	Shares    []Share   `json:"shares,omitempty"` // provider-side split of the weekly window by product
	Notes     []string  `json:"notes,omitempty"`  // extra facts worth showing (extra usage, credits)
	Error     string    `json:"error,omitempty"`
	Raw       any       `json:"raw,omitempty"` // provider response, for debugging
}

// Totals is an aggregate over a set of events.
type Totals struct {
	Requests    int64 `json:"requests"`
	Input       int64 `json:"input"`
	CacheCreate int64 `json:"cache_create"`
	CacheRead   int64 `json:"cache_read"`
	Output      int64 `json:"output"`
	Reasoning   int64 `json:"reasoning"`
	Total       int64 `json:"total"`
	FirstTS     int64 `json:"first_ts,omitempty"`
	LastTS      int64 `json:"last_ts,omitempty"`
}
