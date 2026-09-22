package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kmccarp/token-usage/internal/store"
	"github.com/kmccarp/token-usage/internal/workspace"
)

const rollout = `{"timestamp":"2026-09-21T22:12:10.339Z","type":"session_meta","payload":{"id":"01a0c606-cf74-7032-8370-d74014ae3dd8","timestamp":"2026-09-21T22:12:10.257Z","cwd":"/Users/kevin/dev/git/repo","originator":"codex_exec","cli_version":"0.139.0","source":"exec","thread_source":"user","model_provider":"openai","git":{"branch":"main"}}}
{"timestamp":"2026-09-21T22:12:14.864Z","type":"turn_context","payload":{"turn_id":"t1","cwd":"/Users/kevin/dev/git/repo","model":"gpt-5.5"}}
{"timestamp":"2026-09-21T22:12:15.000Z","type":"event_msg","payload":{"type":"user_message","message":"fix the bug"}}
{"timestamp":"2026-09-21T22:12:23.807Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":14636,"cached_input_tokens":2432,"output_tokens":288,"reasoning_output_tokens":0,"total_tokens":14924},"last_token_usage":{"input_tokens":14636,"cached_input_tokens":2432,"output_tokens":288,"reasoning_output_tokens":0,"total_tokens":14924}},"rate_limits":{"primary":{"used_percent":12.0,"window_minutes":300,"resets_at":1790046736},"secondary":{"used_percent":3.0,"window_minutes":10080,"resets_at":1790544164},"plan_type":"plus"}}}
{"timestamp":"2026-09-21T22:12:33.816Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":33704,"cached_input_tokens":16128,"output_tokens":621,"reasoning_output_tokens":27,"total_tokens":34325},"last_token_usage":{"input_tokens":19068,"cached_input_tokens":13696,"output_tokens":333,"reasoning_output_tokens":27,"total_tokens":19401}},"rate_limits":{"primary":{"used_percent":13.0,"window_minutes":300,"resets_at":1790046744},"secondary":{"used_percent":3.0,"window_minutes":10080,"resets_at":1790544164},"plan_type":"plus"}}}
`

func TestScanRollout(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "09", "21")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "rollout-2026-09-21T19-12-10-01a0c606-cf74-7032-8370-d74014ae3dd8.jsonl")
	if err := os.WriteFile(p, []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ws, _ := workspace.New(nil)
	src := &Source{Root: root, StateDB: filepath.Join(root, "missing.sqlite")}
	ctx := context.Background()
	res, err := src.Scan(ctx, st, ws)
	if err != nil || res.Events != 2 {
		t.Fatalf("scan: %+v %v", res, err)
	}
	total, _ := st.Totals(ctx, store.Filter{Source: "codex"})
	// input is net of cached; total = input + cache_read + output = raw input + output
	if total.Requests != 2 || total.Input != (14636-2432)+(19068-13696) || total.CacheRead != 2432+13696 || total.Output != 288+333 || total.Reasoning != 27 {
		t.Errorf("totals: %+v", total)
	}
	if total.Total != 14636+288+19068+333 {
		t.Errorf("total mismatch: %d", total.Total)
	}
	sess, ok, _ := st.Session(ctx, "codex", "01a0c606-cf74-7032-8370-d74014ae3dd8")
	if !ok || sess.Title != "fix the bug" || sess.Workspace != "repo" || sess.GitBranch != "main" {
		t.Errorf("session: %+v", sess)
	}
	byModel, _ := st.GroupBy(ctx, "model", store.Filter{}, 0)
	if len(byModel) != 1 || byModel[0].Key != "gpt-5.5" {
		t.Errorf("model: %+v", byModel)
	}
	snap, _ := st.LatestLimitSnapshot("codex")
	if snap.Raw == "" || snap.TS == 0 {
		t.Errorf("snapshot: %+v", snap)
	}
	// stable keys on rescan after a truncate-and-rewrite
	if err := os.WriteFile(p, []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, time.Now().Add(time.Second), time.Now().Add(time.Second))
	if _, err := src.Scan(ctx, st, ws); err != nil {
		t.Fatal(err)
	}
	total, _ = st.Totals(ctx, store.Filter{Source: "codex"})
	if total.Requests != 2 {
		t.Errorf("after rewrite: %+v", total)
	}
}
