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

// Session is one visitor's conversation. Nothing is persisted to disk.
type Session struct {
	ID       string
	Messages []moonshot.Message
	Turns    int
	LastSeen time.Time
}

// entry is the store's bookkeeping around a Session: whether it currently
// has an outstanding checkout, which both excludes concurrent callers and
// protects it from Sweep.
type entry struct {
	sess       *Session
	checkedOut bool
}

// Store holds live sessions with a TTL and a per-session turn cap.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*entry
	ttl      time.Duration
	maxTurns int
}

// NewStore returns a store evicting idle sessions after ttl and capping each
// session at maxTurns checkouts.
func NewStore(ttl time.Duration, maxTurns int) *Store {
	return &Store{
		sessions: make(map[string]*entry),
		ttl:      ttl,
		maxTurns: maxTurns,
	}
}

// Checkout reserves the session for id — creating it on first use — for
// exclusive use by the caller and returns a release func to call when done.
// The turn is spent atomically with the reservation, so callers must not
// touch Session.Turns themselves.
//
// It returns ErrSessionBusy if the session already has an outstanding
// checkout, and ErrTurnLimit once the session has spent its turns. The lock
// is not held past this call: callers are expected to do their (possibly
// slow) work between Checkout and release.
func (s *Store) Checkout(id string, now time.Time) (*Session, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, ok := s.sessions[id]
	if !ok {
		e = &entry{sess: &Session{ID: id, LastSeen: now}}
		s.sessions[id] = e
	}
	if e.checkedOut {
		return nil, nil, ErrSessionBusy
	}
	if e.sess.Turns >= s.maxTurns {
		return nil, nil, ErrTurnLimit
	}

	e.checkedOut = true
	e.sess.Turns++
	e.sess.LastSeen = now
	return e.sess, s.releaseFunc(e), nil
}

// releaseFunc closes over e directly rather than re-reading s.sessions[id],
// so a stale release can never touch whatever session has since taken the
// same id — there is nothing here that could resurrect an evicted session.
func (s *Store) releaseFunc(e *entry) func() {
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		e.checkedOut = false
	}
}

// Sweep drops idle sessions that are not currently checked out and returns
// how many went.
func (s *Store) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, e := range s.sessions {
		if e.checkedOut {
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
