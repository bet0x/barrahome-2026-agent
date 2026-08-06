package session

import (
	"errors"
	"testing"
	"time"

	"github.com/bet0x/barrahome-2026-agent/internal/moonshot"
)

func TestGetOrCreateReturnsSameSession(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	now := time.Now()

	s1, err := st.GetOrCreate("abc", now)
	if err != nil {
		t.Fatal(err)
	}
	s1.Messages = append(s1.Messages, moonshot.Message{Role: "user", Content: "hola"})
	st.Save(s1, now)

	s2, err := st.GetOrCreate("abc", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Messages) != 1 || s2.Messages[0].Content != "hola" {
		t.Errorf("history not preserved: %+v", s2.Messages)
	}
	if st.Len() != 1 {
		t.Errorf("Len() = %d, want 1", st.Len())
	}
}

func TestTurnLimit(t *testing.T) {
	st := NewStore(30*time.Minute, 2)
	now := time.Now()

	for i := 0; i < 2; i++ {
		s, err := st.GetOrCreate("abc", now)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		s.Turns++
		st.Save(s, now)
	}
	if _, err := st.GetOrCreate("abc", now); !errors.Is(err, ErrTurnLimit) {
		t.Errorf("third turn error = %v, want ErrTurnLimit", err)
	}
}

func TestSweepEvictsIdleSessions(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	now := time.Now()

	if _, err := st.GetOrCreate("fresh", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetOrCreate("stale", now.Add(-31*time.Minute)); err != nil {
		t.Fatal(err)
	}

	if removed := st.Sweep(now); removed != 1 {
		t.Errorf("Sweep removed %d, want 1", removed)
	}
	if st.Len() != 1 {
		t.Errorf("Len() = %d after sweep, want 1", st.Len())
	}
}

// TestSweeperRunsOnTick drives runSweeper with a synthetic tick channel so
// the test is deterministic: closing stop only after the tick send
// completes guarantees the goroutine has already picked the tick branch, and
// waiting on done guarantees Sweep ran before we inspect the store.
func TestSweeperRunsOnTick(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	base := time.Now()

	if _, err := st.GetOrCreate("stale", base.Add(-31*time.Minute)); err != nil {
		t.Fatal(err)
	}

	ticks := make(chan time.Time)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		st.runSweeper(ticks, stop)
		close(done)
	}()

	ticks <- base
	close(stop)
	<-done

	if st.Len() != 0 {
		t.Errorf("Len() = %d after sweeper tick, want 0", st.Len())
	}
}
