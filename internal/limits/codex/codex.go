// Package codex reports Codex rate limits. Codex writes the account's live
// rate-limit state into every token_count event of every rollout, so the
// scanner captures it and this provider simply reads the newest snapshot.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/kmccarp/token-usage/internal/model"
	"github.com/kmccarp/token-usage/internal/store"
)

// Provider reads limits from the store.
type Provider struct {
	Store *store.Store
	Every time.Duration
}

// Source implements limits.Provider.
func (p *Provider) Source() string { return "codex" }

// Interval implements limits.Provider.
func (p *Provider) Interval() time.Duration {
	if p.Every == 0 {
		return 30 * time.Second
	}
	return p.Every
}

type rateLimits struct {
	PlanType  string `json:"plan_type"`
	Primary   *rlWin `json:"primary"`
	Secondary *rlWin `json:"secondary"`
	Credits   *struct {
		HasCredits bool   `json:"has_credits"`
		Balance    string `json:"balance"`
	} `json:"credits"`
}

type rlWin struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

// Fetch implements limits.Provider.
func (p *Provider) Fetch(ctx context.Context) (model.Limits, error) {
	l := model.Limits{Source: p.Source(), FetchedAt: time.Now()}
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
		mw := model.Window{Key: key, Utilization: w.UsedPercent, WindowMinutes: w.WindowMinutes}
		if w.ResetsAt > 0 {
			mw.ResetsAt = time.Unix(w.ResetsAt, 0)
		}
		switch {
		case w.WindowMinutes == 300:
			mw.Label = "5-hour"
		case w.WindowMinutes == 7*24*60:
			mw.Label = "Weekly"
		default:
			mw.Label = (time.Duration(w.WindowMinutes) * time.Minute).String()
		}
		l.Windows = append(l.Windows, mw)
	}
	add("primary", rl.Primary)
	add("secondary", rl.Secondary)
	if len(l.Windows) == 0 {
		return l, errors.New("Codex snapshot had no windows")
	}
	return l, nil
}
