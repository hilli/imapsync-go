// Package dedup groups messages that may be copies of one another.
//
// What it returns are candidates, never conclusions. The key is a digest of six
// headers plus the server's byte count, which is strong evidence and not proof,
// and every use of this package ends in destroying a message or in declining to
// copy one. Both lose mail if the evidence was wrong, so both must confirm the
// bodies match before acting. Nothing here can confirm that: it never sees a
// body.
package dedup

import (
	"sort"

	"github.com/hilli/imapsync-go/internal/ident"
)

// Key is what two messages must share to be candidates.
//
// The digest covers Message-ID, Date, Subject, From, To and Cc — deliberately
// only headers that servers do not rewrite, since a header the destination
// normalises would make the same message digest differently on the two sides.
// That shortness is also why the digest cannot decide alone. A message with no
// Message-ID is digested from five headers, and two automated notifications
// sent in the same second with the same subject agree on all five while their
// bodies differ.
//
// Size is the second opinion, and it is free: FetchMeta already asks for
// RFC822.SIZE on every message it enumerates, so grouping costs no round trip.
// It is the server's claim rather than a measurement, which is one more reason
// the answer here is a candidate.
type Key struct {
	Digest string
	Size   int64
}

// Message is what grouping needs to know. Identity is taken whole rather than
// as a digest so that the weakness guard lives in here, where it cannot be
// forgotten, rather than in each caller.
type Message struct {
	UID      uint32
	Identity ident.Identity
	Size     int64
}

// Key returns the message's grouping key.
func (m Message) Key() Key { return Key{Digest: m.Identity.Digest, Size: m.Size} }

// groupable reports whether a message is identifiable enough to be grouped at
// all. An unidentifiable message is simply carried, which is the outcome that
// loses nothing.
//
// Weakness is the guard that matters. ident sets it when a message has no
// Message-ID and fewer than two other identity headers survived, and it already
// forbids adoption on a weak identity because a wrong match silently drops a
// message. Acting on one here is strictly worse, because the act is deletion or
// refusal to copy rather than a missed optimisation.
//
// A non-positive size means the server did not tell us, and the key would
// quietly degenerate to the digest alone — a silent widening, in a package
// whose every caller destroys or withholds mail. A real message cannot be zero
// bytes: it has headers.
func (m Message) groupable() bool {
	return !m.Identity.Weak && m.Identity.Digest != "" && m.Size > 0
}

// Group is two or more messages sharing a key, in ascending UID order.
type Group struct {
	Key      Key
	Messages []Message
}

// Candidates returns the groups of possible duplicates among msgs.
//
// Messages that share no key with another, and messages too poorly identified
// to group, are absent: a caller has nothing to decide about them.
//
// The order is deterministic — groups by their lowest UID, messages within a
// group by UID — because the alternative is Go's randomised map iteration, and
// a tool that deletes should not choose different victims on two runs over
// identical mailboxes.
func Candidates(msgs []Message) []Group {
	byKey := make(map[Key][]Message)
	for _, m := range msgs {
		if !m.groupable() {
			continue
		}
		k := m.Key()
		byKey[k] = append(byKey[k], m)
	}

	out := make([]Group, 0, len(byKey))
	for k, ms := range byKey {
		if len(ms) < 2 {
			continue
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].UID < ms[j].UID })
		out = append(out, Group{Key: k, Messages: ms})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Messages[0].UID < out[j].Messages[0].UID
	})
	return out
}

// Partition splits a group into the message to keep and the messages that may
// be removed, once their bodies have been confirmed to match the survivor's.
//
// Survivor and victims are chosen together, in one call, so that no caller can
// pair a survivor from one rule with victims from another and delete every
// copy.
//
// A claimed copy wins. The state database records which destination message a
// source message was copied to, and deleting that one while keeping an
// unclaimed twin leaves the row pointing at nothing — whereupon destination
// verification finds the claim unhonoured and copies a third. The two features
// would fight, and the visible symptom would be a folder that grew every time
// it was cleaned.
//
// Failing that, the lowest UID survives, so that repeated runs agree.
func (g Group) Partition(claimed func(uid uint32) bool) (survivor Message, victims []Message) {
	survivor = g.Messages[0]
	if claimed != nil {
		for _, m := range g.Messages {
			if claimed(m.UID) {
				survivor = m
				break
			}
		}
	}

	victims = make([]Message, 0, len(g.Messages)-1)
	for _, m := range g.Messages {
		if m.UID != survivor.UID {
			victims = append(victims, m)
		}
	}
	return survivor, victims
}
