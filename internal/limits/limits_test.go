package limits

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPerIPQuota(t *testing.T) {
	l := NewLimiter(2, 10)
	now := time.Now()

	for i := 0; i < 2; i++ {
		release, err := l.Acquire("1.2.3.4", now)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		release()
	}
	if _, err := l.Acquire("1.2.3.4", now); !errors.Is(err, ErrPerIPQuota) {
		t.Errorf("third request error = %v, want ErrPerIPQuota", err)
	}
	// A different IP is unaffected.
	if _, err := l.Acquire("5.6.7.8", now); err != nil {
		t.Errorf("other IP should be allowed, got %v", err)
	}
	// The window rolls forward.
	if _, err := l.Acquire("1.2.3.4", now.Add(61*time.Minute)); err != nil {
		t.Errorf("after the window the IP should be allowed again, got %v", err)
	}
}

func TestGlobalConcurrency(t *testing.T) {
	l := NewLimiter(100, 2)
	now := time.Now()

	r1, err := l.Acquire("a", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire("b", now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire("c", now); !errors.Is(err, ErrBusy) {
		t.Errorf("third concurrent request error = %v, want ErrBusy", err)
	}

	r1()
	if _, err := l.Acquire("d", now); err != nil {
		t.Errorf("after a release a slot should free up, got %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	l := NewLimiter(100, 1)
	now := time.Now()

	release, err := l.Acquire("a", now)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // must not free a second slot

	if _, err := l.Acquire("b", now); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Acquire("c", now); !errors.Is(err, ErrBusy) {
		t.Errorf("double release leaked a slot: %v", err)
	}
}

// TestConcurrentPerIPQuotaExact hammers a single IP from many goroutines at
// once. A check-then-act race (read w.count, then increment outside the
// lock) would let more than quota callers succeed; this pins the count to
// exactly quota under -race.
func TestConcurrentPerIPQuotaExact(t *testing.T) {
	const quota = 5
	const workers = 50

	l := NewLimiter(quota, workers) // concurrency cap wide open: only the quota is under test
	now := time.Now()

	var successes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := l.Acquire("shared", now)
			switch {
			case err == nil:
				successes.Add(1)
				release()
			case errors.Is(err, ErrPerIPQuota):
				// expected once the quota is spent
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != quota {
		t.Errorf("successful acquires = %d, want exactly %d", got, quota)
	}
}

// TestConcurrentGlobalCapNeverExceeded runs many goroutines on distinct IPs
// (so the per-IP quota never fires) and tracks the observed peak number of
// simultaneously held slots. A check-then-act race in the concurrency check
// would let the peak exceed maxConcurrent.
func TestConcurrentGlobalCapNeverExceeded(t *testing.T) {
	const maxConcurrent = 4
	const workers = 40

	l := NewLimiter(1000, maxConcurrent)
	now := time.Now()

	var held, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			release, err := l.Acquire(fmt.Sprintf("10.0.0.%d", n), now)
			if err != nil {
				return // ErrBusy is an expected outcome under contention
			}
			defer release()

			cur := held.Add(1)
			for {
				p := peak.Load()
				if cur <= p || peak.CompareAndSwap(p, cur) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			held.Add(-1)
		}(i)
	}
	wg.Wait()

	if p := peak.Load(); p > int32(maxConcurrent) {
		t.Errorf("peak concurrent holders = %d, want <= %d", p, maxConcurrent)
	}
}

// TestSweepDropsRolledOverWindow guards the memory-bounding contract: once
// an IP's window has rolled over, Sweep must reclaim it rather than leaving
// perIP to grow forever as distinct IPs come and go.
func TestSweepDropsRolledOverWindow(t *testing.T) {
	l := NewLimiter(1, 10)
	now := time.Now()

	release, err := l.Acquire("1.2.3.4", now)
	if err != nil {
		t.Fatal(err)
	}
	release()

	if removed := l.Sweep(now); removed != 0 {
		t.Errorf("Sweep removed %d for a window still in its hour, want 0", removed)
	}

	later := now.Add(61 * time.Minute)
	if removed := l.Sweep(later); removed != 1 {
		t.Errorf("Sweep removed %d for a rolled-over window, want 1", removed)
	}
}

// TestSweeperRunsOnTick drives runSweeper with a synthetic tick channel so
// the test is deterministic: closing stop only after the tick send
// completes guarantees the goroutine has already picked the tick branch, and
// waiting on done guarantees Sweep ran before we inspect the map.
func TestSweeperRunsOnTick(t *testing.T) {
	l := NewLimiter(1, 10)
	base := time.Now()

	release, err := l.Acquire("1.2.3.4", base.Add(-61*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	release()

	ticks := make(chan time.Time)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		l.runSweeper(ticks, stop)
		close(done)
	}()

	ticks <- base
	close(stop)
	<-done

	if len(l.perIP) != 0 {
		t.Errorf("perIP has %d entries after sweeper tick, want 0", len(l.perIP))
	}
}
