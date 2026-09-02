package throttle_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/retry"
	"github.com/hilli/imapsync-go/internal/throttle"
)

// Every timing assertion in this file is a lower bound, and that is deliberate.
//
// A lower bound on elapsed time is safe on a loaded machine, because load can
// only make a run slower. An upper bound is the flaky kind, and this project
// has already had to remove one. Where an exact answer is available it is
// asserted on the accounting instead of on the clock.
//
// The bounds themselves are exact rather than approximate: rate.Limiter starts
// with a full bucket, so n tokens are available at burst + rate×t, and the time
// to spend c of them is (c - burst)/rate. The slack below is for clock
// granularity, not for the arithmetic.

func TestNoLimitIsNoLimiter(t *testing.T) {
	t.Parallel()

	if l := throttle.New(0, 0); l != nil {
		t.Fatalf("New(0, 0) = %v, want nil so an unlimited run pays nothing", l)
	}
	if l := throttle.New(-1, -1); l != nil {
		t.Fatalf("New(-1, -1) = %v, want nil", l)
	}
}

func TestANilLimiterChargesNothing(t *testing.T) {
	t.Parallel()

	var l *throttle.Limiter

	started := time.Now()
	for range 100 {
		if err := l.Wait(context.Background(), 1<<30); err != nil {
			t.Fatalf("Wait on a nil limiter: %v", err)
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("100 gigabyte charges against a nil limiter took %s", elapsed)
	}

	if got := l.Stats(); got != (throttle.Stats{}) {
		t.Errorf("Stats on a nil limiter = %+v, want the zero value", got)
	}
	if l.Set() {
		t.Error("Set on a nil limiter is true; a nil limiter is no limit")
	}
}

// TestTheAllowanceIsSharedByEveryWorker is the reason this package exists.
//
// imapsync throttles by sleeping between messages, which is a rate limit
// because imapsync is sequential. Ported here unchanged it would be wrong by a
// factor equal to the concurrency: each of forty fetchers would sleep its own
// interval and the run would move forty times the requested bytes.
//
// So the assertion is about six workers *together*. If each held its own
// allowance, every one of them would fit inside its own burst and the whole
// thing would finish in microseconds — which is exactly what this fails on.
func TestTheAllowanceIsSharedByEveryWorker(t *testing.T) {
	t.Parallel()

	const (
		perSecond = 1 << 20 // burst is one second's worth, so 1 MiB
		workers   = 6
		each      = 256 << 10 // 1.5 MiB in total
	)
	// 1.5 MiB needs (1.5 - 1) MiB beyond the opening burst, at 1 MiB/s.
	const want = 500 * time.Millisecond

	l := throttle.New(perSecond, 0)
	started := time.Now()

	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = l.Wait(context.Background(), each)
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	if elapsed < want-50*time.Millisecond {
		t.Errorf("%d workers moved %d bytes in %s against a limit of %d bytes/second;\n"+
			"that is at least %s of work, so the allowance is being held per worker rather than shared",
			workers, workers*each, elapsed, perSecond, want)
	}
	if got := l.Stats().Moved; got != workers*each {
		t.Errorf("charged %d bytes, want %d", got, workers*each)
	}
}

// TestAMessageLargerThanTheBucketIsChargedInFull covers the burst problem.
//
// rate.Limiter.WaitN refuses outright when the charge exceeds the burst, so a
// large message against a small limit would be an error rather than a wait.
// Instalments are the fix; clamping the charge to the burst is the alternative
// that looks equivalent and is not, because it under-charges in exactly the
// direction that exceeds the limit the user asked for.
//
// The timing is what separates the two. Clamping would let 6 MiB through for
// the price of 4 and finish inside the opening burst.
func TestAMessageLargerThanTheBucketIsChargedInFull(t *testing.T) {
	t.Parallel()

	const (
		perSecond = 4 << 20 // burst is 4 MiB
		message   = 6 << 20 // half again as much as the bucket can hold
	)
	// 6 MiB needs (6 - 4) MiB beyond the opening burst, at 4 MiB/s.
	const want = 500 * time.Millisecond

	l := throttle.New(perSecond, 0)
	started := time.Now()
	if err := l.Wait(context.Background(), message); err != nil {
		t.Fatalf("charging a message larger than the burst: %v", err)
	}
	elapsed := time.Since(started)

	if elapsed < want-50*time.Millisecond {
		t.Errorf("charged %d bytes against a %d bytes/second limit in %s, want at least %s;\n"+
			"the charge is being clamped to the burst rather than paid in instalments",
			message, perSecond, elapsed, want)
	}
	if got := l.Stats().Moved; got != message {
		t.Errorf("charged %d bytes, want the whole %d", got, message)
	}
}

// TestAMessageOfUnknownSizeStillCostsAMessage covers a server that does not
// report RFC822.SIZE. Charging nothing for it would leave such a run with no
// limit at all, so it degrades to the message allowance rather than to nothing.
func TestAMessageOfUnknownSizeStillCostsAMessage(t *testing.T) {
	t.Parallel()

	const perSecond = 2 // burst 2
	// Three messages need one beyond the opening burst, at 2 a second.
	const want = 500 * time.Millisecond

	l := throttle.New(0, perSecond)
	started := time.Now()
	for range 3 {
		if err := l.Wait(context.Background(), 0); err != nil {
			t.Fatalf("charging a message of unknown size: %v", err)
		}
	}
	elapsed := time.Since(started)

	if elapsed < want-50*time.Millisecond {
		t.Errorf("3 messages of unknown size against %d/second took %s, want at least %s",
			perSecond, elapsed, want)
	}
	if got := l.Stats().Moved; got != 0 {
		t.Errorf("charged %d bytes for messages of unknown size, want 0", got)
	}
}

// TestAFractionalMessageLimitIsSlowRatherThanStuck.
//
// rate.NewLimiter with a burst of zero admits nothing ever, so a limit below
// one message a second would hang the run instead of slowing it. The floor in
// burstFor is what stops that, and this is the test that would notice its
// removal.
func TestAFractionalMessageLimitIsSlowRatherThanStuck(t *testing.T) {
	t.Parallel()

	l := throttle.New(0, 0.5)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := l.Wait(ctx, 100); err != nil {
		t.Fatalf("the first message under a limit of one every two seconds: %v", err)
	}
}

func TestBothAllowancesApply(t *testing.T) {
	t.Parallel()

	l := throttle.New(1<<20, 4)
	if !l.Set() {
		t.Error("Set is false on a limiter with two limits")
	}

	got := l.Stats()
	if got.BytesPerSec != 1<<20 {
		t.Errorf("Stats.BytesPerSec = %d, want %d", got.BytesPerSec, 1<<20)
	}
	if got.MessagesPerSec != 4 {
		t.Errorf("Stats.MessagesPerSec = %v, want 4", got.MessagesPerSec)
	}
}

// TestOnlyTheLimitAskedForIsReported keeps the report honest: a run that set a
// byte limit and no message limit must not be told it had one.
func TestOnlyTheLimitAskedForIsReported(t *testing.T) {
	t.Parallel()

	if got := throttle.New(1<<20, 0).Stats(); got.MessagesPerSec != 0 {
		t.Errorf("Stats.MessagesPerSec = %v with no message limit set, want 0", got.MessagesPerSec)
	}
	if got := throttle.New(0, 8).Stats(); got.BytesPerSec != 0 {
		t.Errorf("Stats.BytesPerSec = %d with no byte limit set, want 0", got.BytesPerSec)
	}
}

// TestWaitingIsCounted. Without this number a throttled run and a slow server
// are indistinguishable, and the user cannot tell which one is the constraint.
func TestWaitingIsCounted(t *testing.T) {
	t.Parallel()

	// Burst 1000; charging 1500 needs half a second beyond it.
	l := throttle.New(1000, 0)
	if err := l.Wait(context.Background(), 1500); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if got := l.Stats().Waited; got < 450*time.Millisecond {
		t.Errorf("Stats.Waited = %s after a wait of about half a second", got)
	}
}

// TestACancelledContextStopsTheRunRatherThanTheMessage.
//
// The assertion is on retry.Classify rather than on the error's shape, because
// the shape is only interesting for what the copy path does with it. x/time/rate
// declines up front when it can see a wait will outlast the context's deadline,
// and says so in an error of its own that wraps nothing — and retry.Classify
// treats an error it does not recognise as one message to give up on and step
// past. A throttled run that hit its deadline would then not stop: it would
// write off every remaining message in the folder, one refusal at a time, and
// report a few hundred thousand failures instead of one cancellation.
func TestACancelledContextStopsTheRunRatherThanTheMessage(t *testing.T) {
	t.Parallel()

	l := throttle.New(1, 0) // one byte a second: the charge below never arrives

	for _, tc := range []struct {
		name string
		ctx  func(t *testing.T) context.Context
	}{
		{"a deadline the wait cannot meet", func(t *testing.T) context.Context {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			t.Cleanup(cancel)
			return ctx
		}},
		{"a deadline already passed", func(t *testing.T) context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			t.Cleanup(cancel)
			return ctx
		}},
		{"a cancelled context", func(_ *testing.T) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := l.Wait(tc.ctx(t), 1<<20)
			if err == nil {
				t.Fatal("Wait returned nil for a charge the context could not outlive")
			}
			if got := retry.Classify(err); got != retry.Stop {
				t.Errorf("retry.Classify(%v) = %v, want %v;\n"+
					"the copy path would write the message off as failed and try the next one",
					err, got, retry.Stop)
			}
		})
	}
}

// TestAnAlreadyCancelledContextIsRefusedByEitherLimit makes sure neither
// allowance swallows a cancellation the other would have reported.
func TestAnAlreadyCancelledContextIsRefusedByEitherLimit(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		l    *throttle.Limiter
	}{
		{"bytes only", throttle.New(1<<20, 0)},
		{"messages only", throttle.New(0, 100)},
		{"both", throttle.New(1<<20, 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.l.Wait(ctx, 1024); !errors.Is(err, context.Canceled) {
				t.Errorf("Wait on a cancelled context = %v, want context.Canceled", err)
			}
		})
	}
}

// TestAnOrdinaryFailureIsNotDressedUpAsADeadline keeps classifiable narrow: it
// may only reinterpret an error when the context has a deadline that could have
// caused it.
func TestAnOrdinaryFailureIsNotDressedUpAsADeadline(t *testing.T) {
	t.Parallel()

	// No deadline, and an allowance small enough that a large charge would
	// take days. It must simply wait, not be turned into a deadline error.
	l := throttle.New(1, 0)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- l.Wait(ctx, 1<<20) }()

	select {
	case err := <-done:
		t.Fatalf("Wait returned %v on a context with no deadline; it should have waited", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Wait error after cancel = %v, want context.Canceled", err)
	}
}

// TestAnInterruptedRunIsNotReportedAsATimeout.
//
// Written because a mutation survived: deleting classifiable's first guard —
// the one that leaves an error alone when it is already a context error —
// changed nothing any test could see.
//
// It changes nothing about what the run does, which is why it went unnoticed.
// retry.Classify reads a wrapped context.Canceled as Stop whether or not a
// context.DeadlineExceeded has been laid over the top of it, so the run stops
// either way. What it changes is what the person who pressed Ctrl-C is told:
// "context deadline exceeded: context canceled", which reads as the run having
// run out of time rather than having been interrupted. A long migration is
// interrupted deliberately and often — the tool says so in as many words — so
// telling that person their run timed out is a lie they might act on.
//
// The context here carries a deadline as well, because that is the only
// arrangement in which the guard does any work.
func TestAnInterruptedRunIsNotReportedAsATimeout(t *testing.T) {
	t.Parallel()

	// One byte a second, so a 1 MiB charge cannot finish, and a deadline far
	// enough away that it is not what ends the wait.
	l := throttle.New(1, 0)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- l.Wait(ctx, 1<<20) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error after cancel = %v, want context.Canceled", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an interrupted run reports %q, which says the deadline passed;\n"+
			"the deadline is an hour away and the run was cancelled", err)
	}
}

// TestARunHeldOnlyToAMessageRateStillSaysWhatItMoved.
//
// Found by a live run, not by a test: twenty messages of four kilobytes each,
// held to five messages a second, reported "having moved 0B of message data".
// The bytes were counted inside the branch that consults the byte allowance,
// so a run with no byte limit counted nothing.
//
// Nobody would have lost mail over it. What they would have lost is the ability
// to answer the question the note exists for — whether the brake is what is
// holding the run up, and at what cost — from a note that reports a real
// transfer as nothing at all.
func TestARunHeldOnlyToAMessageRateStillSaysWhatItMoved(t *testing.T) {
	t.Parallel()

	const (
		messages = 4
		each     = 4096
	)
	l := throttle.New(0, 1000) // messages only, fast enough not to wait
	for range messages {
		if err := l.Wait(context.Background(), each); err != nil {
			t.Fatalf("charging: %v", err)
		}
	}

	if got := l.Stats().BytesPerSec; got != 0 {
		t.Fatalf("the limiter has a byte limit of %d; this test needs one with none", got)
	}
	if got, want := l.Stats().Moved, int64(messages*each); got != want {
		t.Errorf("a run limited only by message rate reports %d bytes moved, want %d", got, want)
	}
}
