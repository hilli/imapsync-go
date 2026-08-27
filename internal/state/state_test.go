package state

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()

	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	return db
}

func TestEnsureFolderIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	first, err := db.EnsureFolder(ctx, "pair", "Sent Messages", "Sent")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	if first.ID == 0 {
		t.Error("folder was not assigned an id")
	}

	again, err := db.EnsureFolder(ctx, "pair", "Sent Messages", "Sent")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("second EnsureFolder created a new row: %d then %d", first.ID, again.ID)
	}

	// A changed mapping must update in place rather than orphan the history.
	remapped, err := db.EnsureFolder(ctx, "pair", "Sent Messages", "Sent Items")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	if remapped.ID != first.ID {
		t.Errorf("remapping created a new row: %d then %d", first.ID, remapped.ID)
	}
	if remapped.Dest != "Sent Items" {
		t.Errorf("Dest = %q, want the updated mapping", remapped.Dest)
	}
}

func TestFenceUIDValidity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("a first sync is not an invalidation", func(t *testing.T) {
		t.Parallel()

		db := openTestDB(t)
		f, err := db.EnsureFolder(ctx, "pair", "INBOX", "INBOX")
		if err != nil {
			t.Fatalf("EnsureFolder() error = %v", err)
		}

		kept, err := db.FenceUIDValidity(ctx, f.ID, 111, 222)
		if err != nil {
			t.Fatalf("FenceUIDValidity() error = %v", err)
		}
		if !kept {
			t.Error("a folder with no history was treated as invalidated")
		}
	})

	t.Run("unchanged validity keeps recorded messages", func(t *testing.T) {
		t.Parallel()

		db := openTestDB(t)
		f := seedFolder(t, db, 111, 222)
		seedDone(t, db, f.ID, 111, 7, 222, 70)

		kept, err := db.FenceUIDValidity(ctx, f.ID, 111, 222)
		if err != nil {
			t.Fatalf("FenceUIDValidity() error = %v", err)
		}
		if !kept {
			t.Fatal("unchanged UIDVALIDITY discarded the UID map")
		}
		if got := len(mustSyncedUIDs(t, db, f.ID, 111)); got != 1 {
			t.Errorf("got %d recorded UIDs, want the row preserved", got)
		}
	})

	t.Run("a changed source validity discards the UID map", func(t *testing.T) {
		t.Parallel()

		db := openTestDB(t)
		f := seedFolder(t, db, 111, 222)
		seedDone(t, db, f.ID, 111, 7, 222, 70)

		// The source renumbered the mailbox, so every recorded source UID now
		// refers to a different message, or to none.
		kept, err := db.FenceUIDValidity(ctx, f.ID, 999, 222)
		if err != nil {
			t.Fatalf("FenceUIDValidity() error = %v", err)
		}
		if kept {
			t.Fatal("a changed source UIDVALIDITY was not treated as invalidating")
		}
		if got := len(mustSyncedUIDs(t, db, f.ID, 111)); got != 0 {
			t.Errorf("got %d rows, want the stale UID map discarded", got)
		}
	})

	t.Run("a changed destination validity also discards the UID map", func(t *testing.T) {
		t.Parallel()

		db := openTestDB(t)
		f := seedFolder(t, db, 111, 222)
		seedDone(t, db, f.ID, 111, 7, 222, 70)

		// Recorded destination UIDs are just as meaningless after the
		// destination renumbers, even though the source is untouched.
		kept, err := db.FenceUIDValidity(ctx, f.ID, 111, 999)
		if err != nil {
			t.Fatalf("FenceUIDValidity() error = %v", err)
		}
		if kept {
			t.Fatal("a changed destination UIDVALIDITY was not treated as invalidating")
		}
		if got := len(mustSyncedUIDs(t, db, f.ID, 111)); got != 0 {
			t.Errorf("got %d rows, want the stale UID map discarded", got)
		}
	})
}

// TestBeginAppendIsVisibleImmediately is the point of the whole package. The
// in-flight row has to be durable before the APPEND goes out, so that a crash
// in the gap leaves evidence instead of a message that gets copied twice.
func TestBeginAppendIsVisibleImmediately(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	f, err := db.EnsureFolder(ctx, "pair", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	if err := db.BeginAppend(ctx, Message{
		FolderID: f.ID, SrcUIDValidity: 111, SrcUID: 7,
		IdentHash: "digest", Size: 1024, InternalDate: time.Unix(1700000000, 0),
	}); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}

	// Simulate the crash: drop the handle without any further writes, exactly
	// as if the process had died between BeginAppend and the APPEND response.
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	suspects, err := reopened.InFlight(ctx, f.ID)
	if err != nil {
		t.Fatalf("InFlight() error = %v", err)
	}
	if len(suspects) != 1 {
		t.Fatalf("got %d suspects after a crash, want 1", len(suspects))
	}
	if suspects[0].SrcUID != 7 {
		t.Errorf("SrcUID = %d, want 7", suspects[0].SrcUID)
	}
	if suspects[0].IdentHash != "digest" {
		t.Errorf("IdentHash = %q, want the digest needed to find the message again", suspects[0].IdentHash)
	}
	if suspects[0].Size != 1024 {
		t.Errorf("Size = %d, want 1024", suspects[0].Size)
	}
}

func TestCompleteAppendClearsSuspicion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)
	f := seedFolder(t, db, 111, 222)

	if err := db.BeginAppend(ctx, Message{FolderID: f.ID, SrcUIDValidity: 111, SrcUID: 7}); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}
	if err := db.CompleteAppend(ctx, f.ID, 111, 7, 222, 70); err != nil {
		t.Fatalf("CompleteAppend() error = %v", err)
	}

	suspects, err := db.InFlight(ctx, f.ID)
	if err != nil {
		t.Fatalf("InFlight() error = %v", err)
	}
	if len(suspects) != 0 {
		t.Errorf("got %d suspects after completion, want none", len(suspects))
	}
	if got := mustSyncedUIDs(t, db, f.ID, 111)[7]; got != StateDone {
		t.Errorf("state = %q, want %q", got, StateDone)
	}
}

func TestUpdatingAnUnrecordedMessageIsAnError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)
	f := seedFolder(t, db, 111, 222)

	// Completing without BeginAppend means the write-ahead step was skipped,
	// which is the bug this package exists to prevent. It must not pass quietly.
	err := db.CompleteAppend(ctx, f.ID, 111, 7, 222, 70)
	if !errors.Is(err, ErrNotRecorded) {
		t.Errorf("CompleteAppend() error = %v, want ErrNotRecorded", err)
	}
}

func TestFailAppendKeepsTheReason(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)
	f := seedFolder(t, db, 111, 222)

	if err := db.BeginAppend(ctx, Message{FolderID: f.ID, SrcUIDValidity: 111, SrcUID: 7}); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}
	if err := db.FailAppend(ctx, f.ID, 111, 7, "message too big"); err != nil {
		t.Fatalf("FailAppend() error = %v", err)
	}

	counts, err := db.Counts(ctx, f.ID)
	if err != nil {
		t.Fatalf("Counts() error = %v", err)
	}
	if counts[StateFailed] != 1 {
		t.Errorf("counts = %v, want one failed row", counts)
	}

	failures, err := db.Failures(ctx, f.ID)
	if err != nil {
		t.Fatalf("Failures() error = %v", err)
	}
	if len(failures) != 1 {
		t.Fatalf("got %d failures, want 1", len(failures))
	}
	if failures[0].LastError != "message too big" {
		t.Errorf("LastError = %q, want the reason preserved", failures[0].LastError)
	}
}

// TestRetryAfterFailureClearsTheError covers the second attempt at a message
// that failed once. last_error explains the state the row is in, so an in-flight
// retry must not still carry the reason for a failure it has moved past.
func TestRetryAfterFailureClearsTheError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)
	f := seedFolder(t, db, 111, 222)

	msg := Message{FolderID: f.ID, SrcUIDValidity: 111, SrcUID: 7}
	if err := db.BeginAppend(ctx, msg); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}
	if err := db.FailAppend(ctx, f.ID, 111, 7, "transient"); err != nil {
		t.Fatalf("FailAppend() error = %v", err)
	}
	if err := db.BeginAppend(ctx, msg); err != nil {
		t.Fatalf("BeginAppend() retry error = %v", err)
	}

	suspects, err := db.InFlight(ctx, f.ID)
	if err != nil {
		t.Fatalf("InFlight() error = %v", err)
	}
	if len(suspects) != 1 {
		t.Fatalf("got %d in-flight rows after a retry, want 1", len(suspects))
	}
	if suspects[0].LastError != "" {
		t.Errorf("LastError = %q, want the stale failure reason cleared", suspects[0].LastError)
	}

	if err := db.CompleteAppend(ctx, f.ID, 111, 7, 222, 70); err != nil {
		t.Fatalf("CompleteAppend() error = %v", err)
	}

	counts, err := db.Counts(ctx, f.ID)
	if err != nil {
		t.Fatalf("Counts() error = %v", err)
	}
	if counts[StateFailed] != 0 || counts[StateDone] != 1 {
		t.Errorf("counts = %v, want the retry to have replaced the failure", counts)
	}
}

// TestSyncedUIDsIgnoresOtherValidities guards the diff against reading rows from
// a previous incarnation of the mailbox.
func TestSyncedUIDsIgnoresOtherValidities(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	f := seedFolder(t, db, 111, 222)
	seedDone(t, db, f.ID, 111, 7, 222, 70)

	if got := len(mustSyncedUIDs(t, db, f.ID, 999)); got != 0 {
		t.Errorf("got %d rows for a different UIDVALIDITY, want none", got)
	}
}

func TestMarkSyncedRoundTripsModSeq(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)
	f := seedFolder(t, db, 111, 222)

	// A real iCloud HIGHESTMODSEQ, which is well past anything that fits in 32
	// bits and must survive the round trip intact.
	const modSeq = uint64(406622125881845)

	at := time.Unix(1700000000, 0).UTC()
	if err := db.MarkSynced(ctx, f.ID, modSeq, at); err != nil {
		t.Fatalf("MarkSynced() error = %v", err)
	}

	reloaded, err := db.EnsureFolder(ctx, "pair", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	if reloaded.SrcHighestModSeq != modSeq {
		t.Errorf("SrcHighestModSeq = %d, want %d", reloaded.SrcHighestModSeq, modSeq)
	}
	if !reloaded.LastSync.Equal(at) {
		t.Errorf("LastSync = %v, want %v", reloaded.LastSync, at)
	}
}

func seedFolder(t *testing.T, db *DB, srcUIDValidity, dstUIDValidity uint32) Folder {
	t.Helper()

	ctx := context.Background()
	f, err := db.EnsureFolder(ctx, "pair", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder() error = %v", err)
	}
	if _, err := db.FenceUIDValidity(ctx, f.ID, srcUIDValidity, dstUIDValidity); err != nil {
		t.Fatalf("FenceUIDValidity() error = %v", err)
	}
	return f
}

func seedDone(t *testing.T, db *DB, folderID int64, srcUIDValidity, srcUID, dstUIDValidity, dstUID uint32) {
	t.Helper()

	ctx := context.Background()
	if err := db.BeginAppend(ctx, Message{FolderID: folderID, SrcUIDValidity: srcUIDValidity, SrcUID: srcUID}); err != nil {
		t.Fatalf("BeginAppend() error = %v", err)
	}
	if err := db.CompleteAppend(ctx, folderID, srcUIDValidity, srcUID, dstUIDValidity, dstUID); err != nil {
		t.Fatalf("CompleteAppend() error = %v", err)
	}
}

func mustSyncedUIDs(t *testing.T, db *DB, folderID int64, srcUIDValidity uint32) map[uint32]State {
	t.Helper()

	got, err := db.SyncedUIDs(context.Background(), folderID, srcUIDValidity)
	if err != nil {
		t.Fatalf("SyncedUIDs() error = %v", err)
	}
	return got
}

// The concurrent engine writes twice per message, from as many goroutines as
// there are destination connections. SQLite serialises writers, so the question
// is whether that serialisation surfaces as SQLITE_BUSY errors under a load the
// engine will really produce. It is cheaper to find out here than to find out
// halfway through a 414,022-message folder.
func TestConcurrentWritersDoNotContend(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	folder, err := db.EnsureFolder(ctx, "pair", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}

	const (
		workers         = 32
		perWorker       = 40
		srcUIDValidity  = uint32(1)
		destUIDValidity = uint32(2)
	)

	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker*2)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				uid := uint32(w*perWorker + i + 1)
				m := Message{
					FolderID:       folder.ID,
					SrcUIDValidity: srcUIDValidity,
					SrcUID:         uid,
					IdentHash:      fmt.Sprintf("hash-%d", uid),
					Size:           int64(uid) * 10,
					InternalDate:   time.Unix(int64(uid), 0).UTC(),
				}
				if err := db.BeginAppend(ctx, m); err != nil {
					errs <- fmt.Errorf("BeginAppend(%d): %w", uid, err)
					continue
				}
				if err := db.CompleteAppend(ctx, folder.ID, srcUIDValidity, uid, destUIDValidity, uid+1000); err != nil {
					errs <- fmt.Errorf("CompleteAppend(%d): %w", uid, err)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	synced, err := db.SyncedUIDs(ctx, folder.ID, srcUIDValidity)
	if err != nil {
		t.Fatalf("SyncedUIDs: %v", err)
	}
	if len(synced) != workers*perWorker {
		t.Fatalf("recorded %d messages, want %d", len(synced), workers*perWorker)
	}
	for uid, st := range synced {
		if st != StateDone {
			t.Fatalf("uid %d is %q, want done", uid, st)
		}
	}
}

// Readers must not be blocked out by a steady stream of writes, or a folder
// diff on one folder would stall every worker on every other folder. WAL exists
// precisely so this is true, but the DSN has to actually be asking for it.
func TestReadsProceedDuringWrites(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openTestDB(t)

	folder, err := db.EnsureFolder(ctx, "pair", "INBOX", "INBOX")
	if err != nil {
		t.Fatalf("EnsureFolder: %v", err)
	}

	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var uid uint32
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			uid++
			if err := db.BeginAppend(ctx, Message{
				FolderID: folder.ID, SrcUIDValidity: 1, SrcUID: uid,
				IdentHash: fmt.Sprintf("h%d", uid), InternalDate: time.Unix(1, 0).UTC(),
			}); err != nil {
				done <- err
				return
			}
		}
	}()

	deadline := time.Now().Add(500 * time.Millisecond)
	reads := 0
	for time.Now().Before(deadline) {
		if _, err := db.SyncedUIDs(ctx, folder.ID, 1); err != nil {
			close(stop)
			t.Fatalf("read %d during writes: %v", reads, err)
		}
		reads++
	}
	close(stop)
	if err := <-done; err != nil {
		t.Fatalf("writer: %v", err)
	}
	if reads < 10 {
		t.Errorf("only %d reads completed in 500ms of concurrent writing", reads)
	}
}
