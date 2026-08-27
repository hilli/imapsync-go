package syncer_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/budget"
	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// These tests are about the engine being concurrent. The properties in
// syncer_test.go — nothing duplicated, nothing lost, a run resumable after a
// crash — are the ones that matter, and they run over pools too; what is left
// to establish is that the concurrency is real, that it is bounded, and that it
// cannot wedge.

// fill puts n distinct messages in a mailbox and returns their subjects.
func fill(t *testing.T, a account, mailbox string, n int) []string {
	t.Helper()

	subjects := make([]string, 0, n)
	for i := range n {
		subject := fmt.Sprintf("%s-%04d", mailbox, i)
		a.stuff(t, mailbox, testMessage(subject, fmt.Sprintf("s%d@example.test", i)))
		subjects = append(subjects, subject)
	}
	return subjects
}

// syncWith runs a sync over pools of the given sizes.
func syncWith(t *testing.T, h *harness, srcCap, dstCap int, bytes *budget.Budget, wrapSrc, wrapDst func(imapx.Conn) imapx.Conn) syncer.Report {
	t.Helper()

	s := syncer.New(
		pooled(t, srcCap, readOnly, h.src.dialFunc(t, wrapSrc)),
		pooled(t, dstCap, imapx.SelectOptions{}, h.dst.dialFunc(t, wrapDst)),
		h.db, bytes, syncer.Options{PairID: "test"},
	)
	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return report
}

// assertExactly checks a destination mailbox holds each wanted subject once.
//
// Counting is not enough. The failure this guards against is two workers
// copying the same message while a third copies none, which leaves the total
// right and the mailbox wrong.
func assertExactly(t *testing.T, a account, mailbox string, want []string) {
	t.Helper()

	seen := make(map[string]int, len(want))
	for _, body := range a.contents(t, mailbox) {
		for _, line := range strings.Split(body, "\r\n") {
			if subject, ok := strings.CutPrefix(line, "Subject: "); ok {
				seen[subject]++
				break
			}
		}
	}

	var missing, duplicated int
	for _, subject := range want {
		switch n := seen[subject]; {
		case n == 0:
			missing++
		case n > 1:
			duplicated++
		}
		delete(seen, subject)
	}
	if missing > 0 || duplicated > 0 || len(seen) > 0 {
		t.Errorf("%s: %d missing, %d duplicated, %d unexpected (want %d messages)",
			mailbox, missing, duplicated, len(seen), len(want))
	}
}

// TestALargeFolderIsSplitAcrossConnections is the case the whole milestone
// exists for. iCloud's INBOX holds 53% of that account (§6.3), so a folder that
// only one connection can work on is a folder that sets the runtime of the
// entire migration.
func TestALargeFolderIsSplitAcrossConnections(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 240)

	report := syncWith(t, h, 4, 6, nil, nil, nil)

	if copied, _, failed := report.Totals(); copied != len(want) || failed != 0 {
		t.Fatalf("copied %d, failed %d; want %d copied and none failed", copied, failed, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// TestConcurrentFoldersDoNotMixMessagesUp guards the one mistake that
// destination pooling makes possible: an APPEND names its own mailbox, and a
// connection shared between folders will happily file a message in the wrong
// one if the name is taken from anywhere but the message's own pair.
func TestConcurrentFoldersDoNotMixMessagesUp(t *testing.T) {
	t.Parallel()

	names := []string{"Work", "Personal", "Archive", "Receipts", "Travel", "Lists"}
	h := newHarness(t, rev1Caps(), names...)

	want := map[string][]string{"INBOX": fill(t, h.src, "INBOX", 60)}
	for _, name := range names {
		want[name] = fill(t, h.src, name, 60)
	}

	report := syncWith(t, h, 4, 6, nil, nil, nil)
	if _, _, failed := report.Totals(); failed != 0 {
		t.Fatalf("%d messages failed", failed)
	}
	for mailbox, subjects := range want {
		assertExactly(t, h.dst, mailbox, subjects)
	}
}

// TestOneConnectionEachSideStillCompletes is the deadlock test.
//
// Fetch workers hold a source connection, append workers hold a destination
// one, and the setup stage holds both. If anything ever took a source
// connection while holding a destination one, the two pools would wait on each
// other; at a cap of one each, that wait is immediate and certain. Passing here
// is what makes the pools safe to size independently.
func TestOneConnectionEachSideStillCompletes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Work")
	inbox := fill(t, h.src, "INBOX", 120)
	work := fill(t, h.src, "Work", 120)

	done := make(chan syncer.Report, 1)
	go func() { done <- syncWith(t, h, 1, 1, nil, nil, nil) }()

	select {
	case report := <-done:
		if _, _, failed := report.Totals(); failed != 0 {
			t.Fatalf("%d messages failed", failed)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("a sync over one connection per side did not finish: the pools are deadlocked")
	}

	assertExactly(t, h.dst, "INBOX", inbox)
	assertExactly(t, h.dst, "Work", work)
}

// TestABudgetSmallerThanAMessageStillFinishes checks the other way the run
// could wedge. A message charged what it asks for, when it asks for more than
// the budget holds, can never be granted: no other worker can free bytes that
// were never lent. The budget clamps instead, and this is what depends on it.
func TestABudgetSmallerThanAMessageStillFinishes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 80)

	tiny, err := budget.New(1)
	if err != nil {
		t.Fatalf("budget.New() error = %v", err)
	}

	done := make(chan syncer.Report, 1)
	go func() { done <- syncWith(t, h, 3, 5, tiny, nil, nil) }()

	select {
	case report := <-done:
		if _, _, failed := report.Totals(); failed != 0 {
			t.Fatalf("%d messages failed", failed)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("a one-byte budget stalled the run")
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// slowSource delays every body fetch, standing in for a server on the far end
// of a network.
type slowSource struct {
	imapx.Conn
	delay time.Duration
	meet  *rendezvous
}

func (s slowSource) FetchBody(ctx context.Context, uid uint32, w io.Writer) (int64, error) {
	if s.meet != nil {
		if err := s.meet.arrive(); err != nil {
			return 0, err
		}
	}
	time.Sleep(s.delay)
	return s.Conn.FetchBody(ctx, uid, w)
}

// rendezvous holds the first want callers until all of them have arrived, then
// lets every caller through from then on.
//
// It turns "these operations overlapped" into something a test can state
// without consulting a clock: if want of them are ever in flight at once the
// rendezvous completes, and if they are not it times out. The answer does not
// change when the machine is busy, only how long it takes to arrive.
type rendezvous struct {
	mu      sync.Mutex
	want    int
	arrived int
	open    chan struct{}
}

func newRendezvous(want int) *rendezvous {
	return &rendezvous{want: want, open: make(chan struct{})}
}

func (r *rendezvous) arrive() error {
	r.mu.Lock()
	r.arrived++
	if r.arrived == r.want {
		close(r.open)
	}
	got, open := r.arrived, r.open
	r.mu.Unlock()

	select {
	case <-open:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("waited alongside only %d other fetches, wanted %d", got-1, r.want-1)
	}
}

// TestFetchesInOneFolderActuallyOverlap distinguishes an engine that is
// concurrent from one that merely has the machinery for it.
//
// Every other test here would pass on a strictly sequential engine. This one
// makes each body fetch wait until four are in flight at once inside a single
// folder, which is the thing the milestone is for and which nothing but real
// overlap can produce. A sequential engine parks on the first fetch and the
// folder fails.
//
// The rendezvous replaced a wall-clock assertion — that the folder finished in
// under half the time one connection would need — which passed when run alone
// and failed inside the full suite. A timing test measures the machine as much
// as the code. This one holds whether the machine is idle or thrashing, and the
// four workers cannot slip past each other however the scheduler runs them:
// each is parked on its first message and so cannot take a second chunk.
func TestFetchesInOneFolderActuallyOverlap(t *testing.T) {
	t.Parallel()

	const (
		workers = 4
		// Four chunks at the engine's chunk size of 50, so every worker has
		// one and none of them can find itself with nothing to fetch.
		messages = 200
	)

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", messages)

	meet := newRendezvous(workers)
	report := syncWith(t, h, workers, 6, nil, func(c imapx.Conn) imapx.Conn {
		return slowSource{Conn: c, meet: meet}
	}, nil)

	for _, fr := range report.Folders {
		if fr.Err != nil {
			t.Fatalf("folder %s: %v", fr.Source, fr.Err)
		}
	}
	if copied, _, _ := report.Totals(); copied != messages {
		t.Fatalf("copied %d, want %d", copied, messages)
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// meetingDest holds every append at a rendezvous.
type meetingDest struct {
	imapx.Conn
	meet *rendezvous
}

func (d meetingDest) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	if err := d.meet.arrive(); err != nil {
		return imapx.AppendResult{}, err
	}
	return d.Conn.Append(ctx, mailbox, msg)
}

// TestAppendsToOneFolderActuallyOverlap is the destination half of the test
// above, and it is the half that matters more.
//
// Appending is the expensive side of a sync: the server has to accept, store
// and index a whole message, where the source only has to read one back. That
// is why the destination pool is the larger of the two by default. An engine
// that fetched four at a time and then appended them one after another would
// have moved the bottleneck without lifting it, and every other test here would
// still have passed.
func TestAppendsToOneFolderActuallyOverlap(t *testing.T) {
	t.Parallel()

	const (
		workers  = 4
		messages = 200
	)

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", messages)

	meet := newRendezvous(workers)
	report := syncWith(t, h, 4, workers, nil, nil, func(c imapx.Conn) imapx.Conn {
		return meetingDest{Conn: c, meet: meet}
	})

	for _, fr := range report.Folders {
		if fr.Err != nil {
			t.Fatalf("folder %s: %v", fr.Source, fr.Err)
		}
	}
	if copied, _, _ := report.Totals(); copied != messages {
		t.Fatalf("copied %d, want %d", copied, messages)
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// renumberingSource reports a different UIDVALIDITY on every connection after
// the first, which is what a mailbox recreated mid-run looks like to a pool.
type renumberingSource struct {
	imapx.Conn
	first bool
}

func (r renumberingSource) Select(ctx context.Context, mailbox string, opts imapx.SelectOptions) (imapx.Mailbox, error) {
	box, err := r.Conn.Select(ctx, mailbox, opts)
	if err == nil && !r.first {
		box.UIDValidity++
	}
	return box, err
}

// TestARenumberedSourceFolderIsAbandonedNotMiscopied covers a hazard that only
// exists because of pooling.
//
// One long-lived connection can never see a mailbox renumbered, because it
// never selects it again. A pool selects on every lease, so it can — and every
// UID it is working from then names a different message, or none. Filing those
// bodies under the UIDs that were planned would record copies that never
// happened and skip messages that were never copied, permanently, because the
// state database would look complete.
func TestARenumberedSourceFolderIsAbandonedNotMiscopied(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 200)

	var conns atomic.Int32
	s := syncer.New(
		pooled(t, 3, readOnly, h.src.dialFunc(t, func(c imapx.Conn) imapx.Conn {
			return renumberingSource{Conn: c, first: conns.Add(1) == 1}
		})),
		pooled(t, 5, imapx.SelectOptions{}, h.dst.dialFunc(t, nil)),
		h.db, nil, syncer.Options{PairID: "test"},
	)
	report, err := s.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(report.Folders) != 1 {
		t.Fatalf("got %d folders, want 1", len(report.Folders))
	}
	fr := report.Folders[0]
	if fr.Err == nil {
		t.Fatal("the folder was renumbered mid-run and reported success")
	}
	if !strings.Contains(fr.Err.Error(), "renumbered") {
		t.Errorf("error does not say what happened: %v", fr.Err)
	}

	// Whatever the first connection managed before the others noticed is
	// allowed to have landed. What is not allowed is a second copy of it.
	seen := make(map[string]int)
	for _, body := range h.dst.contents(t, "INBOX") {
		for _, line := range strings.Split(body, "\r\n") {
			if subject, ok := strings.CutPrefix(line, "Subject: "); ok {
				seen[subject]++
				break
			}
		}
	}
	for subject, n := range seen {
		if n > 1 {
			t.Errorf("%s was copied %d times", subject, n)
		}
	}
}

// TestALargeFolderIsAdoptedConcurrently exercises the adoption index under the
// only conditions that can break it.
//
// The index is one map shared by every fetch worker in a folder, and adopting
// consumes an entry: two workers that read it at the same instant could both
// claim the same destination message, which would copy one source message
// twice and leave another with nothing. A small folder cannot show this,
// because one worker does all of it.
func TestALargeFolderIsAdoptedConcurrently(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 240)

	if copied, _, _ := syncWith(t, h, 4, 6, nil, nil, nil).Totals(); copied != len(want) {
		t.Fatalf("first run copied %d, want %d", copied, len(want))
	}

	// The destination is intact but the record of it is gone, which is what a
	// lost database looks like and what a first sync onto an account someone
	// has already half-migrated looks like too.
	h.dbPath = filepath.Join(t.TempDir(), "fresh.db")
	h.db = h.openDB(t)

	report := syncWith(t, h, 4, 6, nil, nil, nil)
	copied, adopted, failed := report.Totals()
	if copied != 0 || adopted != len(want) || failed != 0 {
		t.Errorf("second run copied %d, adopted %d, failed %d; want 0, %d, 0",
			copied, adopted, failed, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// TestCancellationStopsTheRunPromptly matters more than a normal cancellation
// test would.
//
// A sequential engine that ignores cancellation finishes late. This one has a
// feeder, two sets of workers, two connection pools and a byte semaphore, any
// of which can be blocked on any of the others; a cancellation that does not
// reach all of them does not run late, it never returns at all. Ctrl-C on a
// five-day migration has to work.
func TestCancellationStopsTheRunPromptly(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Work")
	fill(t, h.src, "INBOX", 200)
	fill(t, h.src, "Work", 200)

	tiny, err := budget.New(4096)
	if err != nil {
		t.Fatalf("budget.New() error = %v", err)
	}
	s := syncer.New(
		pooled(t, 2, readOnly, h.src.dialFunc(t, func(c imapx.Conn) imapx.Conn {
			return slowSource{Conn: c, delay: 20 * time.Millisecond}
		})),
		pooled(t, 3, imapx.SelectOptions{}, h.dst.dialFunc(t, nil)),
		h.db, tiny, syncer.Options{PairID: "test"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := s.Run(ctx)
		done <- err
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run() after cancellation returned %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run() did not return after its context was cancelled")
	}
}

// brokenDest appends slowly and then starts refusing, the way a server does
// when the credentials it accepted an hour ago are withdrawn.
//
// The refusal has to be one that no retry can fix, because that is what makes a
// folder give up rather than record a failed message and carry on. A dropped
// connection no longer qualifies: it is retried on a fresh one.
//
// The slowness is what makes the failure interesting. Fetch workers run ahead
// of append workers, so by the time the refusal comes the hand-off channel is
// full of messages that have been read and charged against the byte budget and
// will now never be appended.
type brokenDest struct {
	imapx.Conn
	mailbox string
	after   *atomic.Int32
	delay   time.Duration
}

func (b brokenDest) Append(ctx context.Context, mailbox string, msg imapx.AppendMessage) (imapx.AppendResult, error) {
	if !strings.HasPrefix(mailbox, b.mailbox) {
		return b.Conn.Append(ctx, mailbox, msg)
	}
	time.Sleep(b.delay)
	if b.after.Add(-1) > 0 {
		return b.Conn.Append(ctx, mailbox, msg)
	}
	return imapx.AppendResult{}, &imap.Error{
		Type: imap.StatusResponseTypeNo,
		Code: imap.ResponseCodeAuthenticationFailed,
		Text: "credentials withdrawn",
	}
}

// TestFailedFoldersDoNotStarveTheRunOfItsBudget is about what a folder takes
// with it when it dies.
//
// A message is charged against the byte budget when it is read and refunded
// once it has been appended. An append worker that stops early leaves whatever
// was already fetched sitting in the hand-off channel, still charged. Those
// bytes never come back on their own, the budget is shared by the whole run,
// and the loss is permanent — so it accumulates. Enough failed folders and
// there is nothing left to lend, at which point the run does not fail, it
// hangs.
//
// One failed folder cannot show this, because one folder cannot lose more than
// the hand-off channel holds. Several in a row can lose all of it.
func TestFailedFoldersDoNotStarveTheRunOfItsBudget(t *testing.T) {
	t.Parallel()

	doomed := []string{"Doom1", "Doom2", "Doom3", "Doom4", "Doom5", "Doom6"}
	h := newHarness(t, rev1Caps(), append(doomed, "Zed")...)
	for _, name := range doomed {
		fill(t, h.src, name, 40)
	}
	want := fill(t, h.src, "Zed", 20)

	// Room for a handful of messages at a time. Small enough that six folders
	// losing a couple each exhausts it, large enough that nothing is starved
	// while the budget is intact.
	size := int64(len(testMessage("Doom1-0000", "s0@example.test")))
	small, err := budget.New(6 * size)
	if err != nil {
		t.Fatalf("budget.New() error = %v", err)
	}

	// Two append workers, and a break that waits until they have taken a few
	// messages, so the channel behind them is full when it comes.
	var s *syncer.Syncer
	{
		remaining := &atomic.Int32{}
		remaining.Store(3)
		s = syncer.New(
			pooled(t, 2, readOnly, h.src.dialFunc(t, nil)),
			pooled(t, 2, imapx.SelectOptions{}, h.dst.dialFunc(t, func(c imapx.Conn) imapx.Conn {
				return brokenDest{Conn: c, mailbox: "Doom", after: remaining, delay: 25 * time.Millisecond}
			})),
			h.db, small, syncer.Options{PairID: "test"},
		)
	}

	done := make(chan syncer.Report, 1)
	go func() {
		report, err := s.Run(context.Background())
		if err != nil {
			t.Errorf("Run() error = %v", err)
		}
		done <- report
	}()

	var report syncer.Report
	select {
	case report = <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("the run hung: failed folders kept the byte budget they had been lent")
	}

	byName := make(map[string]syncer.FolderReport, len(report.Folders))
	for _, fr := range report.Folders {
		byName[fr.Source] = fr
	}
	for _, name := range doomed {
		if byName[name].Err == nil {
			t.Errorf("%s was expected to fail", name)
		}
	}
	if got := byName["Zed"]; got.Err != nil {
		t.Errorf("Zed failed after the others did: %v", got.Err)
	}
	assertExactly(t, h.dst, "Zed", want)

	// The decisive check. Six folders failed part-way through, and every byte
	// they had been lent must be lendable again — asking for the whole budget
	// at once succeeds only if none of it is still charged to a message that
	// nobody is going to append.
	//
	// Asserting this rather than waiting for a hang is what makes the test
	// worth having. The leak is bounded by the hand-off channel, so a single
	// run has to lose a great deal before it stalls, and a test built around
	// stalling would be slow, flaky, and would pass for the wrong reason as
	// soon as anyone changed a buffer size.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := small.Acquire(ctx, 6*size)
	if err != nil {
		t.Fatalf("the byte budget did not come back after the failed folders: %v", err)
	}
	release()
}
