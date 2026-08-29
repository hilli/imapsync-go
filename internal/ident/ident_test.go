package ident

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func header(lines ...string) []byte {
	return []byte(strings.Join(lines, "\r\n") + "\r\n\r\n")
}

var sample = header(
	"Message-ID: <abc123@example.test>",
	"Date: Mon, 27 Aug 2026 12:00:00 +0000",
	"Subject: Quarterly report",
	"From: Alice <alice@example.test>",
	"To: Bob <bob@example.test>",
)

// TestRefoldingDoesNotChangeTheDigest is the property that makes adoption work.
// Servers fold long headers at their own chosen column, so the same message
// arrives differently on the two sides. If that changed the digest, every such
// message would look uncopied and be duplicated on the next run.
//
// The cases below the first are the ones textproto does not handle for us: it
// normalises folding itself, but leaves interior whitespace runs and tabs
// exactly as they were.
func TestRefoldingDoesNotChangeTheDigest(t *testing.T) {
	t.Parallel()

	oneLine := header(
		"Message-ID: <abc123@example.test>",
		"Subject: A subject long enough that a server might well fold it somewhere",
	)

	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"folded with a tab", []string{
			"Message-ID: <abc123@example.test>",
			"Subject: A subject long enough that a server",
			"\tmight well fold it somewhere",
		}},
		{"folded with spaces", []string{
			"Message-ID: <abc123@example.test>",
			"Subject: A subject long enough that a server",
			"    might well fold it somewhere",
		}},
		{"interior whitespace run", []string{
			"Message-ID: <abc123@example.test>",
			"Subject: A subject long  enough that a server might well fold it somewhere",
		}},
		{"tab inside the value", []string{
			"Message-ID: <abc123@example.test>",
			"Subject: A subject long\tenough that a server might well fold it somewhere",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if a, b := Parse(oneLine).Digest, Parse(header(tc.lines...)).Digest; a != b {
				t.Errorf("%s changed the digest:\n %s\n %s", tc.name, a, b)
			}
		})
	}
}

func TestDigestIsStableAndDistinguishing(t *testing.T) {
	t.Parallel()

	base := Parse(sample)
	if base.Digest != Parse(sample).Digest {
		t.Error("the digest is not deterministic")
	}

	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"different Message-ID", []string{
			"Message-ID: <different@example.test>",
			"Date: Mon, 27 Aug 2026 12:00:00 +0000",
			"Subject: Quarterly report",
			"From: Alice <alice@example.test>",
			"To: Bob <bob@example.test>",
		}},
		{"different Subject", []string{
			"Message-ID: <abc123@example.test>",
			"Date: Mon, 27 Aug 2026 12:00:00 +0000",
			"Subject: Annual report",
			"From: Alice <alice@example.test>",
			"To: Bob <bob@example.test>",
		}},
		{"different Date", []string{
			"Message-ID: <abc123@example.test>",
			"Date: Tue, 28 Aug 2026 12:00:00 +0000",
			"Subject: Quarterly report",
			"From: Alice <alice@example.test>",
			"To: Bob <bob@example.test>",
		}},
		{"missing To", []string{
			"Message-ID: <abc123@example.test>",
			"Date: Mon, 27 Aug 2026 12:00:00 +0000",
			"Subject: Quarterly report",
			"From: Alice <alice@example.test>",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Parse(header(tc.lines...)).Digest; got == base.Digest {
				t.Errorf("%s produced the same digest as the original", tc.name)
			}
		})
	}
}

// TestAValueMovingBetweenFieldsChangesTheDigest guards against a digest built
// by concatenating values, where From and To swapping would go unnoticed.
func TestAValueMovingBetweenFieldsChangesTheDigest(t *testing.T) {
	t.Parallel()

	a := header("From: alice@example.test", "To: bob@example.test")
	b := header("From: bob@example.test", "To: alice@example.test")

	if Parse(a).Digest == Parse(b).Digest {
		t.Error("swapping From and To left the digest unchanged")
	}
}

// TestAdjacentFieldsCannotImitateEachOther covers the separator choice: without
// one, a Subject ending in the next field's value would collide.
func TestAdjacentFieldsCannotImitateEachOther(t *testing.T) {
	t.Parallel()

	a := header("Subject: onetwo", "From: ")
	b := header("Subject: one", "From: two")

	if Parse(a).Digest == Parse(b).Digest {
		t.Error("field boundaries are not represented in the digest")
	}
}

// TestRepeatedFieldsCannotImitateOneValue covers the second separator. A header
// may legally appear more than once, and two Cc lines must not digest the same
// as one Cc holding both values concatenated.
func TestRepeatedFieldsCannotImitateOneValue(t *testing.T) {
	t.Parallel()

	a := header("Cc: onetwo")
	b := header("Cc: one", "Cc: two")

	if Parse(a).Digest == Parse(b).Digest {
		t.Error("repeated fields digest the same as their concatenation")
	}
}

func TestMessageIDIsExtracted(t *testing.T) {
	t.Parallel()

	id := Parse(sample)
	if id.MessageID != "<abc123@example.test>" {
		t.Errorf("MessageID = %q, want the angle-bracketed id", id.MessageID)
	}
	if id.NeedsStamp() {
		t.Error("NeedsStamp is true for a message that has a Message-ID")
	}
	if id.Weak {
		t.Error("Weak is true for a fully populated header")
	}
}

// TestAMessageWithoutAMessageIDNeedsStamping is the tier-4 trigger. Without a
// Message-ID there is nothing to search the destination for, so the copy has to
// carry a marker of our own or a crash will duplicate it.
func TestAMessageWithoutAMessageIDNeedsStamping(t *testing.T) {
	t.Parallel()

	id := Parse(header(
		"Date: Mon, 27 Aug 2026 12:00:00 +0000",
		"Subject: No identifier here",
		"From: alice@example.test",
	))
	if !id.NeedsStamp() {
		t.Error("NeedsStamp is false for a message with no Message-ID")
	}
	if id.Weak {
		t.Error("Weak is true even though several fields survived")
	}

	field, value, ok := SearchTerms(id, true)
	if !ok || field != StampHeader || value != id.Digest {
		t.Errorf("SearchTerms(stamped) = %q, %q, %v; want the stamp header and digest", field, value, ok)
	}
}

// TestAnEmptyHeaderIsWeak stops adoption from acting on an identity that cannot
// tell two messages apart. Matching the wrong message drops mail silently,
// which is worse than copying it twice.
func TestAnEmptyHeaderIsWeak(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		header []byte
	}{
		{"nothing at all", []byte{}},
		{"only a subject", header("Subject: Hello")},
		{"only whitespace values", header("Subject:   ", "From:  ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			id := Parse(tc.header)
			if !id.Weak {
				t.Errorf("Weak is false for %s; adoption would match on almost nothing", tc.name)
			}
			if _, _, ok := SearchTerms(id, false); ok {
				t.Error("SearchTerms offered a search for an unsearchable message")
			}
		})
	}
}

// TestMalformedHeadersStillIdentify covers real mailboxes, which contain
// garbage. Refusing to identify a message would leave it uncopyable forever.
//
// Note what is being asserted: textproto stops at the malformed line and
// discards everything after it, so only Message-ID survives here. That loss is
// accepted — a partial identity still copies the message, and the UID map is
// the authority in any case.
func TestMalformedHeadersStillIdentify(t *testing.T) {
	t.Parallel()

	broken := []byte("Message-ID: <ok@example.test>\r\nthis line has no colon\r\nSubject: Still here\r\n\r\n")

	id := Parse(broken)
	if id.Digest == "" {
		t.Fatal("a malformed header produced no digest at all")
	}
	if id.MessageID != "<ok@example.test>" {
		t.Errorf("MessageID = %q, want the id that parsed cleanly", id.MessageID)
	}
	if id.Weak {
		t.Error("Weak is true despite a usable Message-ID")
	}
}

// TestAHeaderWithoutATerminatorIsStillParsed covers servers that return the
// requested fields without the closing blank line. Every field must survive,
// not just the ones before the last.
func TestAHeaderWithoutATerminatorIsStillParsed(t *testing.T) {
	t.Parallel()

	unterminated := []byte("Message-ID: <trunc@example.test>\r\nSubject: Cut short\r\n")

	id := Parse(unterminated)
	if id.MessageID != "<trunc@example.test>" {
		t.Errorf("MessageID = %q, want it parsed despite the missing terminator", id.MessageID)
	}
	if id.Weak {
		t.Error("Weak is true for a header that parsed fine")
	}

	// The trailing field is the one at risk, so prove it reached the digest.
	if id.Digest == Parse([]byte("Message-ID: <trunc@example.test>\r\n")).Digest {
		t.Error("the last field before the truncation was dropped")
	}
}

func TestSearchTermsPrefersTheStamp(t *testing.T) {
	t.Parallel()

	id := Parse(sample)

	field, value, ok := SearchTerms(id, false)
	if !ok || field != "Message-ID" || value != "<abc123@example.test>" {
		t.Errorf("SearchTerms(unstamped) = %q, %q, %v; want a Message-ID search", field, value, ok)
	}

	// A stamped copy is found by its stamp even when it also has a Message-ID:
	// the stamp is ours and cannot have been rewritten in transit.
	field, value, ok = SearchTerms(id, true)
	if !ok || field != StampHeader || value != id.Digest {
		t.Errorf("SearchTerms(stamped) = %q, %q, %v; want a stamp search", field, value, ok)
	}
}

func TestStampBytesIsAWellFormedHeaderLine(t *testing.T) {
	t.Parallel()

	got := string(StampBytes("deadbeef"))
	if got != "X-Imapsync-Go-Id: deadbeef\r\n" {
		t.Errorf("StampBytes() = %q", got)
	}

	// The stamp must survive a round trip through the parser it is meant to be
	// found by, or recovery searches for a header the message does not have.
	stamped := append(StampBytes("deadbeef"), sample...)
	fields := readHeader(stamped)
	if got := fields.Get(StampHeader); got != "deadbeef" {
		t.Errorf("stamped message parses back as %q, want deadbeef", got)
	}
	if id := Parse(stamped); id.MessageID != "<abc123@example.test>" {
		t.Errorf("prepending the stamp broke the rest of the header: %q", id.MessageID)
	}
}

// TestTheStampIsNotPartOfTheDigest keeps stamping from changing the identity it
// is meant to record. If it did, the value written would never match the value
// searched for.
func TestTheStampIsNotPartOfTheDigest(t *testing.T) {
	t.Parallel()

	before := Parse(sample)
	after := Parse(append(StampBytes(before.Digest), sample...))

	if before.Digest != after.Digest {
		t.Errorf("stamping changed the digest:\n before %s\n after  %s", before.Digest, after.Digest)
	}
}

// TestSentDateReadsTheDateHeader covers the age filter's default basis.
func TestSentDateReadsTheDateHeader(t *testing.T) {
	t.Parallel()

	got := SentDate([]byte("Subject: hi\r\nDate: Mon, 27 Aug 2026 12:00:00 +0000\r\n\r\n"))
	want := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("SentDate = %v, want %v", got, want)
	}
}

// A date carrying an offset is the same instant as its UTC equivalent, and must
// not be read as a different one: an hour's error at a --max-age boundary is a
// message copied or skipped wrongly.
func TestSentDateKeepsTheZoneOffset(t *testing.T) {
	t.Parallel()

	got := SentDate([]byte("Date: Mon, 27 Aug 2026 14:00:00 +0200\r\n\r\n"))
	want := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("SentDate = %v, want the same instant as %v", got, want)
	}
}

// Missing and unparseable dates are reported the same way, as the zero time,
// because the caller's remedy for both is to fall back to the internal date.
func TestSentDateReportsNothingItCannotRead(t *testing.T) {
	t.Parallel()

	for name, header := range map[string]string{
		"no date header":     "Subject: hi\r\nFrom: a@b.test\r\n\r\n",
		"empty header block": "",
		"unparseable date":   "Date: some time last Tuesday\r\n\r\n",
		"empty date":         "Date: \r\n\r\n",
	} {
		if got := SentDate([]byte(header)); !got.IsZero() {
			t.Errorf("%s: SentDate = %v, want the zero time", name, got)
		}
	}
}

// The header block a FETCH of selected fields returns often has no closing
// blank line. Parse already tolerates that; so must this, or every message on
// such a server would silently fall back to its internal date.
func TestSentDateToleratesAnUnterminatedHeader(t *testing.T) {
	t.Parallel()

	got := SentDate([]byte("Subject: hi\r\nDate: Mon, 27 Aug 2026 12:00:00 +0000\r\n"))
	if got.IsZero() {
		t.Error("SentDate found no date in a header with no closing blank line")
	}
}

// Date is in Fields, and age filtering works only because it is: the header
// reaches the filter as a side effect of being digested. An edit that dropped
// it would break selection somewhere far from here.
func TestTheDigestCoversTheDateHeader(t *testing.T) {
	t.Parallel()

	if !slices.Contains(Fields, "Date") {
		t.Error("Fields does not cover Date, so age filtering would see no sent date")
	}
}

// Real mail does not restrict itself to one date format, and every form here is
// legal under RFC 5322 or common enough to be worth accepting anyway.
//
// The consequence of failing to parse one is quiet rather than loud: the age
// filter falls back to the internal date, so the message is judged by when it
// arrived instead of when it was written. That is a wrong answer that looks
// exactly like a right one, which is why this pins the permissive parser rather
// than trusting the one format the other tests happen to use.
func TestSentDateAcceptsTheDateFormsRealMailUses(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	for name, raw := range map[string]string{
		"the usual form":     "Mon, 27 Aug 2026 12:00:00 +0000",
		"no day of week":     "27 Aug 2026 12:00:00 +0000",
		"obsolete zone name": "Mon, 27 Aug 2026 12:00:00 GMT",
		"trailing comment":   "Mon, 27 Aug 2026 12:00:00 +0000 (UTC)",
		"no seconds":         "Mon, 27 Aug 2026 12:00 +0000",
	} {
		got := SentDate([]byte("Date: " + raw + "\r\n\r\n"))
		if got.IsZero() {
			t.Errorf("%s: %q was not parsed at all", name, raw)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%s: %q gave %v, want %v", name, raw, got, want)
		}
	}

	// A day without zero padding is the same date, on a different day of the
	// month from the cases above.
	if got := SentDate([]byte("Date: Mon, 7 Aug 2026 12:00:00 +0000\r\n\r\n")); got.Day() != 7 {
		t.Errorf("an unpadded day gave %v", got)
	}
}
