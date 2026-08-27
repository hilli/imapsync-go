// Package pool leases IMAP connections to concurrent workers.
//
// IMAP has no multiplexing: one connection carries one command at a time, and
// the reply to that command may be interleaved with untagged updates that only
// make sense in the context of the connection's own selected mailbox. Sharing a
// connection between two goroutines is therefore not merely racy, it is
// meaningless. A lease is exclusive for its whole duration and that is the
// single invariant this package exists to enforce.
//
// The two sides of a sync want different things from a pool, which is why the
// mailbox is a parameter of Acquire rather than of New:
//
//   - Source connections are folder-bound. Reading messages requires a selected
//     mailbox, so a lease is tied to one folder for its lifetime.
//   - Destination connections are folder-agnostic. APPEND names its target
//     mailbox, so one destination pool serves every folder at once and never
//     needs to select anything. Only the reconcile stage, which sets flags and
//     expunges, needs a folder-bound destination lease.
package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hilli/imapsync-go/internal/imapx"
)

// ErrClosed is returned by Acquire after Close.
var ErrClosed = errors.New("pool is closed")

// DialFunc opens one new authenticated connection.
type DialFunc func(ctx context.Context) (imapx.Conn, error)

// Options configures a Pool.
type Options struct {
	// Cap is the greatest number of connections that may exist at once.
	// Acquire blocks while all of them are leased.
	Cap int

	// Dial opens a connection. It is called at most Cap times concurrently.
	Dial DialFunc

	// Select is passed to imapx.Select when a lease names a mailbox. A source
	// pool wants ReadOnly true: EXAMINE avoids setting \Seen on messages
	// merely by reading them, which would silently rewrite the source.
	Select imapx.SelectOptions
}

// Pool hands out exclusive leases on connections, dialling lazily up to Cap.
//
// Connections are dialled on demand rather than up front. A run that touches
// three small folders should not open thirty connections to discover it did not
// need them, and on a server that counts connections against a low ceiling the
// unused ones are actively harmful.
type Pool struct {
	dial DialFunc
	sel  imapx.SelectOptions
	cap  int

	// tokens is the concurrency limit, nothing more. It holds Cap values for
	// the pool's whole life, so capacity can be neither leaked nor invented.
	tokens chan struct{}

	// done is closed by Close. Without it, an Acquire already blocked on
	// tokens when Close drains them would block for ever rather than returning
	// ErrClosed. The tokens channel itself is never closed: a Release racing
	// with Close still has to send, and sending on a closed channel panics.
	done chan struct{}

	mu sync.Mutex
	// idle holds connections nobody is using. Acquire prefers one of these to
	// dialling, which is what keeps a pool with a generous cap from opening
	// its whole cap to do one thing at a time: against a server that counts
	// connections against a low ceiling, the ones it did not need are not
	// merely wasteful, they crowd out the ones doing work.
	//
	// It is a stack because that is the cheapest structure that does the job.
	// Nothing observable turns on the order while there is no idle timeout, so
	// nothing here should be read as claiming that it does.
	idle   []*conn
	closed bool
}

// conn is one pooled connection and what it currently has selected.
type conn struct {
	c        imapx.Conn
	selected string
}

// New creates a pool. It dials nothing; the first Acquire does that.
func New(opts Options) (*Pool, error) {
	if opts.Cap < 1 {
		return nil, fmt.Errorf("pool cap must be at least 1, got %d", opts.Cap)
	}
	if opts.Dial == nil {
		return nil, errors.New("pool requires a Dial function")
	}

	p := &Pool{
		dial:   opts.Dial,
		sel:    opts.Select,
		cap:    opts.Cap,
		tokens: make(chan struct{}, opts.Cap),
		done:   make(chan struct{}),
	}
	for range opts.Cap {
		p.tokens <- struct{}{}
	}
	return p, nil
}

// Cap reports the configured ceiling.
func (p *Pool) Cap() int { return p.cap }

// Lease is exclusive use of one connection until Release.
type Lease struct {
	pool *Pool
	c    *conn
	done bool
}

// Conn returns the leased connection. It must not be used after Release, and
// must not be handed to another goroutine that outlives the lease.
func (l *Lease) Conn() imapx.Conn { return l.c.c }

// Release returns the connection to the pool.
//
// Pass the error from the work done under the lease, or nil. A connection whose
// error was imapx.ErrConnectionBroken is closed rather than reused: that error
// means the protocol stream is no longer in a known state, usually because an
// APPEND literal was cut short, and the next command on it would be answered by
// bytes belonging to the last one. Returning it to the pool would spread one
// failure across every later user of that connection.
//
// Release is idempotent so that a deferred Release is safe alongside an
// explicit one.
func (l *Lease) Release(err error) {
	if l.done {
		return
	}
	l.done = true
	l.pool.put(l.c, err)
}

// Acquire leases a connection, blocking until one is free or ctx is done.
//
// When mailbox is not empty the connection is left with that mailbox selected.
// Pass "" for work that names its own mailbox, which on the destination side is
// every APPEND.
func (p *Pool) Acquire(ctx context.Context, mailbox string) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	select {
	case <-p.tokens:
	case <-p.done:
		return nil, ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	c, err := p.ready(ctx, mailbox)
	if err != nil {
		p.tokens <- struct{}{}
		return nil, err
	}
	return &Lease{pool: p, c: c}, nil
}

// ready produces a usable connection with the right mailbox selected, reusing
// an idle one where possible. On failure it closes whatever it could not make
// usable, so a SELECT that fails on a freshly dialled connection does not leave
// that connection open and unreachable.
func (p *Pool) ready(ctx context.Context, mailbox string) (*conn, error) {
	c, err := p.take(ctx)
	if err != nil {
		return nil, err
	}

	// Selecting a mailbox that is already selected is a wasted round trip, and
	// on a chunked folder every worker after the first wants the mailbox the
	// connection already has.
	if mailbox == "" || c.selected == mailbox {
		return c, nil
	}
	if _, err := c.c.Select(ctx, mailbox, p.sel); err != nil {
		_ = c.c.Close()
		return nil, fmt.Errorf("selecting %q: %w", mailbox, err)
	}
	c.selected = mailbox
	return c, nil
}

// take pops an idle connection, dialling only when there is none.
//
// It does not re-check whether the pool has been closed. A caller that holds a
// token has, by construction, prevented Close from finishing, so the idle list
// is still intact and any connection dialled here is still closed by put once
// the lease ends. Checking anyway would only save a pointless dial in a race
// that no test can reach, at the cost of a branch nothing exercises.
func (p *Pool) take(ctx context.Context) (*conn, error) {
	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return c, nil
	}
	p.mu.Unlock()

	c, err := p.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dialling: %w", err)
	}
	return &conn{c: c}, nil
}

// put returns a connection, or closes it if it can no longer be trusted.
func (p *Pool) put(c *conn, err error) {
	discard := errors.Is(err, imapx.ErrConnectionBroken)

	p.mu.Lock()
	if p.closed {
		discard = true
	}
	if !discard {
		p.idle = append(p.idle, c)
	}
	p.mu.Unlock()

	if discard {
		_ = c.c.Close()
	}
	p.tokens <- struct{}{}
}

// Close logs out and closes every connection the pool holds.
//
// It waits for outstanding leases to be released first, by taking every token:
// since the number of tokens is fixed, holding them all means nobody else holds
// anything. Closing a connection out from under a command in flight would race
// with go-imap's own reader goroutine.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	close(p.done)

	for range p.cap {
		<-p.tokens
	}

	p.mu.Lock()
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()

	var errs []error
	for _, c := range idle {
		if err := c.c.Logout(ctx); err != nil {
			errs = append(errs, err)
		}
		if err := c.c.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
