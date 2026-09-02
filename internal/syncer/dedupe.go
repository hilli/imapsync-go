package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/hilli/imapsync-go/internal/dedup"
	"github.com/hilli/imapsync-go/internal/ident"
	"github.com/hilli/imapsync-go/internal/imapx"
)

// dedupeResult is what one pass over a destination folder's own duplicates
// found.
//
// Unequal is reported separately from Removed rather than folded into it
// because the two say opposite things about the key. A run that keeps finding
// candidates it cannot confirm is a run whose grouping is too loose, and that
// is worth being able to see before it is worth acting on.
type dedupeResult struct {
	Population int
	Removed    int
	Refused    int
	Unequal    int
}

// dedupeVictim is a destination message confirmed to repeat another.
type dedupeVictim struct {
	uid      uint32
	survivor uint32
}

// dedupeDest removes messages a destination folder holds more than once.
//
// This is the destination half of duplicate handling, and it is a purely local
// question: whether one mailbox holds the same message twice has nothing to do
// with the source. That is why the standalone dedup command can drive it with
// no source connection at all.
//
// The order of the two writes is chosen so that either failure is harmless.
// Rows are re-pointed at the survivor before the messages go, because the
// survivor is a truthful answer for those source messages whether or not the
// deletion then succeeds — the bodies were compared in full. Deleting first and
// failing to re-point would leave rows naming a message that is gone, which
// destination verification would repair by copying the duplicate back.
func (s *Syncer) dedupeDest(
	ctx context.Context,
	dst imapx.Conn,
	box imapx.Mailbox,
	folderID int64,
	srcUIDValidity uint32,
	dryRun bool,
) (dedupeResult, error) {
	var res dedupeResult

	msgs, population, err := s.dedupeCandidates(ctx, dst, box)
	if err != nil {
		return res, err
	}
	res.Population = population
	groups := dedup.Candidates(msgs)
	if len(groups) == 0 {
		return res, nil
	}

	mirrors, err := s.db.Mirrored(ctx, folderID, srcUIDValidity, box.UIDValidity)
	if err != nil {
		return res, err
	}
	claimedBy := make(map[uint32][]uint32, len(mirrors))
	for _, m := range mirrors {
		claimedBy[m.DstUID] = append(claimedBy[m.DstUID], m.SrcUID)
	}

	victims, unequal, err := s.confirmDuplicates(ctx, dst, groups, claimedBy)
	if err != nil {
		return res, err
	}
	res.Unequal = unequal
	if len(victims) == 0 {
		return res, nil
	}

	// The same ceiling deletion uses, measured the same way. A duplicate
	// deletion is a deletion, and a key that collided across a whole folder is
	// exactly the accident the ceiling exists to stop.
	if !s.allowedToDelete(len(victims), population) {
		res.Refused = len(victims)
		s.log.Error("refusing to remove this many duplicates at once",
			"dest", box.Name, "would_remove", len(victims), "of", population,
			"share", pct(float64(len(victims))/float64(population)), "ceiling", pct(s.ceiling()))
		return res, nil
	}

	if dryRun {
		res.Removed = len(victims)
		return res, nil
	}

	uids := make([]uint32, 0, len(victims))
	for _, v := range victims {
		for _, srcUID := range claimedBy[v.uid] {
			if err := s.db.CompleteAppend(ctx, folderID, srcUIDValidity, srcUID, box.UIDValidity, v.survivor); err != nil {
				return res, fmt.Errorf("pointing message %d at the copy that survived deduplication: %w", srcUID, err)
			}
		}
		uids = append(uids, v.uid)
	}

	if err := dst.DeleteMessages(ctx, uids); err != nil {
		return res, fmt.Errorf("removing %d duplicates from %q: %w", len(uids), box.Name, err)
	}
	res.Removed = len(uids)
	s.log.Info("removed messages the destination folder held more than once",
		"dest", box.Name, "removed", res.Removed, "of", population, "not_identical", unequal)
	return res, nil
}

// dedupeCandidates reads enough of every message in the folder to group it.
//
// The metadata comes from the destination rather than from the state database,
// even for messages we put there ourselves and already recorded a digest and a
// size for. Those numbers describe the *source* message: a copy this tool made
// carries a stamp header the source message did not, so its size differs by
// that line, and comparing a source-claimed size with a destination-claimed one
// would put two copies of one message in different groups. Every key here is
// built from one server's answers about its own messages.
//
// That costs a metadata pass over the folder, which is the honest price of the
// feature and is why it is opt-in. It is the same pass the source side makes on
// every --full run.
func (s *Syncer) dedupeCandidates(ctx context.Context, dst imapx.Conn, box imapx.Mailbox) ([]dedup.Message, int, error) {
	uids, err := dst.AllUIDs(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("enumerating destination messages: %w", err)
	}
	if len(uids) < 2 {
		return nil, len(uids), nil
	}

	msgs := make([]dedup.Message, 0, len(uids))
	for start := 0; start < len(uids); start += metaBatch {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		end := min(start+metaBatch, len(uids))
		metas, err := dst.FetchMeta(ctx, uids[start:end], ident.FetchFields)
		if err != nil {
			return nil, 0, fmt.Errorf("reading destination headers for %q: %w", box.Name, err)
		}
		for _, meta := range metas {
			msgs = append(msgs, dedup.Message{
				UID:      meta.UID,
				Identity: ident.Parse(meta.Header),
				Size:     meta.Size,
			})
		}
	}
	return msgs, len(uids), nil
}

// confirmDuplicates fetches the candidates and keeps only those that match a
// survivor byte for byte.
//
// The comparison is the whole message, which is the only thing that makes
// deleting one safe. dedup's key is six headers and a byte count the server
// claimed, and two automated notifications sent in the same second can agree on
// all of it while saying different things. Proof is one fetch away and the cost
// is proportional to the duplicates found rather than to the folder.
//
// A survivor whose body cannot be read takes its whole group out of the run.
// There is nothing to compare against, and the alternative — promoting the next
// candidate and comparing the rest to that — would delete messages on the
// strength of an error.
func (s *Syncer) confirmDuplicates(
	ctx context.Context,
	dst imapx.Conn,
	groups []dedup.Group,
	claimedBy map[uint32][]uint32,
) (victims []dedupeVictim, unequal int, err error) {
	claimed := func(uid uint32) bool { _, ok := claimedBy[uid]; return ok }

	for _, g := range groups {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		survivor, candidates := g.Partition(claimed)
		want, err := s.bodySum(ctx, dst, survivor.UID)
		if err != nil {
			s.log.Warn("leaving a group of possible duplicates alone: the message they would be compared against could not be read",
				"uid", survivor.UID, "candidates", len(candidates), "err", err)
			continue
		}
		for _, c := range candidates {
			got, err := s.bodySum(ctx, dst, c.UID)
			if err != nil {
				s.log.Warn("leaving a possible duplicate alone: it could not be read",
					"uid", c.UID, "err", err)
				continue
			}
			if got != want {
				unequal++
				continue
			}
			victims = append(victims, dedupeVictim{uid: c.UID, survivor: survivor.UID})
		}
	}
	return victims, unequal, nil
}

// bodySum is the SHA-256 of a message as the server returns it.
func (s *Syncer) bodySum(ctx context.Context, dst imapx.Conn, uid uint32) ([sha256.Size]byte, error) {
	var buf bytes.Buffer
	if _, err := dst.FetchBody(ctx, uid, &buf); err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(buf.Bytes()), nil
}

// allowedToDelete applies the deletion safety valve.
//
// Extracted so that duplicate removal is measured by exactly the rule ordinary
// deletion is, rather than by a second copy of it that could drift.
func (s *Syncer) allowedToDelete(nominated, population int) bool {
	if nominated == 0 || population == 0 {
		return true
	}
	if nominated <= defaultDeleteFloor {
		return true
	}
	return float64(nominated)/float64(population) <= s.ceiling() || s.opts.Force
}
