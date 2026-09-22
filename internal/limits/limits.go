// Package limits defines how rate-limit state is obtained per source.
package limits

import (
	"context"
	"sync"
	"time"

	"github.com/kmccarp/token-usage/internal/model"
)

// Provider reports the current rate-limit windows for one source.
type Provider interface {
	Source() string
	// Fetch returns the latest known limits. Implementations decide whether
	// that means a network call or a local read.
	Fetch(ctx context.Context) (model.Limits, error)
	// Interval is how often the poller should call Fetch.
	Interval() time.Duration
}

// Poller runs providers on their intervals and caches the latest result.
type Poller struct {
	mu        sync.RWMutex
	latest    map[string]model.Limits
	providers []Provider
	onFetch   func(model.Limits)
}

// NewPoller builds a poller. onFetch (optional) is called with every successful fetch.
func NewPoller(providers []Provider, onFetch func(model.Limits)) *Poller {
	return &Poller{latest: map[string]model.Limits{}, providers: providers, onFetch: onFetch}
}

// Run polls until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, pr := range p.providers {
		wg.Add(1)
		go func(pr Provider) {
			defer wg.Done()
			for {
				p.fetchOne(ctx, pr)
				select {
				case <-ctx.Done():
					return
				case <-time.After(pr.Interval()):
				}
			}
		}(pr)
	}
	wg.Wait()
}

func (p *Poller) fetchOne(ctx context.Context, pr Provider) {
	l, err := pr.Fetch(ctx)
	l.Source = pr.Source()
	if l.FetchedAt.IsZero() {
		l.FetchedAt = time.Now()
	}
	p.mu.Lock()
	prev, had := p.latest[pr.Source()]
	if err != nil {
		// keep the last good windows, surface the error
		if had {
			prev.Error = err.Error()
			p.latest[pr.Source()] = prev
		} else {
			l.Error = err.Error()
			p.latest[pr.Source()] = l
		}
		p.mu.Unlock()
		return
	}
	p.latest[pr.Source()] = l
	p.mu.Unlock()
	if p.onFetch != nil {
		p.onFetch(l)
	}
}

// Refresh forces an immediate fetch of every provider.
func (p *Poller) Refresh(ctx context.Context) {
	for _, pr := range p.providers {
		p.fetchOne(ctx, pr)
	}
}

// Latest returns the cached limits for every provider, in provider order.
func (p *Poller) Latest() []model.Limits {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]model.Limits, 0, len(p.providers))
	for _, pr := range p.providers {
		if l, ok := p.latest[pr.Source()]; ok {
			out = append(out, l)
		} else {
			out = append(out, model.Limits{Source: pr.Source(), Error: "not fetched yet"})
		}
	}
	return out
}
