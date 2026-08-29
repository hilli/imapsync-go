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
	"time"

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

	// OnShrink, if set, is told when the pool gives up capacity. It exists so a
	// run can report the width it settled at, which is the only evidence that
	// says whether growing back would ever be worth building.
	OnShrink func(from, to int, cause error)
}

// capacityWindow is how recently another connection must have finished work for
// a failed dial to be read as the server's connection limit rather than as the
// network.
//
// It has to outlast one slow operation — a large fetch over a bad link — without
// outlasting a server restart, during which nothing succeeds anywhere. Ten
// seconds is a judgement call between those two, and no test here can tell me it
// is the right one: a test can only show that the mechanism honours whatever the
// window is. It is a constant rather than a flag because nobody can tune what
// they cannot observe.
const capacityWindow = 10 * time.Second

// Pool hands out exclusive leases on connections, dialling lazily up to Cap.
//
// Connections are dialled on demand rather than up front. A run that touches
// three small folders should not open thirty connections to discover it did not
// need them, and on a server that counts connections against a low ceiling the
// unused ones are actively harmful.
type Pool struct {
	dial     DialFunc
	sel      imapx.SelectOptions
	cap      int
	onShrink func(from, to int, cause error)

	// now is the clock. It is a field only so that an in-package test can move
	// time past capacityWindow without sleeping; nothing outside sets it, which
	// is why it is not in Options.
	now func() time.Time

	// tokens is the concurrency limit, nothing more. It is created holding Cap
	// values, and thereafter capacity can be neither leaked nor invented: it
	// can only be destroyed, in one place, by shrink. The bookkeeping that says
	// so is width and debt, and the invariant relating them to this channel is
	//
	//	tokens still in circulation == width + debt
	//
	// which is what Close relies on to know how many to wait for.
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

	// width is how many connections the pool currently allows itself, starting
	// at cap and only ever falling. debt is how much of the last reduction has
	// yet to be taken out of circulation, and open is how many connections
	// exist right now.
	width int
	debt  int
	open  int

	// lastOK is when a connection most recently finished work without error.
	// It is the whole basis for telling a connection limit from a network
	// fault: at a limit, the connections that got in keep working.
	lastOK time.Time
}

// conn is one pooled connection and what it currently has selected.
type conn struct {
	c        imapx.Conn
	selected string
	// uidValidity is what the server reported for selected. It is carried so
	// that a caller can tell whether the mailbox it is working on is still the
	// one it planned against; see Lease.UIDValidity.
	uidValidity uint32
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
		dial:     opts.Dial,
		sel:      opts.Select,
		cap:      opts.Cap,
		onShrink: opts.OnShrink,
		now:      time.Now,
		tokens:   make(chan struct{}, opts.Cap),
		done:     make(chan struct{}),
		width:    opts.Cap,
	}
	for range opts.Cap {
		p.tokens <- struct{}{}
	}
	return p, nil
}

// Cap reports the configured ceiling.
func (p *Pool) Cap() int { return p.cap }

// Width reports how many connections the pool currently allows itself, which is
// Cap until the server refuses one and never rises again afterwards.
func (p *Pool) Width() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.width
}

// Lease is exclusive use of one connection until Release.
type Lease struct {
	pool *Pool
	c    *conn
	done bool
}

// Conn returns the leased connection. It must not be used after Release, and
// must not be handed to another goroutine that outlives the lease.
func (l *Lease) Conn() imapx.Conn { return l.c.c }

// UIDValidity reports what the server said when this connection selected the
// mailbox, or 0 if the lease named none.
//
// A server may renumber a mailbox at any time, and announces it by changing
// this value on the next SELECT. One long-lived connection never sees that
// happen, because it never selects again; a pool that re-selects on every lease
// sees it immediately. Callers that planned work against a UID list compare
// this to the value they planned with, and abandon the folder when it differs,
// because every UID they hold now means something else or nothing.
//
// The value may be stale in the sense that it is from whenever this connection
// last selected. That is the strongest claim any IMAP client can make: nothing
// tells you a mailbox has been renumbered except selecting it again.
func (l *Lease) UIDValidity() uint32 { return l.c.uidValidity }

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
		// A worker whose dial was refused is holding a token the pool may have
		// already decided to destroy, and handing it straight back is what let
		// a shrunk pool go on being refused indefinitely: the excess tokens
		// simply went round again, each one producing another refusal. Debt is
		// paid by whichever token comes back first, and a failed acquire
		// returns one just as a finished lease does.
		if !p.payDebt() {
			p.tokens <- struct{}{}
		}
		return nil, err
	}
	return &Lease{pool: p, c: c}, nil
}

// ready produces a usable connection with the right mailbox selected, reusing
// an idle one where possible. On failure it closes whatever it could not make
// usable, so a SELECT that fails on a freshly dialled connection does not leave
// that connection open and unreachable.
func (p *Pool) ready(ctx context.Context, mailbox string) (*conn, error) {
	c, reused, err := p.take(ctx)
	if err != nil {
		return nil, err
	}

	// Selecting a mailbox that is already selected is a wasted round trip, and
	// on a chunked folder every worker after the first wants the mailbox the
	// connection already has.
	if mailbox == "" || c.selected == mailbox {
		return c, nil
	}
	mbox, err := c.c.Select(ctx, mailbox, p.sel)
	if err == nil {
		c.selected = mailbox
		c.uidValidity = mbox.UIDValidity
		return c, nil
	}
	_ = c.c.Close()

	// A SELECT that fails on a connection just dialled is a real failure. One
	// that fails on a connection taken from the idle list is more often a
	// server that hung up while it sat there — mox drops long-idle connections,
	// and the failure arrives as "use of closed network connection" against a
	// pool that still believes it holds a working socket.
	//
	// This is not a rare edge. It killed the two largest folders of a 776,791
	// message run, INBOX and Reklamer, at the point they had been going longest
	// and their spare connections had been idle longest, leaving 68,755
	// messages uncopied — while every short folder in the same run finished. So
	// a reused connection gets exactly one fresh dial before the error stands.
	if !reused {
		return nil, fmt.Errorf("selecting %q: %w", mailbox, err)
	}

	p.discard()

	fresh, _, freshErr := p.take(ctx)
	if freshErr != nil {
		return nil, fmt.Errorf("selecting %q: %w", mailbox, err)
	}
	mbox, freshErr = fresh.c.Select(ctx, mailbox, p.sel)
	if freshErr != nil {
		_ = fresh.c.Close()
		p.discard()
		return nil, fmt.Errorf("selecting %q: %w", mailbox, freshErr)
	}
	fresh.selected = mailbox
	fresh.uidValidity = mbox.UIDValidity
	return fresh, nil
}

// discard accounts for a connection this pool opened and has now closed
// outside the normal lease path, so the open count does not drift upwards and
// strand the pool below its own width.
//
// The count cannot go negative here: every caller has just closed a connection
// that take handed it, and take either dialled it — incrementing open — or
// popped it from the idle list, where only connections already counted live.
func (p *Pool) discard() {
	p.mu.Lock()
	p.open--
	p.mu.Unlock()
}

// take pops an idle connection, dialling only when there is none. The bool
// reports whether the connection came from the idle list, which is what tells
// a failure on it apart from a failure on a connection just proven to work.
//
// It does not re-check whether the pool has been closed. A caller that holds a
// token has, by construction, prevented Close from finishing, so the idle list
// is still intact and any connection dialled here is still closed by put once
// the lease ends. Checking anyway would only save a pointless dial in a race
// that no test can reach, at the cost of a branch nothing exercises.
func (p *Pool) take(ctx context.Context) (*conn, bool, error) {
	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		c := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return c, true, nil
	}
	p.mu.Unlock()

	c, err := p.dial(ctx)
	if err != nil {
		if p.refused(err) {
			return nil, false, fmt.Errorf("dialling: %w", errors.Join(imapx.ErrAtCapacity, err))
		}
		return nil, false, fmt.Errorf("dialling: %w", err)
	}

	p.mu.Lock()
	p.open++
	// A dial that completed is proof the server is alive and accepting, which
	// is exactly what a dial that failed alongside it is judged against. It is
	// recorded here rather than only on the return of a working connection
	// because of the cold start: every worker dials at once, so the first
	// refusals arrive before anybody has finished anything, and judging them
	// against completed work would let a pool opened well past a server's limit
	// sit there being refused for the length of the run.
	p.lastOK = p.now()
	p.mu.Unlock()
	return &conn{c: c}, false, nil
}

// refused decides whether a failed dial was the server's connection limit, and
// if so shrinks the pool to the width that is demonstrably working.
//
// The error cannot answer this on its own. A server at its limit may hang up
// during authentication, which arrives as an unexpected EOF and is
// indistinguishable from a dropped network connection — the same bytes, the same
// Go error. What separates them is the surroundings: at a connection limit, the
// connections that already got in carry on working, whereas a server that has
// restarted or a network that has gone is failing everything at once.
//
// So the question asked here is whether any connection finished work in the last
// capacityWindow. Answering no leaves the error classified as a retry, which
// costs one attempt if this really was the limit. Answering yes when the server
// has merely restarted would shrink the pool over a fault that has nothing to do
// with counting, and since the width never rises again that would cost the rest
// of the run. The cheaper mistake wins.
func (p *Pool) refused(cause error) bool {
	p.mu.Lock()

	if p.closed || p.lastOK.IsZero() || p.now().Sub(p.lastOK) > capacityWindow {
		p.mu.Unlock()
		return false
	}

	// A wall is met by every worker at once, so one refusal arrives per worker.
	// What stops that crowd walking the pool down to its floor is not a timer
	// or a flag but the arithmetic below: every one of them reads the same
	// number of open connections, so every one of them computes the same width,
	// and all but the first find nothing left to do.
	//
	// A cooldown was built here first, suppressing refusals until the previous
	// decision had been fully applied. Mutation testing could not find a
	// behaviour that changed when it was removed, and working out why showed it
	// was worse than redundant: the only case it altered was a connection
	// breaking while the pool still owed tokens, where shrinking again is the
	// correct answer and the cooldown withheld it.
	//
	// The connections currently open are exactly the ones the server is willing
	// to hold, so that is the answer rather than something to search for by
	// halving. Multiplicative decrease is for a limit you cannot see; this one
	// can be read off at the moment of refusal.
	to := max(p.open, 1)
	if to >= p.width {
		p.mu.Unlock()
		return true
	}

	from := p.width
	p.debt += p.width - to
	p.width = to

	// Pay what can be paid without waiting. A token sitting free belongs to
	// nobody, so taking it now costs no one anything and makes the new width
	// real immediately rather than whenever the next worker happens to finish.
	for p.debt > 0 && p.takeFreeToken() {
		p.debt--
	}

	closing := p.trimIdleLocked()
	p.mu.Unlock()

	for _, c := range closing {
		_ = c.c.Close()
	}
	if p.onShrink != nil {
		p.onShrink(from, to, cause)
	}
	return true
}

// takeFreeToken removes one token if one is free, without waiting. The caller
// holds mu; a non-blocking receive cannot block, so it cannot deadlock.
func (p *Pool) takeFreeToken() bool {
	select {
	case <-p.tokens:
		return true
	default:
		return false
	}
}

// trimIdleLocked removes idle connections until the pool holds no more than its
// width allows, returning them for the caller to close outside the lock.
//
// Closing idle connections is not an optimisation. The server counts
// connections, not busy ones, so a shrink that reduced only the concurrency
// limit would leave the same number of sockets open and change nothing at all on
// the wire.
func (p *Pool) trimIdleLocked() []*conn {
	var closing []*conn
	for p.open > p.width && len(p.idle) > 0 {
		n := len(p.idle) - 1
		closing = append(closing, p.idle[n])
		p.idle = p.idle[:n]
		p.open--
	}
	return closing
}

// put returns a connection, or closes it if it can no longer be trusted.
//
// This is also where a shrink is paid for. A returning connection is closed
// rather than parked when the pool now holds more than its width allows, and its
// token is swallowed rather than returned when the pool still owes one. The two
// are counted separately because they are separate resources: the server counts
// connections, while the token is what stops another worker opening a
// replacement.
func (p *Pool) put(c *conn, err error) {
	discard := errors.Is(err, imapx.ErrConnectionBroken)

	p.mu.Lock()
	if err == nil {
		// Proof that the server is alive and serving, which is what a later
		// failed dial is judged against.
		p.lastOK = p.now()
	}

	if p.closed || p.open > p.width {
		discard = true
	}

	if discard {
		p.open--
	} else {
		p.idle = append(p.idle, c)
	}
	p.mu.Unlock()

	if discard {
		_ = c.c.Close()
	}
	if !p.payDebt() {
		p.tokens <- struct{}{}
	}
}

// payDebt reports whether a token coming back should be destroyed rather than
// put back into circulation, and records the payment if so.
//
// A shrink cannot take tokens that are already lent out, so it records how many
// it is owed and collects them as they come home. Every path that gives a token
// back must come through here, or the pool keeps lending out concurrency it has
// already decided it cannot have.
func (p *Pool) payDebt() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Once closed, tokens must all come home: Close is counting them, and a
	// token swallowed after it started counting would hang the shutdown.
	if p.closed || p.debt == 0 {
		return false
	}
	p.debt--
	return true
}

// Close logs out and closes every connection the pool holds.
//
// It waits for outstanding leases to be released first, by taking every token
// still in circulation: holding them all means nobody else holds anything.
// Closing a connection out from under a command in flight would race with
// go-imap's own reader goroutine.
//
// The number to wait for is width+debt rather than cap, because a pool that has
// shrunk has permanently destroyed the difference. Setting closed under the
// mutex first is what makes that number safe to read: after it, no shrink starts
// and no token is swallowed, so the count cannot move while it is being counted.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	circulating := p.width + p.debt
	p.mu.Unlock()
	close(p.done)

	for range circulating {
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
