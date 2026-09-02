package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hilli/imapsync-go/internal/syncer"
)

// The duplicates note carries its count in three places, and the first draft
// read "The destination holds one copies of each" — the same class of mistake
// the missing-copy note made twice before anything read it back.
//
// This note has a second job the other one does not. A reader who sees mail
// counted as not copied wants to know at once whether the tool guessed, so the
// sentence has to say what was compared and name the way out. A test that only
// checked agreement would let either of those be dropped.
func TestTheDuplicatesNoteAgreesWithItsCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		count  int
		want   []string
		reject []string
	}{
		{
			name:  "one repeat",
			count: 1,
			want: []string{
				"1 source message was byte for byte identical to one already copied and was not copied a second time",
				"The destination holds one copy of it",
				"--sync-duplicates",
			},
			reject: []string{"messages", " were ", "of each", "copies"},
		},
		{
			name:  "several repeats",
			count: 4,
			want: []string{
				"4 source messages were byte for byte identical to one already copied and were not copied a second time",
				"The destination holds one copy of each",
				"--sync-duplicates",
			},
			reject: []string{" was ", "of it", "one copies"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			p := &printer{w: &buf}
			writeDuplicatesNote(p, reportWithDuplicates(tc.count))
			got := buf.String()

			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("note does not say %q:\n%s", want, got)
				}
			}
			for _, reject := range tc.reject {
				if strings.Contains(got, reject) {
					t.Errorf("note says %q, which does not agree with a count of %d:\n%s", reject, tc.count, got)
				}
			}
		})
	}
}

// Silent on the overwhelming majority of runs, which hold no repeats at all.
func TestTheDuplicatesNoteIsSilentWhenThereAreNone(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := &printer{w: &buf}
	writeDuplicatesNote(p, reportWithDuplicates(0))
	if buf.Len() != 0 {
		t.Errorf("a run with no repeats said %q", buf.String())
	}
}

func reportWithDuplicates(n int) syncer.Report {
	return syncer.Report{Folders: []syncer.FolderReport{{Source: "INBOX", Dest: "INBOX", Duplicates: n}}}
}
