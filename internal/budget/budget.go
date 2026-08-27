// Package budget bounds the bytes of message data a sync holds in memory.
//
// The connection pool already bounds concurrency: at most Cap fetches can be in
// flight because each needs a connection. That alone bounds memory at
// Cap × largest-message, which is fine when Cap is 8 and messages are small,
// and is 3 GB when Cap is 100 and someone mails a 30 MB attachment. A count of
// connections is the wrong unit for a limit on memory, because messages differ
// in size by four orders of magnitude.
//
// So this is a second, finer limit in the right unit. A worker charges the
// budget for the bytes it is about to hold and refunds them when the message
// has been appended and the buffer released.
//
// # What this deliberately does not do
//
// The design called for messages over about a megabyte to spool to a temporary
// file rather than memory. That is cut. With the budget charged in bytes, total
// memory is already bounded by the budget itself; spooling would lower peak RSS
// below the budget but would not change the guarantee, and it would buy that
// with temporary-file lifecycle, cleanup on crash, and disk-full as a new
// failure mode. If a single enormous message ever justifies it, adding it here
// is contained.
//
// The honest bound is therefore max(budget, largest single message): a message
// bigger than the whole budget is still read into memory, because refusing to
// copy it would be worse and blocking for ever would be worse still.
package budget

import (
	"context"
	"fmt"

	"golang.org/x/sync/semaphore"
)

// Budget is a semaphore counted in bytes. The zero value is not usable; call
// New. A nil *Budget is usable and charges nothing, which lets a caller that
// does not want a limit avoid branching on it.
type Budget struct {
	sem *semaphore.Weighted
	cap int64
}

// New creates a budget of the given size in bytes.
func New(bytes int64) (*Budget, error) {
	if bytes < 1 {
		return nil, fmt.Errorf("budget must be at least 1 byte, got %d", bytes)
	}
	return &Budget{sem: semaphore.NewWeighted(bytes), cap: bytes}, nil
}

// Cap reports the budget in bytes.
func (b *Budget) Cap() int64 {
	if b == nil {
		return 0
	}
	return b.cap
}

// Acquire charges the budget for size bytes, blocking until they are available
// or ctx is done. The returned function refunds them and is safe to call more
// than once, so it can be deferred and also called early.
//
// A request larger than the whole budget is charged the whole budget rather
// than being refused. Charging what was asked would block for ever, since no
// combination of refunds can ever satisfy it; refusing would mean a message
// that is merely large could never be copied. Taking everything makes it run
// alone, which is the closest thing to the intended behaviour that is possible.
//
// A size of zero or less is charged one byte, so that a server that fails to
// report RFC822.SIZE degrades to the connection-count limit rather than to no
// limit at all.
func (b *Budget) Acquire(ctx context.Context, size int64) (release func(), err error) {
	if b == nil {
		return func() {}, nil
	}

	n := size
	if n < 1 {
		n = 1
	}
	if n > b.cap {
		n = b.cap
	}

	if err := b.sem.Acquire(ctx, n); err != nil {
		return nil, fmt.Errorf("waiting for %d bytes of budget: %w", n, err)
	}

	released := false
	return func() {
		if released {
			return
		}
		released = true
		b.sem.Release(n)
	}, nil
}
