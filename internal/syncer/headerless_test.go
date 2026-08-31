package syncer_test

import (
	"context"
	"slices"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// muteServer answers with the size of a message but none of its header.
//
// Not hypothetical. mox stores a message whose header holds a line longer than
// its parser allows with the body starting at offset 0, so BODY[HEADER] comes
// back as a zero-length literal while BODY[TEXT] returns the header as well. An
// unfolded References line on a long enough Apple Mail thread is enough to do
// it, and 19 messages in an 11,770-message Sent mailbox were.
type muteServer struct {
	imapx.Conn
	// header replaces what the server would have returned. Empty is the defect
	// being modelled; a short value stands for mail that is merely thin.
	header []byte
	// zeroSize also blanks the size, which is a message that is not there
	// rather than a server refusing to describe one.
	zeroSize bool
}

func (m muteServer) FetchMeta(ctx context.Context, uids []uint32, fields []string) ([]imapx.MessageMeta, error) {
	metas, err := m.Conn.FetchMeta(ctx, uids, fields)
	if err != nil {
		return nil, err
	}
	for i := range metas {
		metas[i].Header = slices.Clone(m.header)
		if m.zeroSize {
			metas[i].Size = 0
		}
	}
	return metas, nil
}

func muted(header []byte, zeroSize bool) func(imapx.Conn) imapx.Conn {
	return func(c imapx.Conn) imapx.Conn {
		return muteServer{Conn: c, header: header, zeroSize: zeroSize}
	}
}

// TestASourceThatWillNotReturnHeadersIsReported.
//
// The run succeeds, which is the problem: the mail arrives, no folder fails and
// the exit code is zero, so without a count nothing distinguishes this from a
// healthy run. It took a byte-for-byte comparison of a backup against a restore
// of that backup to notice it the first time.
func TestASourceThatWillNotReturnHeadersIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 9)

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, muted(nil, false), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	fr := folderReport(t, rep, "INBOX")
	if fr.SourceHeaderless != len(want) {
		t.Errorf("reported %d headerless on the source, want %d", fr.SourceHeaderless, len(want))
	}
	source, dest := rep.Headerless()
	if source != len(want) || dest != 0 {
		t.Errorf("Headerless() = %d source, %d dest; want %d and 0", source, dest, len(want))
	}
	// The mail still arrives. This is a warning about identification, not a
	// failure to copy, and reporting it as a failure would be worse than silence.
	if fr.Failed != 0 {
		t.Errorf("reported %d failed, want 0: the bodies copy regardless", fr.Failed)
	}
	if fr.Copied != len(want) {
		t.Errorf("copied %d of %d", fr.Copied, len(want))
	}
}

// TestADestinationThatWillNotReturnHeadersIsReported.
//
// The destination side is the one that costs mail twice over: a message that
// cannot be identified cannot be adopted either, so the source copies it again
// and the account grows a duplicate. That the copies happen is asserted here,
// because it is the reason the count is worth printing.
func TestADestinationThatWillNotReturnHeadersIsReported(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 12)
	fill(t, h.dst, "INBOX", 12)

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, muted(nil, false))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	fr := folderReport(t, rep, "INBOX")
	if fr.DestHeaderless != len(want) {
		t.Errorf("reported %d headerless on the destination, want %d", fr.DestHeaderless, len(want))
	}
	if fr.Adopted != 0 {
		t.Fatalf("adopted %d: the premise is that none could be identified", fr.Adopted)
	}
	if fr.Copied != len(want) {
		t.Errorf("copied %d of %d: an unidentifiable destination message is copied again", fr.Copied, len(want))
	}
}

// TestThinHeadersAreNotReportedAsMissingOnes.
//
// A message carrying only a Subject is thin but honest, and thin mail is
// ordinary — it makes a weak identity, which the syncer already declines to act
// on. Counting those as a server defect would put a number on every run against
// a mailbox holding a few sparse messages, and a warning that is always there is
// one nobody reads.
func TestThinHeadersAreNotReportedAsMissingOnes(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 6)

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, muted([]byte("Subject: thin\r\n\r\n"), false), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if source, dest := rep.Headerless(); source != 0 || dest != 0 {
		t.Errorf("Headerless() = %d source, %d dest; want 0 and 0: a thin header is still a header", source, dest)
	}
}

// TestAMessageWithNoBytesIsNotAServerDefect.
//
// No header and no size is a message that is not there, which the run already
// counts as vanished. Reading it as a server that would not answer would blame
// the wrong thing, and would do so on exactly the servers that list UIDs they
// have no message for.
func TestAMessageWithNoBytesIsNotAServerDefect(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 5)

	rep, err := syncFlaky(t, h, 1, 1, syncer.Options{}, muted(nil, true), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if source, _ := rep.Headerless(); source != 0 {
		t.Errorf("reported %d headerless, want 0: nothing there is not a refusal to answer", source)
	}
}
