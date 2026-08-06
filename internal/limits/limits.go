// Package limits bounds what an anonymous public visitor can cost.
package limits

import (
	"errors"
	"sync"
	"time"
)

// ErrPerIPQuota is returned when an IP has used its hourly allowance.
var ErrPerIPQuota = errors.New("per-IP quota exceeded")

// ErrBusy is returned when the global concurrency cap is saturated; callers
// must reject rather than queue so a spike can't pile up upstream requests.
var ErrBusy = errors.New("too many concurrent requests")

// ipWindow is one IP's hourly quota window, reset on a fixed clock boundary
// rather than a sliding one.
type ipWindow struct {
	count int
	start time.Time
}

// Limiter enforces a per-IP hourly quota and a global concurrency cap.
type Limiter struct {
	mu            sync.Mutex
	perIP         map[string]*ipWindow
	perIPPerHour  int
	inFlight      int
	maxConcurrent int
}

// NewLimiter returns a limiter with the given quotas.
func NewLimiter(perIPPerHour, maxConcurrent int) *Limiter {
	return &Limiter{
		perIP:         make(map[string]*ipWindow),
		perIPPerHour:  perIPPerHour,
		maxConcurrent: maxConcurrent,
	}
}

// Acquire reserves capacity for one request from ip, checking and
// incrementing both counters under one lock so two callers can never both
// see a slot as free. release is idempotent and must be called when done.
func (l *Limiter) Acquire(ip string, now time.Time) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	w, ok := l.perIP[ip]
	if !ok || now.Sub(w.start) >= time.Hour {
		w = &ipWindow{start: now}
		l.perIP[ip] = w
	}
	if w.count >= l.perIPPerHour {
		return nil, ErrPerIPQuota
	}
	if l.inFlight >= l.maxConcurrent {
		return nil, ErrBusy
	}

	w.count++
	l.inFlight++

	released := false
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if released {
			return
		}
		released = true
		l.inFlight--
	}, nil
}

// Sweep drops per-IP windows that have rolled over and returns how many.
func (l *Limiter) Sweep(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	removed := 0
	for ip, w := range l.perIP {
		if now.Sub(w.start) >= time.Hour {
			delete(l.perIP, ip)
			removed++
		}
	}
	return removed
}

// StartSweeper runs Sweep on an interval until stop is closed.
func (l *Limiter) StartSweeper(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	go func() {
		defer t.Stop()
		l.runSweeper(t.C, stop)
	}()
}

// runSweeper is the sweeper loop, split out from StartSweeper so tests can
// drive it with a synthetic tick channel instead of a real ticker.
func (l *Limiter) runSweeper(ticks <-chan time.Time, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case now := <-ticks:
			l.Sweep(now)
		}
	}
}
