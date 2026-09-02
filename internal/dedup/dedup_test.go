package dedup

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/hilli/imapsync-go/internal/ident"
)

// Two appearances of the same message are candidates, and two different
// messages are not.
//
// The pair is the point. The first case alone is passed by a version that
// groups everything, and the second alone by a version that groups nothing.
// Only together do they say that the key discriminates. This is the shape the
// destination-verification work arrived at the hard way: a count of two is also
// what total failure produces.
func TestTheSameMessageTwiceIsACandidateAndTwoMessagesAreNot(t *testing.T) {
	t.Parallel()

	same := Candidates([]Message{
		msg(1, "digest-a", 500),
		msg(2, "digest-a", 500),
	})
	if len(same) != 1 {
		t.Fatalf("two copies of one message gave %d groups, want 1", len(same))
	}
	if got := uids(same[0].Messages); !reflect.DeepEqual(got, []uint32{1, 2}) {
		t.Errorf("group holds %v, want [1 2]", got)
	}

	different := Candidates([]Message{
		msg(1, "digest-a", 500),
		msg(2, "digest-b", 500),
	})
	if len(different) != 0 {
		t.Errorf("two different messages gave %d groups, want 0", len(different))
	}
}

// Size is the half of the key that the digest cannot supply.
//
// A message with no Message-ID is digested from five headers, so two automated
// notifications sent in the same second with the same subject and sender agree
// on every one of them. Their lengths are the cheapest thing that still tells
// them apart, and it arrives free with the enumeration.
func TestMessagesThatAgreeOnEveryHeaderButDifferInLengthAreNotCandidates(t *testing.T) {
	t.Parallel()

	got := Candidates([]Message{
		msg(1, "digest-a", 500),
		msg(2, "digest-a", 501),
	})
	if len(got) != 0 {
		t.Errorf("messages of different length were grouped: %v", got)
	}
}

// A weak identity is one ident says cannot tell two messages apart. Adoption
// already refuses to act on one because a wrong match silently drops a message;
// here the act is deletion, or a refusal to copy, so the same evidence buys
// even less.
func TestAWeaklyIdentifiedMessageIsNeverACandidate(t *testing.T) {
	t.Parallel()

	weak := msg(2, "digest-a", 500)
	weak.Identity.Weak = true
	alsoWeak := msg(3, "digest-a", 500)
	alsoWeak.Identity.Weak = true

	got := Candidates([]Message{msg(1, "digest-a", 500), weak, alsoWeak})
	if len(got) != 0 {
		t.Fatalf("a weak message was grouped: %v", got)
	}
}

// A server that did not report RFC822.SIZE would leave every message claiming
// zero, and the key would quietly become the digest alone. Widening a key
// silently is the failure mode this package must not have, so an unmeasured
// message is carried instead.
func TestAMessageWhoseSizeTheServerDidNotReportIsNeverACandidate(t *testing.T) {
	t.Parallel()

	got := Candidates([]Message{
		msg(1, "digest-a", 0),
		msg(2, "digest-a", 0),
	})
	if len(got) != 0 {
		t.Errorf("messages of unknown length were grouped: %v", got)
	}
}

// Go randomises map iteration, so an unsorted result would differ between runs
// over an identical mailbox — and a tool that deletes would choose different
// victims each time. Five groups make an accidental ordering a 1-in-120 event,
// and repeating turns that into a certainty.
func TestTheSameMailboxAlwaysGivesTheSameAnswer(t *testing.T) {
	t.Parallel()

	var in []Message
	for g := range 5 {
		digest := fmt.Sprintf("digest-%d", g)
		size := int64(100 + g)
		in = append(in, msg(uint32(10*(g+1)), digest, size), msg(uint32(10*(g+1)+1), digest, size))
	}

	first := uids(flatten(Candidates(in)))
	want := []uint32{10, 11, 20, 21, 30, 31, 40, 41, 50, 51}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("groups came back as %v, want %v", first, want)
	}
	for i := range 20 {
		if got := uids(flatten(Candidates(in))); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d gave %v, want %v", i, got, first)
		}
	}
}

// Deleting the copy the state database points at, while keeping its twin,
// leaves the row pointing at nothing. Destination verification then finds the
// claim unhonoured and copies a third, so the folder grows every time it is
// cleaned. That is why the claimed copy survives, and it is worth asserting by
// UID rather than by count.
func TestTheClaimedCopyIsTheOneThatSurvives(t *testing.T) {
	t.Parallel()

	g := Group{Messages: []Message{msg(1, "d", 9), msg(2, "d", 9), msg(3, "d", 9)}}

	survivor, victims := g.Partition(func(uid uint32) bool { return uid == 2 })
	if survivor.UID != 2 {
		t.Errorf("kept UID %d, want the claimed copy 2", survivor.UID)
	}
	if got := uids(victims); !reflect.DeepEqual(got, []uint32{1, 3}) {
		t.Errorf("victims are %v, want [1 3]", got)
	}
}

// With nothing claimed the choice is arbitrary, and arbitrary must still be
// repeatable: two runs over the same folder have to pick the same survivor or
// they take turns deleting each other's.
func TestWithNothingClaimedTheLowestUIDSurvives(t *testing.T) {
	t.Parallel()

	g := Group{Messages: []Message{msg(4, "d", 9), msg(7, "d", 9)}}
	for _, claimed := range []func(uint32) bool{nil, func(uint32) bool { return false }} {
		survivor, victims := g.Partition(claimed)
		if survivor.UID != 4 {
			t.Errorf("kept UID %d, want 4", survivor.UID)
		}
		if got := uids(victims); !reflect.DeepEqual(got, []uint32{7}) {
			t.Errorf("victims are %v, want [7]", got)
		}
	}
}

// The one outcome that must never happen, whatever else is wrong: every copy
// condemned. Asserted over every claim a caller could make, including claims on
// several copies and on none.
func TestTheSurvivorIsNeverAmongTheVictims(t *testing.T) {
	t.Parallel()

	g := Group{Messages: []Message{msg(1, "d", 9), msg(2, "d", 9), msg(3, "d", 9)}}
	claims := []func(uint32) bool{
		nil,
		func(uint32) bool { return false },
		func(uint32) bool { return true },
		func(uid uint32) bool { return uid == 1 },
		func(uid uint32) bool { return uid == 3 },
		func(uid uint32) bool { return uid != 2 },
	}

	for i, claimed := range claims {
		survivor, victims := g.Partition(claimed)
		if len(victims) != len(g.Messages)-1 {
			t.Errorf("claim %d condemned %d of %d messages, want %d",
				i, len(victims), len(g.Messages), len(g.Messages)-1)
		}
		for _, v := range victims {
			if v.UID == survivor.UID {
				t.Errorf("claim %d condemned the survivor, UID %d", i, survivor.UID)
			}
		}
	}
}

// A message appearing once has nothing for a caller to decide, and returning it
// would invite a caller to delete "all but the first" of a group of one.
func TestAMessageWithNoTwinIsNotReturned(t *testing.T) {
	t.Parallel()

	got := Candidates([]Message{
		msg(1, "digest-a", 500),
		msg(2, "digest-b", 500),
		msg(3, "digest-b", 500),
	})
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if u := uids(got[0].Messages); !reflect.DeepEqual(u, []uint32{2, 3}) {
		t.Errorf("group holds %v, want [2 3]", u)
	}
}

// A Message built without parsing an identity has an empty digest, and that is
// the zero value rather than an exotic case: Message{UID: 1, Size: 500} is not
// weak and has no digest. Without this guard every such message would share a
// key with every other of its length, which is the worst possible grouping in a
// package whose callers delete.
func TestAMessageWithNoDigestIsNeverACandidate(t *testing.T) {
	t.Parallel()

	got := Candidates([]Message{
		{UID: 1, Size: 500},
		{UID: 2, Size: 500},
	})
	if len(got) != 0 {
		t.Errorf("messages with no identity were grouped: %v", got)
	}
}

// Two source duplicates both copied leave two claimed destination copies, so
// "the claimed one" does not always name a single message. The lowest wins,
// matching the unclaimed rule, because the alternative is two runs picking
// different survivors and deleting each other's.
func TestWhenSeveralCopiesAreClaimedTheLowestSurvives(t *testing.T) {
	t.Parallel()

	g := Group{Messages: []Message{msg(1, "d", 9), msg(2, "d", 9), msg(3, "d", 9)}}
	survivor, victims := g.Partition(func(uid uint32) bool { return uid != 2 })
	if survivor.UID != 1 {
		t.Errorf("kept UID %d, want the lowest claimed copy 1", survivor.UID)
	}
	if got := uids(victims); !reflect.DeepEqual(got, []uint32{2, 3}) {
		t.Errorf("victims are %v, want [2 3]", got)
	}
}

// UIDs do not arrive in order. iCloud's UID SEARCH ALL returns them unsorted —
// established while fixing that server's phantom-UID bug — so a group built in
// arrival order is in no order at all, and "keep the lowest" would mean "keep
// whichever the server mentioned first".
func TestAGroupIsOrderedEvenWhenTheServerIsNot(t *testing.T) {
	t.Parallel()

	got := Candidates([]Message{
		msg(70, "digest-a", 500),
		msg(9, "digest-a", 500),
		msg(31, "digest-a", 500),
	})
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if u := uids(got[0].Messages); !reflect.DeepEqual(u, []uint32{9, 31, 70}) {
		t.Fatalf("group holds %v, want [9 31 70]", u)
	}

	survivor, victims := got[0].Partition(nil)
	if survivor.UID != 9 {
		t.Errorf("kept UID %d, want the lowest 9", survivor.UID)
	}
	if u := uids(victims); !reflect.DeepEqual(u, []uint32{31, 70}) {
		t.Errorf("victims are %v, want [31 70]", u)
	}
}

func msg(uid uint32, digest string, size int64) Message {
	return Message{UID: uid, Identity: ident.Identity{Digest: digest}, Size: size}
}

func uids(ms []Message) []uint32 {
	out := make([]uint32, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.UID)
	}
	return out
}

func flatten(gs []Group) []Message {
	var out []Message
	for _, g := range gs {
		out = append(out, g.Messages...)
	}
	return out
}
