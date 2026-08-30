package pool_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/pool"
)

// wall is a server that will hold only so many connections at once, and refuses
// the next one by hanging up rather than by saying so.
//
// This is mox, as measured on 2026-08-28: it accepts thirty and answers the
// thirty-first with an unexpected EOF during authentication. The refusal is
// deliberately the *ambiguous* one, because the ambiguity is the whole problem —
// a server that announced its limit in a response code would need none of this.
type wall struct {
	mu      sync.Mutex
	limit   int
	live    int
	dials   int
	refused int
	conns   []*fakeConn
}

func (w *wall) dial(context.Context) (imapx.Conn, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.dials++
	if w.live >= w.limit {
		w.refused++
		return nil, io.ErrUnexpectedEOF
	}
	w.live++
	c := &fakeConn{onClose: w.release}
	w.conns = append(w.conns, c)
	return c, nil
}

func (w *wall) release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.live--
}

func (w *wall) stats() (dials, refused, live int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dials, w.refused, w.live
}

// work leases a connection, pretends to use it, and releases it.
func work(ctx context.Context, p *pool.Pool) error {
	return workFor(ctx, p, time.Millisecond)
}

// workFor is work that holds the connection long enough to overlap with the
// other workers, which is what makes spare tokens turn into real dials.
func workFor(ctx context.Context, p *pool.Pool, hold time.Duration) error {
	l, err := p.Acquire(ctx, "")
	if err != nil {
		return err
	}
	time.Sleep(hold)
	l.Release(nil)
	return nil
}

// TestThePoolSettlesUnderAServersLimit is the test this whole design exists to
// pass.
//
// A pool capped at 16 against a server that holds 5 must end up at 5 and stay
// there. The assertion that matters is not the width but the dial count: a pool
// that had merely recorded a smaller number while continuing to reach for
// connections would keep the server refusing for the length of the run, which is
// the behaviour being fixed.
func TestThePoolSettlesUnderAServersLimit(t *testing.T) {
	t.Parallel()

	w := &wall{limit: 5}
	p, err := pool.New(pool.Options{Cap: 16, Dial: w.dial})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	ctx := context.Background()

	// Prime the pool: something must have succeeded recently for a refusal to
	// be read as the limit rather than as the network.
	if err := work(ctx, p); err != nil {
		t.Fatalf("first lease: %v", err)
	}

	// Now push against the wall from every direction at once, as a run does.
	for range 4 {
		var wg sync.WaitGroup
		for range 16 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = work(ctx, p)
			}()
		}
		wg.Wait()
	}

	if got := p.Width(); got > 5 || got < 1 {
		t.Errorf("Width() = %d, want the pool settled at or under the server's limit of 5", got)
	}

	_, refusedBefore, _ := w.stats()

	// Settled means settled: more work must not go back to the wall.
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = work(ctx, p)
		}()
	}
	wg.Wait()

	_, refusedAfter, _ := w.stats()
	if refusedAfter != refusedBefore {
		t.Errorf("the server refused %d more connections after the pool had settled; it should have stopped asking",
			refusedAfter-refusedBefore)
	}
}

// TestShrinkingClosesConnectionsRatherThanForgettingThem.
//
// The server counts sockets, not busy ones. A shrink that reduced only the
// concurrency limit would leave exactly as many connections open, change nothing
// the server can see, and still report success — so this asserts on the fake's
// close count rather than on Width.
func TestShrinkingClosesConnectionsRatherThanForgettingThem(t *testing.T) {
	t.Parallel()

	w := &wall{limit: 3}
	p, err := pool.New(pool.Options{Cap: 8, Dial: w.dial})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	ctx := context.Background()
	if err := work(ctx, p); err != nil {
		t.Fatalf("first lease: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = work(ctx, p)
		}()
	}
	wg.Wait()

	_, _, live := w.stats()
	if width := p.Width(); live > width {
		t.Errorf("the server still holds %d connections while the pool allows %d; the shrink did not reach the wire",
			live, width)
	}
}

// TestClosingAShrunkPoolDoesNotHang.
//
// Close waits for every token still in circulation, and a shrink permanently
// destroys some of them. Getting the arithmetic wrong deadlocks shutdown, which
// is a failure mode that reports nothing at all — hence the timeout and the
// explicit message.
func TestClosingAShrunkPoolDoesNotHang(t *testing.T) {
	t.Parallel()

	w := &wall{limit: 2}
	p, err := pool.New(pool.Options{Cap: 8, Dial: w.dial})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	if err := work(ctx, p); err != nil {
		t.Fatalf("first lease: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = work(ctx, p)
		}()
	}
	wg.Wait()

	if p.Width() >= 8 {
		t.Fatalf("Width() = %d; the pool did not shrink, so this test proves nothing", p.Width())
	}

	done := make(chan error, 1)
	go func() { done <- p.Close(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a shrunk pool: it is waiting for tokens the shrink destroyed")
	}
}

// TestARefusalIsReported gives the run something to log. Without it the width a
// run settles at is invisible, and the question of whether growing back is worth
// building goes back to being an argument rather than a measurement.
func TestARefusalIsReported(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		widths [][2]int
		causes []error
	)

	w := &wall{limit: 2}
	p, err := pool.New(pool.Options{
		Cap:  8,
		Dial: w.dial,
		OnShrink: func(from, to int, cause error) {
			mu.Lock()
			defer mu.Unlock()
			widths = append(widths, [2]int{from, to})
			causes = append(causes, cause)
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	ctx := context.Background()
	if err := work(ctx, p); err != nil {
		t.Fatalf("first lease: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = work(ctx, p)
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(widths) == 0 {
		t.Fatal("the pool shrank without telling anyone")
	}
	for i, w := range widths {
		if w[1] >= w[0] {
			t.Errorf("shrink %d reported %d -> %d, which is not a shrink", i, w[0], w[1])
		}
	}
	for i, c := range causes {
		if !errors.Is(c, io.ErrUnexpectedEOF) {
			t.Errorf("shrink %d reported cause %v, want the server's own refusal", i, c)
		}
	}
}

// TestACapacityRefusalIsWaitedOutRatherThanReturned.
//
// This test used to assert the opposite, under the name
// TestACapacityRefusalIsDistinguishableByCallers, and its premise was the bug.
// Measuring mox settled it: asking a server that holds 30 connections for 36
// produced one shrink and four failed folders, because the refusals racing
// alongside the shrink found the width already corrected, had nothing left to
// do, and became each folder's own failure. A pool that has just been refused
// is a pool demonstrably holding connections the server accepted, so the answer
// is to wait for one of them.
func TestACapacityRefusalIsWaitedOutRatherThanReturned(t *testing.T) {
	t.Parallel()

	w := &wall{limit: 1}
	p, err := pool.New(pool.Options{Cap: 4, Dial: w.dial})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	ctx := context.Background()

	// Something must have succeeded recently, or a refusal is indistinguishable
	// from the network being gone and is deliberately not judged.
	if err := work(ctx, p); err != nil {
		t.Fatalf("first lease: %v", err)
	}

	// One lease held open, so the server is at its limit and demonstrably alive.
	held, err := p.Acquire(ctx, "")
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}

	// The second acquire must block on the held connection rather than fail. It
	// is released from another goroutine so that a pool which waits completes
	// and a pool which returns the refusal fails on the error instead.
	go func() {
		time.Sleep(20 * time.Millisecond)
		held.Release(nil)
	}()

	second, err := p.Acquire(ctx, "")
	if err != nil {
		t.Fatalf("Acquire() after a capacity refusal = %v, want it to wait for the connection that was busy", err)
	}
	second.Release(nil)

	if _, _, live := w.stats(); live > 1 {
		t.Errorf("server sees %d live connections, want no more than its limit of 1", live)
	}
}

// TestACapacityRefusalWithNothingToWaitForReachesTheCaller.
//
// Waiting is only right while the pool holds a connection to wait for. Once
// every connection it opened has broken, there is nothing a wait could ever be
// satisfied by, and the refusal has to go back with its judgement intact — this
// is the error retry.classify reads to decide to slow down rather than
// reconnect straight into a server asking for less load.
func TestACapacityRefusalWithNothingToWaitForReachesTheCaller(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	dials := 0
	dial := func(context.Context) (imapx.Conn, error) {
		mu.Lock()
		defer mu.Unlock()
		dials++
		if dials == 1 {
			return &fakeConn{}, nil
		}
		return nil, io.ErrUnexpectedEOF
	}

	p, err := pool.New(pool.Options{Cap: 4, Dial: dial})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	ctx := context.Background()

	// The one connection is opened, proving the server alive, and then broken,
	// so the pool holds nothing while its judgement of a refusal is still fresh.
	l, err := p.Acquire(ctx, "")
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	l.Release(imapx.ErrConnectionBroken)

	_, err = p.Acquire(ctx, "")
	if err == nil {
		t.Fatal("Acquire() succeeded against a server refusing every connection")
	}
	if !errors.Is(err, imapx.ErrAtCapacity) {
		t.Errorf("Acquire() error = %v, want it to carry imapx.ErrAtCapacity", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("Acquire() error = %v, want the server's own error preserved beneath the judgement", err)
	}
}

// TestAPoolOpenedPastTheLimitStopsFailingWork.
//
// Every other test here primes the pool first, and that priming was hiding a
// bug that only a real server found. A run does not arrive one connection at a
// time: forty workers start together and dial in the same instant, so the first
// refusals land before anybody has finished any work. Judging those refusals
// against completed work meant judging them against nothing, so they read as
// "the server is gone" and the pool never narrowed at all.
//
// The measurement that matters is not the width but the work that fails.
// Against mox, asking for forty where thirty are allowed: 121 leases failed
// before this was right and 10 after, and the version that failed more was the
// one that settled on the *wider* number. A pool can hold a small width and
// still hand out concurrency it has already decided it cannot have, and every
// worker that takes some gets refused again.
// TestACrowdMeetingTheWallLosesNoWork is the mox scenario, in miniature.
//
// The pool is warm — something has already succeeded, so a refusal is judged
// rather than mistaken for the network — and then more workers than the server
// will hold arrive at once. That is what a run does when it reaches a folder
// wide enough to want every connection, and it is what asking mox for 36
// against its limit of 30 did: one worker's refusal narrowed the pool, and the
// refusals racing alongside it found the width already corrected and came back
// as four failed folders, rescued only by a retry pass 42 seconds later.
//
// Nothing here may fail. Every one of these workers is asking for a connection
// the server is holding perfectly happily; that they asked while the pool was
// still working out its width is not their problem.
func TestACrowdMeetingTheWallLosesNoWork(t *testing.T) {
	t.Parallel()

	const workers = 24
	w := &wall{limit: 5}
	p, err := pool.New(pool.Options{Cap: workers, Dial: w.dial})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	ctx := context.Background()

	// Priming is the whole difference from TestAPoolOpenedPastTheLimitStops-
	// FailingWork. A refusal that arrives before anything has ever succeeded
	// cannot be told apart from a dead network and is deliberately not judged,
	// so that pool is allowed its early failures. This one is not.
	if err := work(ctx, p); err != nil {
		t.Fatalf("priming lease: %v", err)
	}

	var (
		wg     sync.WaitGroup
		failed atomic.Int64
		sample atomic.Value
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				if err := workFor(ctx, p, 3*time.Millisecond); err != nil {
					failed.Add(1)
					sample.Store(err.Error())
				}
			}
		}()
	}
	wg.Wait()

	if n := failed.Load(); n > 0 {
		got, _ := sample.Load().(string)
		t.Errorf("%d of %d leases failed against a server that was holding its %d connections throughout; first was: %s",
			n, workers*8, w.limit, got)
	}

	if got := p.Width(); got > 5 {
		t.Errorf("Width() = %d, want the pool narrowed to the server's limit of 5", got)
	}
}

func TestAPoolOpenedPastTheLimitStopsFailingWork(t *testing.T) {
	t.Parallel()

	const workers = 24
	w := &wall{limit: 5}
	p, err := pool.New(pool.Options{Cap: workers, Dial: w.dial})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	ctx := context.Background()

	// No priming: this is the first thing that happens to the pool. Sustained
	// overlapping load, because that is the only condition under which excess
	// tokens turn into dials the server has to refuse.
	var (
		wg          sync.WaitGroup
		early, late atomic.Int64
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 8 {
				if err := workFor(ctx, p, 3*time.Millisecond); err != nil {
					if round < 2 {
						early.Add(1)
					} else {
						late.Add(1)
					}
				}
			}
		}()
	}
	wg.Wait()

	if got := p.Width(); got > 5 {
		t.Errorf("Width() = %d, want the pool narrowed to the server's limit of 5 without being primed", got)
	}

	// Failing while the pool works out where the wall is, is the design. Still
	// failing six rounds later is the bug.
	if n := late.Load(); n > 0 {
		t.Errorf("%d leases failed after the pool had settled (%d during); a settled pool must stop handing out concurrency it cannot use",
			n, early.Load())
	}
}
