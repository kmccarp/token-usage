// Package server exposes the index over HTTP and serves the embedded UI.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kmccarp/token-usage/internal/limits"
	"github.com/kmccarp/token-usage/internal/model"
	"github.com/kmccarp/token-usage/internal/source"
	"github.com/kmccarp/token-usage/internal/store"
)

//go:embed ui
var uiFS embed.FS

// SourceInfo is what the UI needs to know about a source.
type SourceInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// Server is the HTTP layer.
type Server struct {
	Store   *store.Store
	Poller  *limits.Poller
	Sources []SourceInfo
	Version string

	mu       sync.RWMutex
	lastScan map[string]source.ScanResult
	scanAt   time.Time
	scanning bool
	Scan     func(ctx context.Context) // trigger a scan now (optional)
}

// RecordScan stores the latest scan result for status reporting.
func (s *Server) RecordScan(name string, r source.ScanResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastScan == nil {
		s.lastScan = map[string]source.ScanResult{}
	}
	s.lastScan[name] = r
	s.scanAt = time.Now()
}

// SetScanning flags whether a scan pass is in progress.
func (s *Server) SetScanning(v bool) {
	s.mu.Lock()
	s.scanning = v
	s.mu.Unlock()
}

// Handler builds the mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/limits", s.handleLimits)
	mux.HandleFunc("GET /api/summary", s.handleSummary)
	mux.HandleFunc("GET /api/breakdown", s.handleBreakdown)
	mux.HandleFunc("GET /api/timeseries", s.handleTimeseries)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("POST /api/refresh", s.handleRefresh)
	sub, _ := fs.Sub(uiFS, "ui")
	fileServer := http.FileServer(http.FS(sub))
	mux.Handle("GET /", noCache(fileServer))
	return logRequests(mux)
}

func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s %s", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", " ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// timeRange is the resolved [From, To) window plus a description of it.
type timeRange struct {
	From  int64  `json:"from"`
	To    int64  `json:"to"`
	Label string `json:"label"`
}

// resolveRange turns query params into a window.
//
//	range=5h|24h|today|7d|30d|all       rolling windows (today = local midnight)
//	range=window:<source>:<key>         aligned to that provider's reset time
//	from=<ms>&to=<ms>                   explicit
func (s *Server) resolveRange(q map[string][]string) timeRange {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	now := time.Now()
	if f := get("from"); f != "" {
		from, _ := strconv.ParseInt(f, 10, 64)
		to, _ := strconv.ParseInt(get("to"), 10, 64)
		if to == 0 {
			to = now.UnixMilli()
		}
		return timeRange{From: from, To: to, Label: "custom"}
	}
	r := get("range")
	if r == "" {
		r = "5h"
	}
	if strings.HasPrefix(r, "window:") {
		parts := strings.SplitN(r, ":", 3)
		if len(parts) == 3 && s.Poller != nil {
			for _, l := range s.Poller.Latest() {
				if l.Source != parts[1] {
					continue
				}
				for _, w := range l.Windows {
					if w.Key == parts[2] && w.WindowMinutes > 0 {
						return windowRange(w, now)
					}
				}
			}
		}
		r = "5h"
	}
	switch r {
	case "today":
		y, m, d := now.Date()
		start := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		return timeRange{From: start.UnixMilli(), To: now.UnixMilli(), Label: "today"}
	case "all":
		return timeRange{From: 0, To: now.UnixMilli(), Label: "all time"}
	}
	d, err := time.ParseDuration(r)
	if err != nil {
		if strings.HasSuffix(r, "d") {
			if n, e := strconv.Atoi(strings.TrimSuffix(r, "d")); e == nil {
				d, err = time.Duration(n)*24*time.Hour, nil
			}
		}
	}
	if err != nil || d <= 0 {
		d = 5 * time.Hour
		r = "5h"
	}
	return timeRange{From: now.Add(-d).UnixMilli(), To: now.UnixMilli(), Label: "last " + r}
}

// windowRange is the provider window that is currently open: it ends at the
// reset time and started one window-length earlier. When the reset is in the
// past (stale snapshot) the window is rolled forward to contain now.
func windowRange(w model.Window, now time.Time) timeRange {
	length := time.Duration(w.WindowMinutes) * time.Minute
	if !w.StartedAt.IsZero() && w.StartedAt.Before(now) && now.Sub(w.StartedAt) <= length {
		return timeRange{From: w.StartedAt.UnixMilli(), To: now.UnixMilli(), Label: w.Label + " window"}
	}
	end := w.ResetsAt
	if end.IsZero() {
		return timeRange{From: now.Add(-length).UnixMilli(), To: now.UnixMilli(), Label: w.Label + " (rolling)"}
	}
	for end.Before(now) {
		end = end.Add(length)
	}
	start := end.Add(-length)
	return timeRange{From: start.UnixMilli(), To: now.UnixMilli(), Label: w.Label + " window"}
}

func filterFrom(q map[string][]string, tr timeRange) store.Filter {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	return store.Filter{
		From: tr.From, To: tr.To,
		Source: get("source"), Workspace: get("workspace"), SessionID: get("session"),
		AgentID: get("agent"), Model: get("model"), Dir: get("dir"),
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.mu.RLock()
	scans := make(map[string]source.ScanResult, len(s.lastScan))
	for k, v := range s.lastScan {
		scans[k] = v
	}
	scanAt, scanning := s.scanAt, s.scanning
	s.mu.RUnlock()
	writeJSON(w, 200, map[string]any{
		"version":   s.Version,
		"now":       time.Now().UnixMilli(),
		"stats":     st,
		"sources":   s.Sources,
		"last_scan": scans,
		"scan_at":   scanAt,
		"scanning":  scanning,
	})
}

// limitView is a provider window plus the local token totals inside it.
type limitView struct {
	model.Window
	Range timeRange    `json:"range"`
	Local model.Totals `json:"local"`
}

type limitsView struct {
	Source    string        `json:"source"`
	Plan      string        `json:"plan,omitempty"`
	FetchedAt time.Time     `json:"fetched_at"`
	Error     string        `json:"error,omitempty"`
	Windows   []limitView   `json:"windows"`
	Shares    []model.Share `json:"shares,omitempty"`
	Notes     []string      `json:"notes,omitempty"`
}

func (s *Server) handleLimits(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	var out []limitsView
	if s.Poller != nil {
		for _, l := range s.Poller.Latest() {
			lv := limitsView{Source: l.Source, Plan: l.Plan, FetchedAt: l.FetchedAt, Error: l.Error, Windows: []limitView{}, Shares: l.Shares, Notes: l.Notes}
			for _, win := range l.Windows {
				v := limitView{Window: win}
				if win.WindowMinutes > 0 {
					v.Range = windowRange(win, now)
					f := store.Filter{From: v.Range.From, To: v.Range.To, Source: l.Source}
					if win.Scope != "" {
						// a model-scoped cap: count only that model family's tokens
						f.ModelLike = strings.ToLower(strings.Fields(win.Scope)[0])
					}
					t, err := s.Store.Totals(r.Context(), f)
					if err != nil {
						writeErr(w, 500, err)
						return
					}
					v.Local = t
				}
				lv.Windows = append(lv.Windows, v)
			}
			out = append(out, lv)
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tr := s.resolveRange(q)
	f := filterFrom(q, tr)
	total, err := s.Store.Totals(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	bySource, err := s.Store.GroupBy(r.Context(), "source", f, 0)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	byModel, err := s.Store.GroupBy(r.Context(), "model", f, 0)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"range": tr, "totals": total, "by_source": orEmpty(bySource), "by_model": orEmpty(byModel)})
}

func orEmpty(g []store.Group) []store.Group {
	if g == nil {
		return []store.Group{}
	}
	return g
}

func (s *Server) handleBreakdown(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tr := s.resolveRange(q)
	f := filterFrom(q, tr)
	by := q.Get("by")
	if by == "" {
		by = "workspace"
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	ctx := r.Context()
	var groups []store.Group
	var err error
	switch by {
	case "directory":
		groups, err = s.directoryBreakdown(ctx, f)
	case "session":
		groups, err = s.Store.GroupBy(ctx, "session", f, limit)
		if err == nil {
			err = s.decorateSessions(ctx, f.Source, groups)
		}
	case "agent":
		groups, err = s.Store.GroupBy(ctx, "agent", f, limit)
		if err == nil {
			err = s.decorateAgents(ctx, f.Source, f.SessionID, groups)
		}
	default:
		groups, err = s.Store.GroupBy(ctx, by, f, limit)
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	total, err := s.Store.Totals(ctx, f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"range": tr, "by": by, "totals": total, "groups": orEmpty(groups)})
}

// directoryBreakdown aggregates by the next path component under f.Dir.
func (s *Server) directoryBreakdown(ctx context.Context, f store.Filter) ([]store.Group, error) {
	byCwd, err := s.Store.GroupBy(ctx, "cwd", f, 0)
	if err != nil {
		return nil, err
	}
	prefix := strings.TrimRight(f.Dir, "/")
	agg := map[string]*store.Group{}
	for _, g := range byCwd {
		cwd := strings.TrimRight(g.Key, "/")
		var key, label string
		leaf := false
		switch {
		case cwd == prefix || cwd == "":
			key, label, leaf = prefix, "(this directory)", true
			if cwd == "" {
				key, label = "", "(unknown)"
			}
		case prefix == "":
			// top level: first component
			rest := strings.TrimPrefix(cwd, "/")
			comp := rest
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				comp = rest[:i]
			} else {
				leaf = true
			}
			key, label = "/"+comp, "/"+comp
		default:
			rest := strings.TrimPrefix(strings.TrimPrefix(cwd, prefix), "/")
			comp := rest
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				comp = rest[:i]
			} else {
				leaf = true
			}
			key, label = prefix+"/"+comp, comp
		}
		a, ok := agg[key]
		if !ok {
			a = &store.Group{Key: key, Label: label, Meta: map[string]any{"leaf": leaf, "cwds": 0}}
			agg[key] = a
		}
		m := a.Meta.(map[string]any)
		m["cwds"] = m["cwds"].(int) + 1
		if !leaf {
			m["leaf"] = false
		}
		add(&a.Totals, g.Totals)
	}
	out := make([]store.Group, 0, len(agg))
	for _, g := range agg {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Totals.Total > out[j].Totals.Total })
	return out, nil
}

func add(dst *model.Totals, src model.Totals) {
	dst.Requests += src.Requests
	dst.Input += src.Input
	dst.CacheCreate += src.CacheCreate
	dst.CacheRead += src.CacheRead
	dst.Output += src.Output
	dst.Reasoning += src.Reasoning
	dst.Total += src.Total
	if dst.FirstTS == 0 || (src.FirstTS > 0 && src.FirstTS < dst.FirstTS) {
		dst.FirstTS = src.FirstTS
	}
	if src.LastTS > dst.LastTS {
		dst.LastTS = src.LastTS
	}
}

func (s *Server) decorateSessions(ctx context.Context, sourceName string, groups []store.Group) error {
	// sessions from several sources can share the list when no source filter is set
	bySource := map[string][]int{}
	if sourceName != "" {
		for i := range groups {
			bySource[sourceName] = append(bySource[sourceName], i)
		}
	} else {
		for _, src := range s.Sources {
			for i := range groups {
				bySource[src.Name] = append(bySource[src.Name], i)
			}
		}
	}
	for src, idxs := range bySource {
		ids := make([]string, 0, len(idxs))
		for _, i := range idxs {
			ids = append(ids, groups[i].Key)
		}
		meta, err := s.Store.Sessions(ctx, src, ids)
		if err != nil {
			return err
		}
		for _, i := range idxs {
			if m, ok := meta[groups[i].Key]; ok {
				groups[i].Meta = m
				groups[i].Label = m.Title
				if groups[i].Label == "" {
					groups[i].Label = path.Base(m.Cwd) + " · " + shortID(m.ID)
				}
			}
		}
	}
	for i := range groups {
		if groups[i].Label == "" {
			groups[i].Label = shortID(groups[i].Key)
		}
	}
	return nil
}

func (s *Server) decorateAgents(ctx context.Context, sourceName, sessionID string, groups []store.Group) error {
	var agents map[string]model.Agent
	if sourceName != "" && sessionID != "" {
		var err error
		agents, err = s.Store.Agents(ctx, sourceName, sessionID)
		if err != nil {
			return err
		}
	}
	for i := range groups {
		if groups[i].Key == "" {
			groups[i].Label = "main thread"
			continue
		}
		if a, ok := agents[groups[i].Key]; ok {
			groups[i].Meta = a
			groups[i].Label = a.Description
			if a.AgentType != "" {
				groups[i].Label = a.AgentType + ": " + groups[i].Label
			}
		}
		if strings.TrimSpace(strings.TrimSuffix(groups[i].Label, ":")) == "" {
			groups[i].Label = "agent " + shortID(groups[i].Key)
		}
	}
	return nil
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func (s *Server) handleTimeseries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tr := s.resolveRange(q)
	f := filterFrom(q, tr)
	by := q.Get("by")
	if by == "" {
		by = "source"
	}
	var bucket int64
	switch q.Get("bucket") {
	case "day":
		bucket = 24 * time.Hour.Milliseconds()
	case "15m":
		bucket = 15 * time.Minute.Milliseconds()
	case "hour", "":
		bucket = time.Hour.Milliseconds()
	default:
		d, err := time.ParseDuration(q.Get("bucket"))
		if err != nil || d <= 0 {
			writeErr(w, 400, errors.New("bad bucket"))
			return
		}
		bucket = d.Milliseconds()
	}
	rows, err := s.Store.TimeSeries(r.Context(), bucket, by, f)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if rows == nil {
		rows = []store.Bucket{}
	}
	writeJSON(w, 200, map[string]any{"range": tr, "bucket_ms": bucket, "by": by, "rows": rows})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	src, id := q.Get("source"), q.Get("id")
	if src == "" || id == "" {
		writeErr(w, 400, errors.New("source and id are required"))
		return
	}
	ctx := r.Context()
	sess, ok, err := s.Store.Session(ctx, src, id)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if !ok {
		writeErr(w, 404, errors.New("session not found"))
		return
	}
	f := store.Filter{Source: src, SessionID: id}
	total, err := s.Store.Totals(ctx, f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	agents, err := s.Store.GroupBy(ctx, "agent", f, 0)
	if err == nil {
		err = s.decorateAgents(ctx, src, id, agents)
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	models, err := s.Store.GroupBy(ctx, "model", f, 0)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"session": sess, "totals": total, "agents": orEmpty(agents), "models": orEmpty(models)})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if s.Scan != nil {
		go s.Scan(context.Background())
	}
	if s.Poller != nil {
		go s.Poller.Refresh(context.Background())
	}
	writeJSON(w, 202, map[string]string{"status": "refreshing"})
}
