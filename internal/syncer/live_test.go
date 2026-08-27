package syncer

import (
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/hilli/imapsync-go/internal/ident"
)

// TestTheAdoptionIndexHandsEachMessageOutOnce is the one property of the
// adoption index that only concurrency can break, and it cannot be reached from
// outside this package.
//
// Adopting consumes an entry: a digest maps to a list of destination UIDs
// because two identical messages in the source are two messages, and each must
// end up as its own message at the destination. If two workers read that list
// at the same instant they can both take its head, which copies one source
// message twice and abandons another — and the counts still add up, so nothing
// downstream notices.
//
// An end-to-end test cannot show this. Every adoption in the engine is
// bracketed by two SQLite writes, each of which takes about a millisecond,
// around a map access that takes about fifty nanoseconds. Removing the lock and
// running a 240-message folder five times under the race detector reports
// nothing at all: the window is real but far too narrow to hit by chance.
// Hitting it in production, on 776,747 messages, is a different matter, and the
// failure mode of a concurrent map write is the process being killed outright.
func TestTheAdoptionIndexHandsEachMessageOutOnce(t *testing.T) {
	const (
		digests = 250
		copies  = 4
		workers = 8
	)

	lv := &live{index: make(adoption, digests)}
	var next uint32
	var wants []ident.Identity
	for d := range digests {
		digest := fmt.Sprintf("digest-%04d", d)
		for range copies {
			next++
			lv.index[digest] = append(lv.index[digest], next)
			wants = append(wants, ident.Identity{Digest: digest})
		}
	}

	// Shuffled so that workers contend for the same digest rather than each
	// settling into its own corner of the map.
	rand.Shuffle(len(wants), func(i, j int) { wants[i], wants[j] = wants[j], wants[i] })

	claimed := make([][]uint32, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Every worker tries for every message. Only one can win each.
			for _, id := range wants {
				if uid, ok := lv.adopt(id); ok {
					claimed[w] = append(claimed[w], uid)
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	seen := make(map[uint32]int, digests*copies)
	total := 0
	for _, uids := range claimed {
		for _, uid := range uids {
			seen[uid]++
			total++
		}
	}

	if want := digests * copies; total != want {
		t.Errorf("handed out %d destination messages, want %d", total, want)
	}
	for uid, n := range seen {
		if n > 1 {
			t.Errorf("destination UID %d was adopted %d times", uid, n)
		}
	}
	if len(seen) != digests*copies {
		t.Errorf("%d distinct destination messages were adopted, want %d", len(seen), digests*copies)
	}

	// Nothing is left over, and nothing can be claimed twice.
	for _, id := range wants {
		if uid, ok := lv.adopt(id); ok {
			t.Fatalf("index still held UID %d for %q after every copy was claimed", uid, id.Digest)
		}
	}
}
