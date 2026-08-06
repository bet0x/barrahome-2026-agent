// Package session keeps per-visitor conversation state in memory only.
package session

import (
	"errors"
	"sync"
	"time"

	"github.com/bet0x/barrahome-2026-agent/internal/moonshot"
)

// ErrTurnLimit is returned once a session has used its allowance of turns.
var ErrTurnLimit = errors.New("session turn limit reached")

// ErrSessionBusy is returned when a session already has an outstanding
// checkout. A session models one browser tab talking to the agent, so a
// second concurrent request for the same id is refused rather than left to
// race the first over the same Messages slice and Turns counter.
var ErrSessionBusy = errors.New("session already in use")

// ErrStoreFull is returned when the store is at its session cap and every
// existing session is checked out, so there is nothing idle to evict to
// make room for a new one. The global concurrency cap upstream is what
// keeps this case rare: it bounds how many sessions can be checked out at
// once, which bounds how much of the store this can ever apply to.
var ErrStoreFull = errors.New("session store full")

// DefaultCheckoutDeadline is a sensible default for the checkoutDeadline
// passed to NewStore: how long a checkout may stay outstanding before Sweep
// force-evicts it. Without this, a stuck caller — e.g. blocked forever on
// an upstream read — bricks its session id for that visitor permanently:
// the entry never ages out and every later request 409s.
const DefaultCheckoutDeadline = 5 * time.Minute

// DefaultMaxSessions is a sensible default for the maxSessions passed to
// NewStore. Session ids are client-supplied on a public, unauthenticated
// endpoint, so nothing else bounds how many distinct sessions accumulate;
// each can carry up to MaxTurns turns of history with tool results capped
// at 64KB apiece (~1.3MB per session worst case at the default MaxTurns of
// 20). 100 sessions puts the worst-case ceiling on session memory at
// roughly 128MB — the same order of magnitude as the sandbox's own 192M
// MaxMemory budget, so a flood of live sessions can no longer contend with
// it for the host's memory unboundedly.
const DefaultMaxSessions = 100

// Session is one visitor's conversation. Nothing is persisted to disk.
type Session struct {
	ID       string
	Messages []moonshot.Message
	Turns    int
	LastSeen time.Time
}

// entry is the store's bookkeeping around a Session: whether it currently
// has an outstanding checkout, which both excludes concurrent callers and
// protects it from ttl-based sweeping (though not forever — see
// DefaultCheckoutDeadline).
type entry struct {
	sess         *Session
	checkedOut   bool
	checkedOutAt time.Time
}

// Store holds live sessions with a TTL and a per-session turn cap.
type Store struct {
	mu               sync.Mutex
	sessions         map[string]*entry
	ttl              time.Duration
	maxTurns         int
	checkoutDeadline time.Duration
	maxSessions      int
	forcedEvictions  int
}

// NewStore returns a store evicting idle sessions after ttl and capping each
// session at maxTurns checkouts. checkoutDeadline bounds how long a checkout
// may stay outstanding before Sweep force-evicts it (see
// DefaultCheckoutDeadline). maxSessions caps the number of live sessions;
// once at capacity, Checkout evicts the least-recently-seen idle session to
// make room rather than refusing the visitor (see DefaultMaxSessions).
func NewStore(ttl time.Duration, maxTurns int, checkoutDeadline time.Duration, maxSessions int) *Store {
	return &Store{
		sessions:         make(map[string]*entry),
		ttl:              ttl,
		maxTurns:         maxTurns,
		checkoutDeadline: checkoutDeadline,
		maxSessions:      maxSessions,
	}
}

// noopRelease lets Checkout return a non-nil release func on every path, so
// callers can write "defer release()" unconditionally, even when err != nil.
func noopRelease() {}

// Checkout reserves the session for id — creating it on first use — for
// exclusive use by the caller and returns a release func to call when done.
// The turn is spent atomically with the reservation, so callers must not
// touch Session.Turns themselves. A spent turn is never refunded, even if
// the caller's own request later fails: a refund path is exactly what an
// abuser would drive through.
//
// The returned *Session must not be read or written after release is
// called — once released, a later caller may be handed the same pointer.
//
// It returns ErrSessionBusy if the session already has an outstanding
// checkout, ErrTurnLimit once the session has spent its turns, and
// ErrStoreFull if a brand-new id arrives while the store is at maxSessions
// and every existing session is checked out (nothing idle to evict); all
// three error paths still return a usable no-op release func. The store's
// lock is not held past this call: callers are expected to do their
// (possibly slow) work between Checkout and release.
func (s *Store) Checkout(id string, now time.Time) (*Session, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.sessions[id]
	if !ok {
		if len(s.sessions) >= s.maxSessions {
			victim, found := s.lruIdleLocked()
			if !found {
				return nil, noopRelease, ErrStoreFull
			}
			delete(s.sessions, victim)
		}
		e = &entry{sess: &Session{ID: id, LastSeen: now}}
		s.sessions[id] = e
	}
	if e.checkedOut {
		return nil, noopRelease, ErrSessionBusy
	}
	if e.sess.Turns >= s.maxTurns {
		return nil, noopRelease, ErrTurnLimit
	}

	e.checkedOut = true
	e.checkedOutAt = now
	e.sess.Turns++
	e.sess.LastSeen = now
	return e.sess, s.releaseFunc(e), nil
}

// releaseFunc closes over e directly rather than re-reading s.sessions[id],
// so a stale release can never touch whatever session has since taken the
// same id — there is nothing here that could resurrect an evicted session.
// The released flag makes the returned func idempotent: without it, a
// second call from a caller who released once already would clear whatever
// later holder's checkedOut flag happens to be on e, breaking that holder's
// exclusivity.
func (s *Store) releaseFunc(e *entry) func() {
	released := false
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if released {
			return
		}
		released = true
		e.checkedOut = false
	}
}

// lruIdleLocked returns the id of the least-recently-seen session that is
// not currently checked out, so Checkout can evict it to make room at
// capacity. The caller must hold s.mu. found is false if every session is
// checked out.
func (s *Store) lruIdleLocked() (id string, found bool) {
	var oldest time.Time
	for candidate, e := range s.sessions {
		if e.checkedOut {
			continue
		}
		if !found || e.sess.LastSeen.Before(oldest) {
			id, oldest, found = candidate, e.sess.LastSeen, true
		}
	}
	return id, found
}

// Sweep drops sessions that are idle past the TTL. A session that is
// currently checked out is left alone, unless the checkout itself has been
// outstanding for longer than the store's checkoutDeadline, in which case it
// is force-evicted — see DefaultCheckoutDeadline for why that backstop
// exists. Sweep returns how many sessions, of either kind, were removed.
func (s *Store) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, e := range s.sessions {
		if e.checkedOut {
			if now.Sub(e.checkedOutAt) > s.checkoutDeadline {
				delete(s.sessions, id)
				removed++
				s.forcedEvictions++
			}
			continue
		}
		if now.Sub(e.sess.LastSeen) > s.ttl {
			delete(s.sessions, id)
			removed++
		}
	}
	return removed
}

// Len reports the number of live sessions.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// ForcedEvictions reports how many checkouts Sweep has ever force-evicted
// for staying outstanding past the store's checkoutDeadline. It should stay
// at zero in normal operation; a nonzero and growing count is the signal
// that some caller is leaking checkouts (never releasing) and is worth
// alerting on.
func (s *Store) ForcedEvictions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forcedEvictions
}

// StartSweeper runs Sweep on an interval until stop is closed.
func (s *Store) StartSweeper(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	go func() {
		defer t.Stop()
		s.runSweeper(t.C, stop)
	}()
}

// runSweeper is the sweeper loop, split out from StartSweeper so tests can
// drive it with a synthetic tick channel instead of a real ticker.
func (s *Store) runSweeper(ticks <-chan time.Time, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case now := <-ticks:
			s.Sweep(now)
		}
	}
}
