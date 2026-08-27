package syncer

import (
	"slices"
	"testing"

	"github.com/hilli/imapsync-go/internal/ident"
)

// TestCopyableFlagsDropsRecent is white-box because the property is invisible
// from outside: the destination sets \Recent on arrival regardless, so a copied
// \Recent and a server-assigned one look identical at the far end. The only
// place the distinction exists is here.
func TestCopyableFlagsDropsRecent(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"nothing to drop", []string{"\\Seen", "\\Flagged"}, []string{"\\Seen", "\\Flagged"}},
		{"drops Recent", []string{"\\Seen", "\\Recent", "\\Answered"}, []string{"\\Seen", "\\Answered"}},
		{"case insensitive", []string{"\\recent", "\\Seen"}, []string{"\\Seen"}},
		{"keeps keywords", []string{"$Junk", "NonJunk", "\\Recent"}, []string{"$Junk", "NonJunk"}},
		{"no flags at all", nil, []string{}},
		{"only Recent", []string{"\\Recent"}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := copyableFlags(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("copyableFlags(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestTakeConsumesOneMatch is the multiplicity rule at unit level: two identical
// destination messages can satisfy two source messages, and no more.
func TestTakeConsumesOneMatch(t *testing.T) {
	t.Parallel()

	index := adoption{"abc": {7, 9}}
	id := identityOf("abc")

	if uid, ok := index.take(id); !ok || uid != 7 {
		t.Errorf("first take = %d, %v; want 7, true", uid, ok)
	}
	if uid, ok := index.take(id); !ok || uid != 9 {
		t.Errorf("second take = %d, %v; want 9, true", uid, ok)
	}
	if uid, ok := index.take(id); ok {
		t.Errorf("third take = %d, %v; want nothing left", uid, ok)
	}
}

func TestTakeRefusesUnknownIdentities(t *testing.T) {
	t.Parallel()

	index := adoption{"abc": {7}}
	if uid, ok := index.take(identityOf("zzz")); ok {
		t.Errorf("take(unknown) = %d, %v; want no match", uid, ok)
	}

	// And a nil index, which is what an empty destination produces.
	var empty adoption
	if uid, ok := empty.take(identityOf("abc")); ok {
		t.Errorf("take on an unbuilt index = %d, %v; want no match", uid, ok)
	}
}

func identityOf(digest string) ident.Identity {
	return ident.Identity{Digest: digest}
}
