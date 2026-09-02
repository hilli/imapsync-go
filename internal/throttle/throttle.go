// Package throttle bounds how fast a sync moves message data.
//
// The connection pools bound how much happens at once; this bounds how much
// happens per second, which is a different question with a different answer.
// Lowering the pool width to slow a transfer down also removes the parallelism
// the tool exists for, and it does not actually bound bandwidth: one connection
// on a fast link saturates a household uplink, and forty against a folder of
// small messages move almost nothing.
//
// # Why this is not a sleep
//
// imapsync throttles by working out how long a message should have taken and
// sleeping the difference. That is a correct rate limit there because imapsync
// is sequential: one connection, one message at a time.
//
// Here it would be wrong by a factor equal to the concurrency. Forty fetchers
// each sleeping the same computed interval produce forty times the requested
// rate, and the faster the machine is configured the further off it lands. So
// this is one allowance shared by the whole run — a token bucket every worker
// draws from — rather than a delay each worker applies to itself.
//
// # What is counted
//
// Message bytes, which is what imapsync counts, so that a command line moved
// across from imapsync means the same thing rather than merely being accepted.
// Protocol overhead, TLS, and the header sweep that precedes a copy are not
// counted. Counting them would mean metering every connection and would still
// not produce imapsync's number.
//
// A message is charged once and crosses the wire twice, down from the source
// and up to the destination, so a limit of 1 MiB/s produces about 2 MiB/s of
// real traffic split between two hosts. The help text says so, because nothing
// about the flag's name suggests it.
package throttle

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a rate allowance shared by every worker in a run.
//
// A nil *Limiter is usable and charges nothing, which lets the copy path call
// Wait unconditionally rather than branching on whether a limit was asked for.
type Limiter struct {
	bytes    *rate.Limiter
	messages *rate.Limiter

	// bytesPerSec and messagesPerSec are kept as given so that the report can
	// state the limit that was in force. Reading them back off the rate.Limiter
	// would work today and would quietly become a different number if either
	// limit ever grew a burst that was not one second's worth.
	bytesPerSec    int64
	messagesPerSec float64

	waited atomic.Int64 // nanoseconds spent waiting, summed across workers
	moved  atomic.Int64 // message bytes that passed through
}

// New returns a limiter over the given per-second allowances. A limit of zero
// or less means that dimension is unbounded; if both are unbounded the result
// is nil, so a run without limits pays nothing at all.
func New(bytesPerSec int64, messagesPerSec float64) *Limiter {
	if bytesPerSec <= 0 && messagesPerSec <= 0 {
		return nil
	}
	l := &Limiter{bytesPerSec: bytesPerSec, messagesPerSec: messagesPerSec}
	if bytesPerSec > 0 {
		l.bytes = rate.NewLimiter(rate.Limit(bytesPerSec), burstFor(float64(bytesPerSec)))
	}
	if messagesPerSec > 0 {
		l.messages = rate.NewLimiter(rate.Limit(messagesPerSec), burstFor(messagesPerSec))
	}
	return l
}

// burstFor is one second's worth of an allowance, and at least one token.
//
// One second is enough to absorb the ordinary jitter of forty workers arriving
// together, and short enough that the opening burst is not a visible spike. The
// floor matters for a fractional limit: rate.NewLimiter with a burst of zero
// never admits anything, so "one message every ten seconds" would hang for ever
// instead of being slow.
func burstFor(perSecond float64) int {
	if perSecond < 1 {
		return 1
	}
	return int(perSecond)
}

// Wait blocks until the run's allowance covers one message of the given size,
// or until ctx is done.
//
// It is called before the body is read rather than after, because a limiter
// that waits once the bandwidth has already been spent is not limiting
// anything.
//
// A size of zero or less is charged nothing against the byte allowance but
// still costs one message, so a server that fails to report RFC822.SIZE
// degrades to the message limit rather than to no limit at all.
func (l *Limiter) Wait(ctx context.Context, size int64) error {
	if l == nil {
		return nil
	}

	started := time.Now()
	defer func() { l.waited.Add(int64(time.Since(started))) }()

	if l.messages != nil {
		if err := l.messages.Wait(ctx); err != nil {
			return fmt.Errorf("waiting for the message-rate allowance: %w", classifiable(ctx, err))
		}
	}
	if l.bytes != nil && size > 0 {
		if err := spend(ctx, l.bytes, size); err != nil {
			return fmt.Errorf("waiting for %d bytes of rate allowance: %w", size, classifiable(ctx, err))
		}
	}
	// Counted whether or not a byte limit is in force, and outside the branch
	// above for that reason. A run limited only by message rate still moves
	// bytes, and counting them only when the byte allowance was consulted made
	// the report say "having moved 0B" of eighty kilobytes that plainly had.
	if size > 0 {
		l.moved.Add(size)
	}
	return nil
}

// classifiable makes a refusal look like what it is.
//
// x/time/rate declines up front when it can see a wait will outlast the
// context's deadline, and it reports that in an error of its own rather than in
// the context's — "rate: Wait(n=1) would exceed context deadline", which wraps
// nothing.
//
// That shape is dangerous here rather than merely untidy. retry.Classify asks
// first, and deliberately, whether an error is a context error, so that Ctrl-C
// is not read as a broken connection; anything it does not recognise it
// classifies as one message to give up on and step past. A run stopped by its
// deadline while throttled would therefore not stop. It would record the
// message as failed, take the next one, be refused for the same reason, and
// work through the whole remaining folder that way — reporting a few hundred
// thousand failures where it should have reported one cancellation.
//
// A context that carries no deadline cannot have produced that refusal, so the
// error is left exactly as it came.
func classifiable(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if _, ok := ctx.Deadline(); ok {
		return fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
	}
	return err
}

// spend charges n tokens, in burst-sized instalments if n is larger than the
// bucket can hold.
//
// rate.Limiter.WaitN refuses outright when n exceeds the burst, so a 30 MB
// message against a 1 MiB/s limit would be an error rather than a wait. The
// obvious alternative — clamping the charge to the burst — is wrong in the
// dangerous direction, because a run of large messages would then be
// systematically under-charged and would exceed the limit that was asked for.
//
// Instalments give the right answer: thirty waits of about a second, totalling
// about thirty seconds. They also stop one huge message blocking the head of
// the line, because it re-queues between instalments and small messages
// interleave with it.
func spend(ctx context.Context, l *rate.Limiter, n int64) error {
	burst := int64(l.Burst())
	for n > 0 {
		take := min(n, burst)
		if err := l.WaitN(ctx, int(take)); err != nil {
			return err
		}
		n -= take
	}
	return nil
}

// Stats is what a run spent against its allowance.
type Stats struct {
	// BytesPerSec and MessagesPerSec are the limits that were in force. Zero
	// means that dimension was unbounded.
	BytesPerSec    int64
	MessagesPerSec float64

	// Waited is the total time workers spent blocked on the allowance, summed
	// across them. It exceeds the wall clock when several waited at once, which
	// is the normal case and is why it is not reported as a percentage.
	//
	// This is the number that makes the feature debuggable: without it a
	// throttled run and a slow server look identical, and the user cannot tell
	// whether their own brake or the network is the constraint.
	Waited time.Duration

	// Moved is the message bytes that passed through the limiter. It is what
	// the run moved, not what the byte allowance was charged: a run held only
	// to a message rate still moves bytes and should say how many.
	Moved int64
}

// Stats reports what was spent. The zero value is returned for a nil limiter,
// which is the honest answer: no limit was in force and nothing waited.
func (l *Limiter) Stats() Stats {
	if l == nil {
		return Stats{}
	}
	return Stats{
		BytesPerSec:    l.bytesPerSec,
		MessagesPerSec: l.messagesPerSec,
		Waited:         time.Duration(l.waited.Load()),
		Moved:          l.moved.Load(),
	}
}

// Set reports whether any limit is in force.
func (l *Limiter) Set() bool { return l != nil }
