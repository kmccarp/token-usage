package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kmccarp/token-usage/internal/store"
	"github.com/kmccarp/token-usage/internal/workspace"
)

const mainTranscript = `{"type":"user","uuid":"u1","sessionId":"s1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:00:00.000Z","gitBranch":"ws1","version":"2.1.280","message":{"role":"user","content":"do the thing\nmore"}}
{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:00:05.000Z","requestId":"r1","message":{"id":"m1","model":"claude-fable-5-1","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":10,"cache_creation":{"ephemeral_1h_input_tokens":100,"ephemeral_5m_input_tokens":0}}}}
{"type":"assistant","uuid":"a2","parentUuid":"u1","sessionId":"s1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:00:05.000Z","requestId":"r1","message":{"id":"m1","model":"claude-fable-5-1","content":[{"type":"tool_use","id":"toolu_1","name":"Agent","input":{"description":"Review it","subagent_type":"general-purpose"}}],"usage":{"input_tokens":2,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":10}}}
{"type":"user","uuid":"u2","sessionId":"s1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:00:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"launched"}]},"toolUseResult":{"agentId":"ag1","description":"Review it","resolvedModel":"claude-fable-5-1"}}
{"type":"assistant","uuid":"a3","sessionId":"s1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:00:07.000Z","requestId":"r2","message":{"id":"m2","model":"<synthetic>","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"ai-title","aiTitle":"The thing","sessionId":"s1"}
`

const agentTranscript = `{"type":"user","uuid":"au1","sessionId":"s1","isSidechain":true,"agentId":"ag1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:00:06.500Z","message":{"role":"user","content":"Review it"}}
{"type":"assistant","uuid":"aa1","sessionId":"s1","isSidechain":true,"agentId":"ag1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:00:08.000Z","requestId":"r3","message":{"id":"m3","model":"claude-fable-5-1","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":50,"cache_read_input_tokens":500,"output_tokens":20}}}
`

func TestScanMainAndSubagent(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-Users-alice-worktrees-repo-ws1")
	if err := os.MkdirAll(filepath.Join(proj, "s1", "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "s1.jsonl"), []byte(mainTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "s1", "subagents", "agent-ag1.jsonl"), []byte(agentTranscript), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws, _ := workspace.NewForHome("/Users/alice", nil)
	src := &Source{Root: root}
	ctx := context.Background()

	res, err := src.Scan(ctx, st, ws)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesChanged != 2 {
		t.Errorf("files changed: %+v", res)
	}
	total, err := st.Totals(ctx, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// m1 counted once despite two lines; synthetic skipped; agent counted
	if total.Requests != 2 || total.Total != (2+100+1000+10)+(5+50+500+20) {
		t.Errorf("totals: %+v", total)
	}
	main, _ := st.Totals(ctx, store.Filter{AgentID: "-"})
	if main.Requests != 1 || main.Total != 1112 {
		t.Errorf("main: %+v", main)
	}
	agents, err := st.Agents(ctx, "claude", "s1")
	if err != nil {
		t.Fatal(err)
	}
	a := agents["ag1"]
	if a.Description != "Review it" || a.AgentType != "general-purpose" || a.Model != "claude-fable-5-1" {
		t.Errorf("agent: %+v", a)
	}
	sess, ok, _ := st.Session(ctx, "claude", "s1")
	if !ok || sess.Title != "The thing" || sess.Workspace != "ws1" || sess.GitBranch != "ws1" {
		t.Errorf("session: %+v", sess)
	}

	// second scan: nothing changed
	res, err = src.Scan(ctx, st, ws)
	if err != nil || res.FilesChanged != 0 {
		t.Errorf("rescan: %+v %v", res, err)
	}

	// append a new request and a partial line; only the complete one is counted
	f, _ := os.OpenFile(filepath.Join(proj, "s1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"type":"assistant","uuid":"a9","sessionId":"s1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:01:00.000Z","requestId":"r9","message":{"id":"m9","model":"claude-opus-5","content":[],"usage":{"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}` + "\n")
	f.WriteString(`{"type":"assistant","uuid":"a10","sessionId":"s1","cwd":"/Users/alice/worktrees/repo/ws1","timestamp":"2026-09-22T10:01:01.000Z","requestId":"r10","message":{"id":"m10","model":"claude-opus-5","content":[],"usage":{"input_tokens":1,"cache_creation_input`)
	f.Close()
	res, err = src.Scan(ctx, st, ws)
	if err != nil || res.Events != 1 {
		t.Errorf("incremental: %+v %v", res, err)
	}
	total, _ = st.Totals(ctx, store.Filter{})
	if total.Requests != 3 {
		t.Errorf("after append: %+v", total)
	}
	// complete the partial line
	f, _ = os.OpenFile(filepath.Join(proj, "s1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`_tokens":0,"cache_read_input_tokens":0,"output_tokens":1}}}` + "\n")
	f.Close()
	res, _ = src.Scan(ctx, st, ws)
	if res.Events != 1 {
		t.Errorf("completed line: %+v", res)
	}
	total, _ = st.Totals(ctx, store.Filter{})
	if total.Requests != 4 {
		t.Errorf("after completing: %+v", total)
	}
}
