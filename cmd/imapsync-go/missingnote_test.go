package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hilli/imapsync-go/internal/syncer"
)

// The note has been grammatically wrong twice: once agreeing the verb with the
// plural while the count was one, and once using the object pronoun as the
// subject of the next clause ("and them have been copied again"). Both got as
// far as a real run against a real server, because nothing here reads the
// sentence back.
//
// A report that cannot write a number of messages correctly is a report a
// reader stops trusting about the numbers themselves, and this project has
// already had three findings that were entirely about a run describing itself
// inaccurately.
func TestTheMissingCopyNoteAgreesWithItsCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		missing int
		dryRun  bool
		want    []string
		reject  []string
	}{
		{
			name:    "one message, real run",
			missing: 1,
			want:    []string{"1 message recorded as copied was no longer", "removed it, and it has been copied again"},
			reject:  []string{"messages", "were", "them", "they"},
		},
		{
			name:    "several messages, real run",
			missing: 3,
			want:    []string{"3 messages recorded as copied were no longer", "removed them, and they have been copied again"},
			reject:  []string{" was ", " it "},
		},
		{
			name:    "one message, dry run",
			missing: 1,
			dryRun:  true,
			want:    []string{"1 message recorded as copied was no longer", "a real run would copy it again"},
			reject:  []string{"has been copied", "were"},
		},
		{
			name:    "several messages, dry run",
			missing: 2,
			dryRun:  true,
			want:    []string{"2 messages recorded as copied were no longer", "a real run would copy them again"},
			reject:  []string{"have been copied", " was "},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			p := &printer{w: &buf}
			writeMissingNote(p, reportWithMissing(tc.missing), tc.dryRun)
			got := buf.String()

			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("note does not say %q:\n%s", want, got)
				}
			}
			for _, reject := range tc.reject {
				if strings.Contains(got, reject) {
					t.Errorf("note says %q, which does not agree with a count of %d:\n%s", reject, tc.missing, got)
				}
			}
		})
	}
}

// A healthy run must say nothing at all. A line reading "0 messages recorded as
// copied were no longer on the destination" on every run is how a warning stops
// being read.
func TestTheMissingCopyNoteIsSilentWhenNothingIsMissing(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	p := &printer{w: &buf}
	writeMissingNote(p, reportWithMissing(0), false)
	if buf.Len() != 0 {
		t.Errorf("a run that lost nothing said %q", buf.String())
	}
}

func reportWithMissing(n int) syncer.Report {
	return syncer.Report{Folders: []syncer.FolderReport{{Source: "INBOX", Dest: "INBOX", Missing: n}}}
}
