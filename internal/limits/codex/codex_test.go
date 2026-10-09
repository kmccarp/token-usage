package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseLive(t *testing.T) {
	body, err := os.ReadFile("testdata/usage.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := parseLive(body)
	if err != nil {
		t.Fatal(err)
	}
	if p.plan != "plus" {
		t.Errorf("plan: %q", p.plan)
	}
	if len(p.windows) != 2 {
		t.Fatalf("want 2 windows, got %+v", p.windows)
	}
	if w := p.windows[0]; w.Key != "primary" || w.Label != "5-hour" || w.Utilization != 4 || w.WindowMinutes != 300 || w.ResetsAt.Unix() != 1791568484 {
		t.Errorf("primary: %+v", w)
	}
	if w := p.windows[1]; w.Key != "secondary" || w.Label != "Weekly" || w.Utilization != 8 || w.WindowMinutes != 10080 {
		t.Errorf("secondary: %+v", w)
	}
	if len(p.notes) != 0 {
		t.Errorf("notes: %+v", p.notes)
	}
}

func writeAuth(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "auth.json")
	auth := `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"id-old","access_token":"access-old","refresh_token":"refresh-old","account_id":"acct-1"},"last_refresh":"2026-09-29T19:43:01.000000Z"}`
	if err := os.WriteFile(path, []byte(auth+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFetchLive(t *testing.T) {
	usage, _ := os.ReadFile("testdata/usage.json")
	var gotAuth, gotAccount string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccount = r.Header.Get("ChatGPT-Account-Id")
		w.Write(usage)
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeAuth(t, dir)
	p := &Provider{CodexDir: dir, UsageURL: srv.URL, Client: srv.Client()}
	l, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer access-old" || gotAccount != "acct-1" {
		t.Errorf("headers: %q %q", gotAuth, gotAccount)
	}
	if l.Plan != "plus" || len(l.Windows) != 2 || l.Windows[1].Utilization != 8 {
		t.Errorf("limits: %+v", l)
	}
}

func TestFetchRefreshesExpiredToken(t *testing.T) {
	usage, _ := os.ReadFile("testdata/usage.json")
	var refreshBody map[string]string
	usageCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/usage", func(w http.ResponseWriter, r *http.Request) {
		usageCalls++
		if r.Header.Get("Authorization") != "Bearer access-new" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"code":"token_expired"}}`))
			return
		}
		w.Write(usage)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&refreshBody)
		w.Write([]byte(`{"id_token":"id-new","access_token":"access-new","refresh_token":"refresh-new"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	dir := t.TempDir()
	path := writeAuth(t, dir)
	p := &Provider{CodexDir: dir, UsageURL: srv.URL + "/usage", RefreshURL: srv.URL + "/token", Client: srv.Client()}
	l, err := p.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if usageCalls != 2 || len(l.Windows) != 2 {
		t.Errorf("calls=%d limits=%+v", usageCalls, l)
	}
	if refreshBody["grant_type"] != "refresh_token" || refreshBody["refresh_token"] != "refresh-old" || refreshBody["client_id"] != clientID {
		t.Errorf("refresh request: %+v", refreshBody)
	}
	b, _ := os.ReadFile(path)
	var saved struct {
		AuthMode string `json:"auth_mode"`
		Tokens   struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.AuthMode != "chatgpt" || saved.Tokens.AccessToken != "access-new" || saved.Tokens.RefreshToken != "refresh-new" || saved.Tokens.IDToken != "id-new" || saved.Tokens.AccountID != "acct-1" {
		t.Errorf("saved auth: %s", b)
	}
	if saved.LastRefresh == "2026-09-29T19:43:01.000000Z" {
		t.Errorf("last_refresh not updated")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
}

func TestFetchFallsBackWithoutStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	dir := t.TempDir()
	writeAuth(t, dir)
	p := &Provider{CodexDir: dir, UsageURL: srv.URL, Client: srv.Client()}
	if _, err := p.Fetch(context.Background()); err == nil {
		t.Fatal("want error when live fails and there is no store")
	}
}
