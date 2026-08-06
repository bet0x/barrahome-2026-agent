// Package limits bounds what an anonymous public visitor can cost.
package limits

import (
	"errors"
	"sync"
	"time"
)

// ErrPerIPQuota is returned when an IP has used its hourly allowance.
var ErrPerIPQuota = errors.New("per-IP quota exceeded")

// ErrBusy is returned when the global concurrency cap is saturated. Callers
// must reject rather than queue, so a traffic spike cannot pile up unbounded
// upstream requests.
var ErrBusy = errors.New("too many concurrent requests")

// ipWindow is one IP's rolling hourly quota window.
type ipWindow struct {
	count int
	start time.Time
}

// Limiter enforces a rolling per-IP hourly quota and a global cap on the
// number of in-flight upstream streams.
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

// Acquire reserves capacity for one request from ip. The quota check, the
// concurrency check and both counter increments happen under a single lock,
// so two concurrent callers can never both observe a slot as free and both
// take it. The returned release must be called when the request finishes;
// calling it more than once is safe.
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

// Sweep drops per-IP windows that have rolled over, bounding perIP's growth
// as distinct IPs come and go. It returns how many windows were removed.
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
