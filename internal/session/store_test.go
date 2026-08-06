package session

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bet0x/barrahome-2026-agent/internal/moonshot"
)

func TestCheckoutPreservesHistory(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	now := time.Now()

	s1, release1, err := st.Checkout("abc", now)
	if err != nil {
		t.Fatal(err)
	}
	s1.Messages = append(s1.Messages, moonshot.Message{Role: "user", Content: "hola"})
	release1()

	s2, release2, err := st.Checkout("abc", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if len(s2.Messages) != 1 || s2.Messages[0].Content != "hola" {
		t.Errorf("history not preserved: %+v", s2.Messages)
	}
	if st.Len() != 1 {
		t.Errorf("Len() = %d, want 1", st.Len())
	}
}

func TestCheckoutTurnLimit(t *testing.T) {
	st := NewStore(30*time.Minute, 2)
	now := time.Now()

	for i := 0; i < 2; i++ {
		_, release, err := st.Checkout("abc", now)
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		release()
	}
	if _, _, err := st.Checkout("abc", now); !errors.Is(err, ErrTurnLimit) {
		t.Errorf("third turn error = %v, want ErrTurnLimit", err)
	}
}

func TestCheckoutRefusesConcurrentSameSession(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	now := time.Now()

	_, release, err := st.Checkout("abc", now)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := st.Checkout("abc", now); !errors.Is(err, ErrSessionBusy) {
		t.Errorf("second concurrent checkout error = %v, want ErrSessionBusy", err)
	}

	release()

	// Once released, the id is free again.
	_, release2, err := st.Checkout("abc", now)
	if err != nil {
		t.Fatalf("checkout after release: %v", err)
	}
	release2()
}

// TestErrorPathsReturnUsableRelease guards the "defer release()" idiom: a
// caller that writes it immediately after Checkout, before checking err,
// must not panic on the ErrSessionBusy or ErrTurnLimit paths.
func TestErrorPathsReturnUsableRelease(t *testing.T) {
	st := NewStore(30*time.Minute, 1)
	now := time.Now()

	_, release, err := st.Checkout("abc", now)
	if err != nil {
		t.Fatal(err)
	}

	_, busyRelease, err := st.Checkout("abc", now)
	if !errors.Is(err, ErrSessionBusy) {
		t.Fatalf("err = %v, want ErrSessionBusy", err)
	}
	if busyRelease == nil {
		t.Fatal("release func on the ErrSessionBusy path is nil")
	}
	busyRelease() // must not panic

	release() // maxTurns is 1: this was the only turn, and it's spent now.

	_, limitRelease, err := st.Checkout("abc", now)
	if !errors.Is(err, ErrTurnLimit) {
		t.Fatalf("err = %v, want ErrTurnLimit", err)
	}
	if limitRelease == nil {
		t.Fatal("release func on the ErrTurnLimit path is nil")
	}
	limitRelease() // must not panic
}

// TestReleaseIsIdempotent reproduces the round-2 bug: a caller that calls
// its release func twice must not clear a later holder's checkedOut flag
// out from under it. Without idempotency, the second call here would free
// "abc" for a third party while B still believes it holds it exclusively.
func TestReleaseIsIdempotent(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	now := time.Now()

	_, releaseA, err := st.Checkout("abc", now)
	if err != nil {
		t.Fatal(err)
	}
	releaseA()

	_, releaseB, err := st.Checkout("abc", now)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseB()

	releaseA() // stale double release, must be a no-op now

	if _, _, err := st.Checkout("abc", now); !errors.Is(err, ErrSessionBusy) {
		t.Errorf("checkout while B still holds it = %v, want ErrSessionBusy (A's double release broke exclusivity)", err)
	}
}

// TestConcurrentCheckoutRespectsTurnCap reproduces the reviewer's race
// (many goroutines hammering one session id under a low turn cap) against
// the new API: each goroutine retries on ErrSessionBusy and stops on
// ErrTurnLimit, and the number that ever complete a checkout must equal
// maxTurns exactly, not more.
func TestConcurrentCheckoutRespectsTurnCap(t *testing.T) {
	const maxTurns = 3
	const workers = 20

	st := NewStore(30*time.Minute, maxTurns)
	now := time.Now()

	// Bounded, not "for {}": if a regression ever makes release stop
	// freeing the session, this fails fast with a message instead of
	// hanging until the test binary's own timeout.
	const maxAttempts = 100_000

	var successes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < maxAttempts; attempt++ {
				sess, release, err := st.Checkout("shared", now)
				switch {
				case errors.Is(err, ErrSessionBusy):
					continue // another worker holds it right now; retry
				case errors.Is(err, ErrTurnLimit):
					return // cap spent; give up
				case err != nil:
					t.Errorf("unexpected error: %v", err)
					return
				}
				sess.Messages = append(sess.Messages, moonshot.Message{Role: "user", Content: "hi"})
				release()
				successes.Add(1)
				return
			}
			t.Errorf("gave up after %d attempts waiting for %q to free up", maxAttempts, "shared")
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != maxTurns {
		t.Errorf("successful checkouts = %d, want exactly %d", got, maxTurns)
	}
}

// TestSweepSkipsRecentCheckedOutSession uses a ttl shorter than
// CheckoutDeadline so the two exemptions don't overlap: LastSeen alone
// would make this session look idle past the ttl, but a live, recent
// checkout must still protect it from Sweep. Once released, the same
// idle-past-ttl check evicts it normally.
func TestSweepSkipsRecentCheckedOutSession(t *testing.T) {
	st := NewStore(1*time.Minute, 20)
	now := time.Now()

	_, release, err := st.Checkout("busy", now)
	if err != nil {
		t.Fatal(err)
	}

	later := now.Add(2 * time.Minute) // past the 1-minute ttl, well under CheckoutDeadline
	if removed := st.Sweep(later); removed != 0 {
		t.Errorf("Sweep removed %d for a live, non-stuck checkout, want 0", removed)
	}
	if st.Len() != 1 {
		t.Errorf("Len() = %d, want 1 while checked out", st.Len())
	}

	release()

	if removed := st.Sweep(later); removed != 1 {
		t.Errorf("Sweep removed %d after release, want 1", removed)
	}
	if st.Len() != 0 {
		t.Errorf("Len() = %d after sweep, want 0", st.Len())
	}
}

// TestSweepForceEvictsStuckCheckout covers the leak this round closes: a
// checkout that is never released (a caller stuck on a slow upstream read,
// say) must still age out once it has outstayed CheckoutDeadline, rather
// than bricking that session id forever. The ForcedEvictions counter is the
// operator-visible signal that this path fired.
func TestSweepForceEvictsStuckCheckout(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	t0 := time.Now()

	_, release1, err := st.Checkout("stuck", t0)
	if err != nil {
		t.Fatal(err)
	}

	past := t0.Add(CheckoutDeadline + time.Minute)
	if removed := st.Sweep(past); removed != 1 {
		t.Errorf("Sweep removed %d, want 1 (stuck checkout past deadline)", removed)
	}
	if st.Len() != 0 {
		t.Errorf("Len() = %d after force-eviction, want 0", st.Len())
	}
	if got := st.ForcedEvictions(); got != 1 {
		t.Errorf("ForcedEvictions() = %d, want 1", got)
	}

	// A new request for the same id must get a fresh session rather than
	// ErrSessionBusy forever.
	sess2, release2, err := st.Checkout("stuck", past)
	if err != nil {
		t.Fatal(err)
	}
	sess2.Messages = append(sess2.Messages, moonshot.Message{Role: "user", Content: "fresh"})
	release2()

	// The stuck caller's eventual, very late release must not disturb the
	// session that has since taken its place — same guarantee as
	// TestReleaseCannotResurrectEvictedSession, exercised via force-eviction
	// instead of ttl-eviction.
	release1()

	sess3, release3, err := st.Checkout("stuck", past)
	if err != nil {
		t.Fatal(err)
	}
	defer release3()
	if len(sess3.Messages) != 1 || sess3.Messages[0].Content != "fresh" {
		t.Errorf("force-eviction was undone by the stuck release: %+v", sess3.Messages)
	}
}

func TestSweepEvictsIdleSessions(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	now := time.Now()

	_, releaseFresh, err := st.Checkout("fresh", now)
	if err != nil {
		t.Fatal(err)
	}
	releaseFresh()
	_, releaseStale, err := st.Checkout("stale", now.Add(-31*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	releaseStale()

	if removed := st.Sweep(now); removed != 1 {
		t.Errorf("Sweep removed %d, want 1", removed)
	}
	if st.Len() != 1 {
		t.Errorf("Len() = %d after sweep, want 1", st.Len())
	}
}

// TestReleaseCannotResurrectEvictedSession reproduces the reviewer's
// deterministic (no goroutines needed) bug: a handler's stale release,
// called after its session was evicted and a fresh one took the same id,
// must not clobber the fresh session's history.
func TestReleaseCannotResurrectEvictedSession(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	t0 := time.Now()

	sess1, release1, err := st.Checkout("abc", t0)
	if err != nil {
		t.Fatal(err)
	}
	sess1.Messages = append(sess1.Messages, moonshot.Message{Role: "user", Content: "old"})
	release1()

	later := t0.Add(31 * time.Minute)
	if removed := st.Sweep(later); removed != 1 {
		t.Fatalf("Sweep removed %d, want 1", removed)
	}

	sess2, release2, err := st.Checkout("abc", later)
	if err != nil {
		t.Fatal(err)
	}
	sess2.Messages = append(sess2.Messages, moonshot.Message{Role: "user", Content: "new"})
	release2()

	// The stale release for the evicted session must be a no-op against
	// the fresh session that has since taken its place under the same id.
	release1()

	sess3, release3, err := st.Checkout("abc", later)
	if err != nil {
		t.Fatal(err)
	}
	defer release3()
	if len(sess3.Messages) != 1 || sess3.Messages[0].Content != "new" {
		t.Errorf("resurrected stale session: %+v", sess3.Messages)
	}
}

// TestSweeperRunsOnTick drives runSweeper with a synthetic tick channel so
// the test is deterministic: closing stop only after the tick send
// completes guarantees the goroutine has already picked the tick branch, and
// waiting on done guarantees Sweep ran before we inspect the store.
func TestSweeperRunsOnTick(t *testing.T) {
	st := NewStore(30*time.Minute, 20)
	base := time.Now()

	_, release, err := st.Checkout("stale", base.Add(-31*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	release()

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
