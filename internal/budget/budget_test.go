package budget_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/budget"
)

func mustNew(t *testing.T, n int64) *budget.Budget {
	t.Helper()
	b, err := budget.New(n)
	if err != nil {
		t.Fatalf("New(%d): %v", n, err)
	}
	return b
}

// The point of the whole package: bytes in flight stay under the limit even
// when many workers want memory at once.
func TestConcurrentHoldersStayUnderTheBudget(t *testing.T) {
	t.Parallel()

	const cap = 1000
	b := mustNew(t, cap)

	var held, peak atomic.Int64
	sizes := []int64{1, 7, 100, 250, 400, 999}

	var wg sync.WaitGroup
	for i := range 120 {
		size := sizes[i%len(sizes)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := b.Acquire(context.Background(), size)
			if err != nil {
				t.Errorf("Acquire(%d): %v", size, err)
				return
			}
			n := held.Add(size)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(100 * time.Microsecond)
			held.Add(-size)
			release()
		}()
	}
	wg.Wait()

	if peak.Load() > cap {
		t.Errorf("peak bytes in flight = %d, budget is %d", peak.Load(), cap)
	}
	if peak.Load() == 0 {
		t.Error("nothing was ever held; the test is not measuring anything")
	}
}

// A message larger than the whole budget cannot be satisfied by any combination
// of refunds. Charging what was asked would block for ever, so it is charged
// the whole budget and runs alone.
func TestAMessageBiggerThanTheBudgetStillRuns(t *testing.T) {
	t.Parallel()

	b := mustNew(t, 100)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	release, err := b.Acquire(ctx, 5_000_000)
	if err != nil {
		t.Fatalf("a message larger than the budget must still be copyable: %v", err)
	}

	// It holds everything, so nothing else can run alongside it.
	blocked, cancelBlocked := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelBlocked()
	if _, err := b.Acquire(blocked, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an oversized holder should exclude everything else, got %v", err)
	}

	release()

	after, err := b.Acquire(ctx, 100)
	if err != nil {
		t.Fatalf("releasing an oversized charge must return the whole budget: %v", err)
	}
	after()
}

// A server that fails to report RFC822.SIZE should degrade to the
// connection-count limit, not to no limit at all.
func TestAnUnknownSizeIsStillCharged(t *testing.T) {
	t.Parallel()

	b := mustNew(t, 2)

	first, err := b.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatalf("Acquire(0): %v", err)
	}
	second, err := b.Acquire(context.Background(), 0)
	if err != nil {
		t.Fatalf("Acquire(0): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := b.Acquire(ctx, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a third zero-size charge should have waited, got %v", err)
	}

	first()
	second()
}

func TestAcquireBlocksUntilBytesComeBack(t *testing.T) {
	t.Parallel()

	b := mustNew(t, 100)

	release, err := b.Acquire(context.Background(), 100)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	got := make(chan error, 1)
	go func() {
		r, err := b.Acquire(context.Background(), 60)
		if err == nil {
			r()
		}
		got <- err
	}()

	select {
	case <-got:
		t.Fatal("Acquire succeeded while the budget was fully held")
	case <-time.After(50 * time.Millisecond):
	}

	release()

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("Acquire after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("releasing the budget did not wake the waiter")
	}
}

func TestAcquireRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	b := mustNew(t, 10)
	release, err := b.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := b.Acquire(ctx, 5); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Acquire took %s to notice a 20ms deadline", elapsed)
	}
}

// A cancelled Acquire must not have charged anything, or the budget bleeds away
// to nothing over a long run with a cancelled context.
func TestACancelledAcquireChargesNothing(t *testing.T) {
	t.Parallel()

	b := mustNew(t, 10)
	held, err := b.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		if _, err := b.Acquire(ctx, 4); err == nil {
			t.Fatal("want the acquire to time out")
		}
		cancel()
	}
	held()

	full, err := b.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatalf("the whole budget should be available again: %v", err)
	}
	full()
}

// Deferring the release and also calling it early is the natural way to write a
// fetch that wants to free memory before its append finishes. Refunding twice
// would let the budget grow without bound.
func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	b := mustNew(t, 10)

	release, err := b.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	release()

	first, err := b.Acquire(context.Background(), 10)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer first()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := b.Acquire(ctx, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a double release created budget out of nothing: %v", err)
	}
}

// A nil budget means "no limit", so a caller that does not want one does not
// have to branch on it at every use.
func TestANilBudgetChargesNothing(t *testing.T) {
	t.Parallel()

	var b *budget.Budget
	for range 3 {
		release, err := b.Acquire(context.Background(), 1<<40)
		if err != nil {
			t.Fatalf("a nil budget must never block: %v", err)
		}
		release()
	}
	if b.Cap() != 0 {
		t.Errorf("nil budget Cap() = %d, want 0", b.Cap())
	}
}

func TestNewRejectsUnusableSizes(t *testing.T) {
	t.Parallel()

	for _, n := range []int64{0, -1} {
		if _, err := budget.New(n); err == nil {
			t.Errorf("New(%d) should fail; every acquire would block for ever", n)
		}
	}
}
