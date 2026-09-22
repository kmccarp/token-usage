// Package codex scans OpenAI Codex CLI rollouts under ~/.codex/sessions.
//
// Layout: <sessions>/YYYY/MM/DD/rollout-<timestamp>-<thread-id>.jsonl, one
// file per thread. Every event_msg/token_count line carries last_token_usage,
// the usage of the API call that just finished, plus the account's current
// rate-limit windows. Sub-agent threads are their own rollout files with
// thread_source = "subagent"; the parent link lives in Codex's state SQLite
// (thread_spawn_edges), which is consulted when present.
package codex

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kmccarp/token-usage/internal/model"
	"github.com/kmccarp/token-usage/internal/source"
	"github.com/kmccarp/token-usage/internal/store"
	"github.com/kmccarp/token-usage/internal/workspace"
)

// Source scans Codex.
type Source struct {
	Root    string // ~/.codex/sessions
	StateDB string // ~/.codex/state_*.sqlite (optional; used for parent links)
}

// Name implements source.Source.
func (s *Source) Name() string { return "codex" }

// Label implements source.Source.
func (s *Source) Label() string { return "Codex" }

type tokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

type line struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		// session_meta
		ID           string          `json:"id"`
		Cwd          string          `json:"cwd"`
		Originator   string          `json:"originator"`
		CLIVersion   string          `json:"cli_version"`
		Source       json.RawMessage `json:"source"`
		ThreadSource json.RawMessage `json:"thread_source"`
		Git          *struct {
			Branch string `json:"branch"`
		} `json:"git"`
		// turn_context
		Model string `json:"model"`
		// event_msg
		Type    string `json:"type"`
		Message string `json:"message"`
		Info    *struct {
			LastTokenUsage *tokenUsage `json:"last_token_usage"`
		} `json:"info"`
		RateLimits json.RawMessage `json:"rate_limits"`
	} `json:"payload"`
}

// Scan implements source.Source.
func (s *Source) Scan(ctx context.Context, st *store.Store, ws *workspace.Mapper) (source.ScanResult, error) {
	start := time.Now()
	var res source.ScanResult
	paths, err := source.WalkJSONL(s.Root, func(p string) bool { return strings.HasPrefix(filepath.Base(p), "rollout-") })
	if err != nil {
		return res, err
	}
	states, err := st.FileStates(s.Name())
	if err != nil {
		return res, err
	}
	parents := s.parentLinks()
	res.FilesSeen = len(paths)
	for _, p := range paths {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		prev, seen := states[p]
		size, mtime := fi.Size(), fi.ModTime().UnixMilli()
		if seen && prev.Size == size && prev.MTime == mtime {
			continue
		}
		offset := prev.Offset
		if !seen || size < prev.Offset {
			offset = 0
		}
		n, ev, err := s.scanFile(st, ws, p, prev, offset, parents)
		res.Lines += n
		res.Events += ev
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		res.FilesChanged++
	}
	res.Duration = time.Since(start)
	return res, nil
}

// threadIDFromName extracts the thread id from rollout-<ts>-<uuid>.jsonl.
func threadIDFromName(p string) string {
	base := strings.TrimSuffix(filepath.Base(p), ".jsonl")
	if len(base) > 36 {
		return base[len(base)-36:]
	}
	return base
}

func (s *Source) scanFile(st *store.Store, ws *workspace.Mapper, p string, prev store.FileState, offset int64, parents map[string]string) (lines, events int, err error) {
	b, err := st.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			b.Rollback()
		}
	}()
	src := s.Name()
	threadID := threadIDFromName(p)
	if prev.SessionID != "" && prev.AgentID != "" {
		threadID = prev.AgentID
	} else if prev.SessionID != "" {
		threadID = prev.SessionID
	}
	sess := model.Session{Source: src, ID: threadID, Kind: "session"}
	// Where events land: a sub-agent thread that has a known parent is
	// attributed to the parent session with this thread as the agent.
	eventSession, eventAgent := threadID, ""
	if parent, ok := parents[threadID]; ok {
		eventSession, eventAgent = parent, threadID
	}
	agent := model.Agent{Source: src, SessionID: eventSession, ID: threadID}
	var (
		curModel string
		curCwd   string
		lastTS   int64
		lastRL   string
		lineNo   = countLinesBefore(p, offset)
	)
	handle := func(raw []byte) error {
		lines++
		lineNo++
		if !bytes.Contains(raw, []byte(`"session_meta"`)) && !bytes.Contains(raw, []byte(`"turn_context"`)) &&
			!bytes.Contains(raw, []byte(`"token_count"`)) && !bytes.Contains(raw, []byte(`"user_message"`)) {
			return nil
		}
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil
		}
		ts := source.ParseTS(l.Timestamp)
		if ts > lastTS {
			lastTS = ts
		}
		switch l.Type {
		case "session_meta":
			if l.Payload.ID != "" {
				sess.ID = l.Payload.ID
				threadID = l.Payload.ID
				if parent, ok := parents[threadID]; ok {
					eventSession, eventAgent = parent, threadID
				} else {
					eventSession, eventAgent = threadID, ""
				}
				agent.SessionID, agent.ID = eventSession, threadID
			}
			sess.Cwd = l.Payload.Cwd
			curCwd = l.Payload.Cwd
			sess.Workspace = ws.Name(l.Payload.Cwd)
			sess.Version = l.Payload.CLIVersion
			sess.StartedAt = ts
			if l.Payload.Git != nil {
				sess.GitBranch = l.Payload.Git.Branch
			}
			if bytes.Contains(l.Payload.ThreadSource, []byte("subagent")) {
				sess.Kind = "subagent"
				// source: {"subagent": "review"} names the role
				var role struct {
					Subagent string `json:"subagent"`
				}
				if json.Unmarshal(l.Payload.Source, &role) == nil && role.Subagent != "" {
					agent.AgentType = role.Subagent
					agent.Description = "codex " + role.Subagent
				}
			}
		case "turn_context":
			if l.Payload.Model != "" {
				curModel = l.Payload.Model
			}
			if l.Payload.Cwd != "" {
				curCwd = l.Payload.Cwd
			}
		case "event_msg":
			switch l.Payload.Type {
			case "user_message":
				if sess.Title == "" && l.Payload.Message != "" {
					sess.Title = firstLine(l.Payload.Message, 120)
				}
			case "token_count":
				if len(l.Payload.RateLimits) > 2 && string(l.Payload.RateLimits) != "null" {
					rl := string(l.Payload.RateLimits)
					if rl != lastRL {
						lastRL = rl
						if err := b.LimitSnapshot(src, ts, rl); err != nil {
							return err
						}
					}
				}
				if l.Payload.Info == nil || l.Payload.Info.LastTokenUsage == nil {
					return nil
				}
				u := l.Payload.Info.LastTokenUsage
				if u.TotalTokens == 0 && u.InputTokens+u.OutputTokens == 0 {
					return nil
				}
				cwd := curCwd
				if cwd == "" {
					cwd = sess.Cwd
				}
				e := model.Event{
					Source: src, Key: fmt.Sprintf("%s:%d", threadID, lineNo), SessionID: eventSession, AgentID: eventAgent, TS: ts, Model: curModel,
					Cwd: cwd, Workspace: ws.Name(cwd),
					Input: u.InputTokens - u.CachedInputTokens, CacheRead: u.CachedInputTokens, Output: u.OutputTokens, Reasoning: u.ReasoningOutputTokens,
				}
				if e.Input < 0 {
					e.Input = 0
				}
				if curModel != "" && agent.Model == "" {
					agent.Model = curModel
				}
				events++
				return b.Event(e)
			}
		}
		return nil
	}
	newOffset, _, err := source.ReadNewLines(p, offset, handle)
	if err != nil {
		return lines, events, err
	}
	sess.LastAt = lastTS
	if eventAgent != "" {
		sess.ParentID = eventSession
		if err = b.Agent(agent); err != nil {
			return
		}
	}
	if err = b.Session(sess); err != nil {
		return
	}
	fi, statErr := os.Stat(p)
	if statErr != nil {
		err = statErr
		return
	}
	fs := store.FileState{Path: p, Source: src, Size: fi.Size(), MTime: fi.ModTime().UnixMilli(), Offset: newOffset, SessionID: eventSession, AgentID: eventAgent}
	if err = b.File(fs); err != nil {
		return
	}
	err = b.Commit()
	return
}

// countLinesBefore counts newlines in the first offset bytes so event keys
// stay stable across incremental passes.
func countLinesBefore(p string, offset int64) int {
	if offset <= 0 {
		return 0
	}
	f, err := os.Open(p)
	if err != nil {
		return 0
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var n int
	var read int64
	for read < offset {
		want := int64(len(buf))
		if offset-read < want {
			want = offset - read
		}
		k, err := f.Read(buf[:want])
		if k > 0 {
			n += bytes.Count(buf[:k], []byte{'\n'})
			read += int64(k)
		}
		if err != nil {
			break
		}
	}
	return n
}

// parentLinks reads child -> parent thread ids from Codex's state database.
func (s *Source) parentLinks() map[string]string {
	out := map[string]string{}
	path := s.StateDB
	if path == "" {
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(s.Root), "state_*.sqlite"))
		if len(matches) == 0 {
			return out
		}
		path = matches[len(matches)-1]
	}
	if _, err := os.Stat(path); err != nil {
		return out
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return out
	}
	defer db.Close()
	rows, err := db.Query("SELECT parent_thread_id, child_thread_id FROM thread_spawn_edges")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var parent, child string
		if rows.Scan(&parent, &child) == nil {
			out[child] = parent
		}
	}
	return out
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
