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

// Session is one visitor's conversation. Nothing is persisted to disk.
type Session struct {
	ID       string
	Messages []moonshot.Message
	Turns    int
	LastSeen time.Time
}

// Store holds live sessions with a TTL and a per-session turn cap.
type Store struct {
	mu       sync.Mutex
	sessions map[string]*Session
	ttl      time.Duration
	maxTurns int
}

// NewStore returns a store evicting sessions idle longer than ttl.
func NewStore(ttl time.Duration, maxTurns int) *Store {
	return &Store{
		sessions: make(map[string]*Session),
		ttl:      ttl,
		maxTurns: maxTurns,
	}
}

// GetOrCreate returns the session for id, creating it if absent. It returns
// ErrTurnLimit once the session has spent its turns.
func (s *Store) GetOrCreate(id string, now time.Time) (*Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		sess = &Session{ID: id, LastSeen: now}
		s.sessions[id] = sess
		return sess, nil
	}
	if sess.Turns >= s.maxTurns {
		return nil, ErrTurnLimit
	}
	sess.LastSeen = now
	return sess, nil
}

// Save records the session's updated state.
func (s *Store) Save(sess *Session, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess.LastSeen = now
	s.sessions[sess.ID] = sess
}

// Sweep drops sessions idle for longer than the TTL and returns how many went.
func (s *Store) Sweep(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for id, sess := range s.sessions {
		if now.Sub(sess.LastSeen) > s.ttl {
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
