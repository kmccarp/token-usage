// Package codex reports Codex rate limits from the usage endpoint the Codex CLI uses, falling back to the newest rollout snapshot.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kmccarp/token-usage/internal/model"
	"github.com/kmccarp/token-usage/internal/store"
)

const (
	defaultUsageURL   = "https://chatgpt.com/backend-api/wham/usage"
	defaultRefreshURL = "https://auth.openai.com/oauth/token"
	// clientID is the OAuth client the Codex CLI registers as.
	clientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// Provider polls the usage endpoint and falls back to the store.
type Provider struct {
	Store    *store.Store
	CodexDir string        // ~/.codex; empty means store-only
	Every    time.Duration // poll interval
	Client   *http.Client

	UsageURL   string // test override
	RefreshURL string // test override

	mu sync.Mutex
}

// Source implements limits.Provider.
func (p *Provider) Source() string { return "codex" }

// Interval implements limits.Provider.
func (p *Provider) Interval() time.Duration {
	if p.Every == 0 {
		if p.CodexDir == "" {
			return 30 * time.Second
		}
		return 2 * time.Minute
	}
	return p.Every
}

// Fetch implements limits.Provider.
func (p *Provider) Fetch(ctx context.Context) (model.Limits, error) {
	if p.CodexDir == "" {
		return p.fromStore()
	}
	l, liveErr := p.fromLive(ctx)
	if liveErr == nil {
		return l, nil
	}
	l, err := p.fromStore()
	if err != nil {
		return l, fmt.Errorf("%v; and no rollout snapshot: %v", liveErr, err)
	}
	l.Notes = append(l.Notes, "live usage unavailable ("+liveErr.Error()+"); showing the last rollout snapshot")
	return l, nil
}

// fromLive calls the usage endpoint, refreshing the token once if it has expired.
func (p *Provider) fromLive(ctx context.Context) (model.Limits, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := model.Limits{Source: p.Source(), FetchedAt: time.Now()}
	auth, err := p.readAuth()
	if err != nil {
		return l, err
	}
	status, body, err := p.getUsage(ctx, auth)
	if err != nil {
		return l, err
	}
	if status == http.StatusUnauthorized && bytes.Contains(body, []byte("token_expired")) {
		if err := p.refresh(ctx, auth); err != nil {
			return l, fmt.Errorf("Codex token expired and refresh failed: %w", err)
		}
		status, body, err = p.getUsage(ctx, auth)
		if err != nil {
			return l, err
		}
	}
	if status/100 != 2 {
		return l, fmt.Errorf("usage endpoint returned %d: %s", status, truncate(string(body), 200))
	}
	parsed, err := parseLive(body)
	if err != nil {
		return l, err
	}
	l.Plan = parsed.plan
	l.Windows = parsed.windows
	l.Notes = parsed.notes
	l.Raw = json.RawMessage(body)
	return l, nil
}

type authFile struct {
	path   string
	fields map[string]json.RawMessage
	tokens struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	}
}

func (p *Provider) readAuth() (*authFile, error) {
	a := &authFile{path: filepath.Join(p.CodexDir, "auth.json")}
	b, err := os.ReadFile(a.path)
	if err != nil {
		return nil, errors.New("no Codex login found (" + a.path + ")")
	}
	if err := json.Unmarshal(b, &a.fields); err != nil {
		return nil, fmt.Errorf("parse %s: %w", a.path, err)
	}
	if raw, ok := a.fields["tokens"]; ok {
		if err := json.Unmarshal(raw, &a.tokens); err != nil {
			return nil, fmt.Errorf("parse %s tokens: %w", a.path, err)
		}
	}
	if a.tokens.AccessToken == "" {
		return nil, errors.New("Codex auth.json has no access token; run codex login")
	}
	return a, nil
}

func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (p *Provider) getUsage(ctx context.Context, a *authFile) (int, []byte, error) {
	u := p.UsageURL
	if u == "" {
		u = defaultUsageURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.tokens.AccessToken)
	if a.tokens.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", a.tokens.AccountID)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs")
	resp, err := p.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, nil
}

// refresh exchanges the refresh token for new tokens and writes them back to auth.json.
func (p *Provider) refresh(ctx context.Context, a *authFile) error {
	if a.tokens.RefreshToken == "" {
		return errors.New("auth.json has no refresh token; run codex login")
	}
	u := p.RefreshURL
	if u == "" {
		u = defaultRefreshURL
	}
	payload, _ := json.Marshal(map[string]string{
		"client_id":     clientID,
		"grant_type":    "refresh_token",
		"refresh_token": a.tokens.RefreshToken,
		"scope":         "openid profile email",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("refresh returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	var nt struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &nt); err != nil || nt.AccessToken == "" {
		return errors.New("refresh response had no access_token")
	}
	a.tokens.AccessToken = nt.AccessToken
	if nt.IDToken != "" {
		a.tokens.IDToken = nt.IDToken
	}
	if nt.RefreshToken != "" {
		a.tokens.RefreshToken = nt.RefreshToken
	}
	return a.write()
}

// write saves the tokens the way Codex does: same file, other fields untouched, 0600, atomic.
func (a *authFile) write() error {
	tok, err := json.Marshal(a.tokens)
	if err != nil {
		return err
	}
	a.fields["tokens"] = tok
	a.fields["last_refresh"], _ = json.Marshal(time.Now().UTC().Format("2006-01-02T15:04:05.000000Z"))
	out, err := json.MarshalIndent(a.fields, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

type usageResponse struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Primary   *liveWin `json:"primary_window"`
		Secondary *liveWin `json:"secondary_window"`
	} `json:"rate_limit"`
	Credits *struct {
		HasCredits bool   `json:"has_credits"`
		Unlimited  bool   `json:"unlimited"`
		Balance    string `json:"balance"`
	} `json:"credits"`
}

type liveWin struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int     `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

type parsed struct {
	plan    string
	windows []model.Window
	notes   []string
}

func parseLive(body []byte) (parsed, error) {
	var out parsed
	var ur usageResponse
	if err := json.Unmarshal(body, &ur); err != nil {
		return out, fmt.Errorf("decode usage: %w", err)
	}
	out.plan = ur.PlanType
	if ur.RateLimit != nil {
		add := func(key string, w *liveWin) {
			if w == nil {
				return
			}
			out.windows = append(out.windows, window(key, w.UsedPercent, w.LimitWindowSeconds/60, w.ResetAt))
		}
		add("primary", ur.RateLimit.Primary)
		add("secondary", ur.RateLimit.Secondary)
	}
	if ur.Credits != nil && (ur.Credits.HasCredits || ur.Credits.Unlimited) {
		if ur.Credits.Unlimited {
			out.notes = append(out.notes, "credits: unlimited")
		} else {
			out.notes = append(out.notes, "credits balance: "+ur.Credits.Balance)
		}
	}
	if len(out.windows) == 0 {
		return out, fmt.Errorf("usage response had no windows: %s", truncate(string(body), 200))
	}
	return out, nil
}

func window(key string, usedPercent float64, minutes int, resetsAt int64) model.Window {
	mw := model.Window{Key: key, Utilization: usedPercent, WindowMinutes: minutes}
	if resetsAt > 0 {
		mw.ResetsAt = time.Unix(resetsAt, 0)
	}
	switch {
	case minutes == 300:
		mw.Label = "5-hour"
	case minutes == 7*24*60:
		mw.Label = "Weekly"
	default:
		mw.Label = (time.Duration(minutes) * time.Minute).String()
	}
	return mw
}

type rateLimits struct {
	PlanType  string `json:"plan_type"`
	Primary   *rlWin `json:"primary"`
	Secondary *rlWin `json:"secondary"`
}

type rlWin struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// fromStore reads the newest rate_limits block the scanner captured from a rollout.
func (p *Provider) fromStore() (model.Limits, error) {
	l := model.Limits{Source: p.Source(), FetchedAt: time.Now()}
	if p.Store == nil {
		return l, errors.New("no store configured")
	}
	snap, err := p.Store.LatestLimitSnapshot(p.Source())
	if err != nil {
		return l, err
	}
	if snap.Raw == "" {
		return l, errors.New("no Codex rate-limit snapshot scanned yet")
	}
	var rl rateLimits
	if err := json.Unmarshal([]byte(snap.Raw), &rl); err != nil {
		return l, err
	}
	l.Plan = rl.PlanType
	l.FetchedAt = time.UnixMilli(snap.TS)
	l.Raw = json.RawMessage(snap.Raw)
	add := func(key string, w *rlWin) {
		if w == nil {
			return
		}
		l.Windows = append(l.Windows, window(key, w.UsedPercent, w.WindowMinutes, w.ResetsAt))
	}
	add("primary", rl.Primary)
	add("secondary", rl.Secondary)
	if len(l.Windows) == 0 {
		return l, errors.New("Codex snapshot had no windows")
	}
	return l, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return strings.TrimSpace(s)
}
