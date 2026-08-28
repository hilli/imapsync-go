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

// fakeConn records what was done to it and can be told to fail on demand.
type fakeConn struct {
	mu sync.Mutex

	selects  []string
	selErr   error
	closed   int
	loggedOu int

	// busy is set while a caller holds this connection and cleared on release,
	// so a second concurrent user is detectable rather than merely unlikely.
	busy atomic.Bool
}

func (f *fakeConn) Select(_ context.Context, mailbox string, _ imapx.SelectOptions) (imapx.Mailbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.selErr != nil {
		return imapx.Mailbox{}, f.selErr
	}
	f.selects = append(f.selects, mailbox)
	return imapx.Mailbox{Name: mailbox}, nil
}

func (f *fakeConn) selected() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.selects...)
}

func (f *fakeConn) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeConn) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loggedOu++
	return nil
}

func (f *fakeConn) setSelErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selErr = err
}

func (f *fakeConn) counts() (closed, loggedOut int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed, f.loggedOu
}

func (f *fakeConn) Caps() imapx.Caps { return imapx.Caps{} }
func (f *fakeConn) Namespaces(context.Context) (imapx.Namespaces, error) {
	return imapx.Namespaces{}, nil
}

func (f *fakeConn) ListFolders(context.Context, imapx.ListOptions) ([]imapx.Folder, error) {
	return nil, nil
}
func (f *fakeConn) CreateFolder(context.Context, string) error    { return nil }
func (f *fakeConn) SubscribeFolder(context.Context, string) error { return nil }
func (f *fakeConn) AllUIDs(context.Context) ([]uint32, error)     { return nil, nil }
func (f *fakeConn) FetchMeta(context.Context, []uint32, []string) ([]imapx.MessageMeta, error) {
	return nil, nil
}
func (f *fakeConn) FetchBody(context.Context, uint32, io.Writer) (int64, error) { return 0, nil }
func (f *fakeConn) Append(context.Context, string, imapx.AppendMessage) (imapx.AppendResult, error) {
	return imapx.AppendResult{}, nil
}
func (f *fakeConn) SearchHeader(context.Context, string, string) ([]uint32, error) { return nil, nil }

func (f *fakeConn) FetchFlags(context.Context, uint64) ([]imapx.FlagSet, error) { return nil, nil }

func (f *fakeConn) StoreFlags(context.Context, uint32, []string) error { return nil }

func (f *fakeConn) DeleteMessages(context.Context, []uint32) error { return nil }

// dialer hands out fakeConns and counts how many it was asked for.
type dialer struct {
	mu     sync.Mutex
	conns  []*fakeConn
	err    error
	failAt int // dial number, 1-based, that returns err; 0 means always
}

func (d *dialer) dial(context.Context) (imapx.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := len(d.conns) + 1
	if d.err != nil && (d.failAt == 0 || d.failAt == n) {
		return nil, d.err
	}
	c := &fakeConn{}
	d.conns = append(d.conns, c)
	return c, nil
}

func (d *dialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.conns)
}

func newPool(t *testing.T, capacity int, d *dialer) *pool.Pool {
	t.Helper()
	p, err := pool.New(pool.Options{Cap: capacity, Dial: d.dial})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	return p
}

// The whole reason this package exists. IMAP carries one command at a time, so
// two goroutines on one connection is not a race to be tuned away, it is a
// protocol violation.
func TestALeasedConnectionIsNeverSharedConcurrently(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 4, d)

	var wg sync.WaitGroup
	var overlaps atomic.Int64
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				l, err := p.Acquire(context.Background(), "INBOX")
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				fc := l.Conn().(*fakeConn)
				if !fc.busy.CompareAndSwap(false, true) {
					overlaps.Add(1)
				}
				time.Sleep(time.Microsecond)
				fc.busy.Store(false)
				l.Release(nil)
			}
		}()
	}
	wg.Wait()

	if got := overlaps.Load(); got != 0 {
		t.Fatalf("%d concurrent uses of one connection", got)
	}
}

// Dialling more connections than the cap is how you get banned from a server
// that counts them, which is the situation iCloud is in.
func TestTheCapIsNeverExceeded(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 3, d)

	var live, peak atomic.Int64
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(context.Background(), "")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			n := live.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(200 * time.Microsecond)
			live.Add(-1)
			l.Release(nil)
		}()
	}
	wg.Wait()

	if peak.Load() > 3 {
		t.Errorf("peak concurrent leases = %d, cap is 3", peak.Load())
	}
	if d.count() > 3 {
		t.Errorf("dialled %d connections, cap is 3", d.count())
	}
}

// A run over three small folders should not open thirty connections to find out
// it did not need them. On a server with a low ceiling the unused ones are not
// merely wasteful, they crowd out the ones doing work.
func TestConnectionsAreDialledOnlyWhenNeeded(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 10, d)

	if d.count() != 0 {
		t.Fatalf("New dialled %d connections; it should dial none", d.count())
	}
	for range 5 {
		l, err := p.Acquire(context.Background(), "")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		l.Release(nil)
	}
	if d.count() != 1 {
		t.Errorf("five sequential leases dialled %d connections, want 1", d.count())
	}
}

// A broken connection is one whose protocol stream is no longer in a known
// state, typically a cut-short APPEND literal. Reusing it answers the next
// command with bytes belonging to the last one, turning one failure into every
// later failure on that connection.
func TestABrokenConnectionIsNotReused(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 1, d)

	first, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	broken := first.Conn().(*fakeConn)
	first.Release(imapx.ErrConnectionBroken)

	if closed, _ := broken.counts(); closed != 1 {
		t.Errorf("broken connection closed %d times, want 1", closed)
	}

	second, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire after break: %v", err)
	}
	defer second.Release(nil)

	if second.Conn() == broken {
		t.Error("the broken connection was handed out again")
	}
	if d.count() != 2 {
		t.Errorf("dialled %d connections, want a replacement to have been dialled", d.count())
	}
}

// An ordinary error says nothing about the connection. A folder that does not
// exist, a message that vanished: the stream is fine and throwing the
// connection away would mean re-dialling on every routine failure.
func TestAnOrdinaryErrorKeepsTheConnection(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 1, d)

	first, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	kept := first.Conn()
	first.Release(errors.New("mailbox does not exist"))

	second, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer second.Release(nil)

	if second.Conn() != kept {
		t.Error("an ordinary error discarded the connection")
	}
	if d.count() != 1 {
		t.Errorf("dialled %d connections, want 1", d.count())
	}
}

// On a folder split into chunks, every worker after the first wants the mailbox
// the connection already has. Selecting it again is a wasted round trip on a
// server whose round trips are the bottleneck.
func TestSelectIsSkippedWhenTheMailboxIsAlreadySelected(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 1, d)

	for range 4 {
		l, err := p.Acquire(context.Background(), "INBOX")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		l.Release(nil)
	}
	l, err := p.Acquire(context.Background(), "Archive")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release(nil)

	got := d.conns[0].selected()
	want := []string{"INBOX", "Archive"}
	if len(got) != len(want) {
		t.Fatalf("selected %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selected %v, want %v", got, want)
		}
	}
}

// Naming no mailbox is what the destination side does for every APPEND, which
// carries its own target. Selecting one anyway is a round trip per message, and
// selecting the empty string is not a command any server will accept.
//
// The connection must already have something selected for this to prove
// anything: on a fresh connection "no mailbox wanted" and "the mailbox I want
// is already selected" are the same empty string, and a pool that confused the
// two would pass anyway.
func TestAnEmptyMailboxSelectsNothing(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 1, d)

	first, err := p.Acquire(context.Background(), "INBOX")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	first.Release(nil)

	second, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	second.Release(nil)

	got := d.conns[0].selected()
	if len(got) != 1 || got[0] != "INBOX" {
		t.Errorf("selected %v, want only the one explicit INBOX", got)
	}
}

// A SELECT that fails on a freshly dialled connection must not leave that
// connection open and unreachable: the pool would slowly leak its whole cap
// against a server that counts connections.
func TestAFailedSelectDoesNotLeakTheConnection(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	// A cap of one, so the connection whose SELECT is about to fail is
	// necessarily the one the next Acquire receives. With a larger cap the
	// pool hands out an unused slot first and the test proves nothing.
	p := newPool(t, 1, d)

	l, err := p.Acquire(context.Background(), "INBOX")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	first := l.Conn().(*fakeConn)
	first.setSelErr(errors.New("no such mailbox"))
	l.Release(nil)

	if _, err := p.Acquire(context.Background(), "Nope"); err == nil {
		t.Fatal("want the select failure to surface")
	}
	if closed, _ := first.counts(); closed != 1 {
		t.Errorf("connection closed %d times after a failed select, want 1", closed)
	}

	// The capacity must have come back, or the pool shrinks by one every time
	// a select fails and eventually stops working altogether.
	next, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire after a failed select: %v", err)
	}
	defer next.Release(nil)
	if next.Conn() == first {
		t.Error("the connection whose select failed was handed out again")
	}
}

// A dial failure must give the capacity back too, and must surface rather than
// blocking the caller for ever.
func TestADialFailureReturnsCapacity(t *testing.T) {
	t.Parallel()

	d := &dialer{err: errors.New("connection refused")}
	p := newPool(t, 1, d)

	for range 3 {
		if _, err := p.Acquire(context.Background(), ""); err == nil {
			t.Fatal("want the dial failure to surface")
		}
	}
}

func TestAcquireRespectsContextCancellation(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 1, d)

	held, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := p.Acquire(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Acquire took %s to notice a 20ms deadline", elapsed)
	}
}

// Close waits for outstanding leases because closing a connection under a
// command in flight races with go-imap's own reader goroutine.
func TestCloseWaitsForOutstandingLeases(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p, err := pool.New(pool.Options{Cap: 2, Dial: d.dial})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	l, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- p.Close(context.Background()) }()

	select {
	case <-closed:
		t.Fatal("Close returned while a lease was outstanding")
	case <-time.After(50 * time.Millisecond):
	}

	l.Release(nil)

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the lease was released")
	}
}

// An Acquire already blocked when Close is called must be told the pool is
// closed. Before the done channel existed it blocked for ever, because Close
// takes every token and nothing ever puts one back.
//
// The held lease is deliberately not released until after the assertion. If it
// were, the blocked Acquire might be woken by the freed token rather than by
// Close, and the test would pass with the done channel removed.
func TestCloseUnblocksAWaitingAcquire(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p, err := pool.New(pool.Options{Cap: 1, Dial: d.dial})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	held, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	waiting := make(chan error, 1)
	go func() {
		_, err := p.Acquire(context.Background(), "")
		waiting <- err
	}()
	time.Sleep(20 * time.Millisecond)

	// Close cannot return while the lease is held, so it runs in the
	// background; what matters is that it has closed the done channel.
	closed := make(chan error, 1)
	go func() { closed <- p.Close(context.Background()) }()

	select {
	case err := <-waiting:
		if !errors.Is(err, pool.ErrClosed) {
			t.Errorf("blocked Acquire returned %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Close left an Acquire blocked with no token to wake it")
	}

	held.Release(nil)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the lease was released")
	}
}

func TestCloseLogsOutAndClosesEveryConnection(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p, err := pool.New(pool.Options{Cap: 3, Dial: d.dial})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var leases []*pool.Lease
	for range 3 {
		l, err := p.Acquire(context.Background(), "")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		leases = append(leases, l)
	}
	for _, l := range leases {
		l.Release(nil)
	}

	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d.count() != 3 {
		t.Fatalf("dialled %d, want 3", d.count())
	}
	for i, c := range d.conns {
		closed, loggedOut := c.counts()
		if loggedOut != 1 {
			t.Errorf("connection %d logged out %d times, want 1", i, loggedOut)
		}
		if closed != 1 {
			t.Errorf("connection %d closed %d times, want 1", i, closed)
		}
	}

	if err := p.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if closed, _ := d.conns[0].counts(); closed != 1 {
		t.Errorf("a second Close closed the connection again")
	}
}

func TestAcquireAfterCloseFails(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p, err := pool.New(pool.Options{Cap: 2, Dial: d.dial})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := p.Acquire(context.Background(), ""); !errors.Is(err, pool.ErrClosed) {
		t.Fatalf("Acquire after Close = %v, want ErrClosed", err)
	}
}

// A deferred Release alongside an explicit one is the natural way to write this
// and must not corrupt the pool's accounting by returning capacity twice.
func TestReleaseIsIdempotent(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	p := newPool(t, 1, d)

	l, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	l.Release(nil)
	l.Release(nil)

	// If the second Release returned capacity, two concurrent Acquires would
	// both succeed on a pool with a cap of one.
	first, err := p.Acquire(context.Background(), "")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer first.Release(nil)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx, ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a double Release created capacity out of nothing: %v", err)
	}
}

func TestNewRejectsUnusableOptions(t *testing.T) {
	t.Parallel()

	d := &dialer{}
	if _, err := pool.New(pool.Options{Cap: 0, Dial: d.dial}); err == nil {
		t.Error("a cap of zero would block for ever on the first Acquire")
	}
	if _, err := pool.New(pool.Options{Cap: -1, Dial: d.dial}); err == nil {
		t.Error("want a negative cap rejected")
	}
	if _, err := pool.New(pool.Options{Cap: 1}); err == nil {
		t.Error("want a missing Dial rejected")
	}
}
