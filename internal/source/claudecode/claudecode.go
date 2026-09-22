// Package claudecode scans Claude Code transcripts under ~/.claude/projects.
//
// Layout:
//
//	<projects>/<project-dir>/<session-id>.jsonl                          main thread
//	<projects>/<project-dir>/<session-id>/subagents/agent-<id>.jsonl     one file per sub-agent
//
// Every assistant line carries the usage for one API call. A single call is
// written as several lines (one per content block) that share message.id, so
// message.id is the dedupe key.
package claudecode

import (
	"bytes"
	"context"
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

// Source scans Claude Code.
type Source struct {
	Root string // ~/.claude/projects
}

// Name implements source.Source.
func (s *Source) Name() string { return "claude" }

// Label implements source.Source.
func (s *Source) Label() string { return "Claude Code" }

type usage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
	CacheCreation struct {
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
}

type contentBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Name      string `json:"name"`
	ToolUseID string `json:"tool_use_id"`
	Input     struct {
		Description  string `json:"description"`
		SubagentType string `json:"subagent_type"`
	} `json:"input"`
}

type line struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	SessionID   string `json:"sessionId"`
	Cwd         string `json:"cwd"`
	Timestamp   string `json:"timestamp"`
	RequestID   string `json:"requestId"`
	IsSidechain bool   `json:"isSidechain"`
	AgentID     string `json:"agentId"`
	GitBranch   string `json:"gitBranch"`
	Version     string `json:"version"`
	AITitle     string `json:"aiTitle"`
	Message     struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Usage   *usage          `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	ToolUseResult *struct {
		AgentID       string `json:"agentId"`
		Description   string `json:"description"`
		ResolvedModel string `json:"resolvedModel"`
	} `json:"toolUseResult"`
}

// fileInfo is what the path tells us before reading a byte.
type fileInfo struct {
	projectDir string
	sessionID  string
	agentID    string
}

func classify(root, p string) (fileInfo, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return fileInfo{}, false
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	base := strings.TrimSuffix(parts[len(parts)-1], ".jsonl")
	switch {
	case len(parts) == 2:
		return fileInfo{projectDir: parts[0], sessionID: base}, true
	case len(parts) == 4 && parts[2] == "subagents" && strings.HasPrefix(base, "agent-"):
		return fileInfo{projectDir: parts[0], sessionID: parts[1], agentID: strings.TrimPrefix(base, "agent-")}, true
	}
	return fileInfo{}, false
}

// Scan implements source.Source.
func (s *Source) Scan(ctx context.Context, st *store.Store, ws *workspace.Mapper) (source.ScanResult, error) {
	start := time.Now()
	var res source.ScanResult
	root := s.Root
	paths, err := source.WalkJSONL(root, func(p string) bool { _, ok := classify(root, p); return ok })
	if err != nil {
		return res, err
	}
	states, err := st.FileStates(s.Name())
	if err != nil {
		return res, err
	}
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
			offset = 0 // new, truncated, or rewritten
		}
		info, _ := classify(root, p)
		n, ev, newOff, err := s.scanFile(st, ws, p, info, offset)
		res.Lines += n
		res.Events += ev
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		res.FilesChanged++
		_ = newOff
	}
	res.Duration = time.Since(start)
	return res, nil
}

func (s *Source) scanFile(st *store.Store, ws *workspace.Mapper, p string, info fileInfo, offset int64) (lines, events int, newOffset int64, err error) {
	b, err := st.Begin()
	if err != nil {
		return 0, 0, offset, err
	}
	defer func() {
		if err != nil {
			b.Rollback()
		}
	}()
	src := s.Name()
	sess := model.Session{Source: src, ID: info.sessionID, ProjectDir: info.projectDir, Kind: "session"}
	var lastTS int64
	handle := func(raw []byte) error {
		lines++
		isAssistant := bytes.Contains(raw, []byte(`"type":"assistant"`))
		isUser := bytes.Contains(raw, []byte(`"type":"user"`))
		isTitle := bytes.Contains(raw, []byte(`"type":"ai-title"`))
		if !isAssistant && !isUser && !isTitle {
			return nil
		}
		var l line
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil // tolerate junk lines
		}
		ts := source.ParseTS(l.Timestamp)
		if ts > lastTS {
			lastTS = ts
		}
		if l.Cwd != "" && sess.Cwd == "" {
			sess.Cwd = l.Cwd
			sess.Workspace = ws.Name(l.Cwd)
		}
		if l.GitBranch != "" && sess.GitBranch == "" {
			sess.GitBranch = l.GitBranch
		}
		if l.Version != "" {
			sess.Version = l.Version
		}
		if ts > 0 && info.agentID == "" && (sess.StartedAt == 0 || ts < sess.StartedAt) {
			sess.StartedAt = ts
		}
		switch l.Type {
		case "ai-title":
			if l.AITitle != "" {
				sess.Title = l.AITitle
			}
		case "user":
			if info.agentID == "" && sess.Title == "" && len(l.Message.Content) > 0 && l.Message.Content[0] == '"' {
				var text string
				if json.Unmarshal(l.Message.Content, &text) == nil {
					sess.Title = firstLine(text, 120)
				}
			}
			if l.ToolUseResult != nil && l.ToolUseResult.AgentID != "" {
				a := model.Agent{Source: src, SessionID: info.sessionID, ID: l.ToolUseResult.AgentID, Description: l.ToolUseResult.Description, Model: l.ToolUseResult.ResolvedModel}
				var blocks []contentBlock
				if json.Unmarshal(l.Message.Content, &blocks) == nil {
					for _, cb := range blocks {
						if cb.Type == "tool_result" && cb.ToolUseID != "" {
							if t, d, ok := b.LookupToolUse(src, cb.ToolUseID); ok {
								a.AgentType = t
								if a.Description == "" {
									a.Description = d
								}
							}
						}
					}
				}
				if err := b.Agent(a); err != nil {
					return err
				}
			}
		case "assistant":
			if len(l.Message.Content) > 0 && l.Message.Content[0] == '[' && bytes.Contains(l.Message.Content, []byte(`"name":"Agent"`)) {
				var blocks []contentBlock
				if json.Unmarshal(l.Message.Content, &blocks) == nil {
					for _, cb := range blocks {
						if cb.Type == "tool_use" && cb.Name == "Agent" && cb.ID != "" {
							if err := b.ToolUse(src, cb.ID, cb.Input.SubagentType, cb.Input.Description); err != nil {
								return err
							}
						}
					}
				}
			}
			u := l.Message.Usage
			if u == nil || l.Message.Model == "" || strings.HasPrefix(l.Message.Model, "<") {
				return nil
			}
			if u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens+u.OutputTokens == 0 {
				return nil
			}
			key := l.Message.ID
			if key == "" {
				key = l.RequestID
			}
			if key == "" {
				key = l.UUID
			}
			agent := info.agentID
			if l.AgentID != "" {
				agent = l.AgentID
			} else if agent == "" && l.IsSidechain {
				agent = "sidechain"
			}
			cwd := l.Cwd
			if cwd == "" {
				cwd = sess.Cwd
			}
			e := model.Event{
				Source: src, Key: key, SessionID: info.sessionID, AgentID: agent, TS: ts, Model: l.Message.Model,
				Cwd: cwd, Workspace: ws.Name(cwd),
				Input: u.InputTokens, CacheCreate: u.CacheCreationInputTokens, CacheRead: u.CacheReadInputTokens, Output: u.OutputTokens,
				Reasoning: u.OutputTokensDetails.ThinkingTokens, Cache1h: u.CacheCreation.Ephemeral1h, Cache5m: u.CacheCreation.Ephemeral5m,
			}
			events++
			return b.Event(e)
		}
		return nil
	}
	newOffset, _, err = source.ReadNewLines(p, offset, handle)
	if err != nil {
		return lines, events, offset, err
	}
	if agent := info.agentID; agent != "" {
		// make sure the agent row exists even if the parent's launch record was missed
		if err = b.Agent(model.Agent{Source: src, SessionID: info.sessionID, ID: agent}); err != nil {
			return
		}
	}
	sess.LastAt = lastTS
	if err = b.Session(sess); err != nil {
		return
	}
	fi, statErr := os.Stat(p)
	if statErr != nil {
		err = statErr
		return
	}
	if err = b.File(store.FileState{Path: p, Source: src, Size: fi.Size(), MTime: fi.ModTime().UnixMilli(), Offset: newOffset, SessionID: info.sessionID, AgentID: info.agentID}); err != nil {
		return
	}
	err = b.Commit()
	return
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
