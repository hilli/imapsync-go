package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/throttle"
)

// The README quotes two of the run report's notes verbatim, inside fenced
// blocks, because a paraphrase of a report is worth much less than the report:
// the reader is trying to recognise something they will see on their own
// terminal.
//
// Quoting it makes the README a copy of code, and a copy goes stale in silence.
// Both of these already had: the connection note still said the run "settled
// on" a width months after the governor stopped settling and started climbing
// back, and the passage around it still explained that the pool never grows.
// Nothing failed, because nothing was looking.
//
// So the README's copies are generated here from the functions that print them
// and compared. This does not check that the prose around them is true — no
// test can — but it removes the failure that has actually happened twice, which
// is the wording moving underneath a quotation nobody thought to re-read.
//
// Only the blocks the README quotes verbatim are pinned. Its `compat` examples
// are hand-wrapped and hand-padded to fit the page, so they are illustrations
// rather than copies and there is nothing exact to compare them against; the
// generated option matrix covers that table instead.
func TestTheREADMEQuotesTheReportAsItIsActuallyPrinted(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatalf("reading the README: %v", err)
	}
	doc := string(readme)

	for _, tc := range []struct {
		name  string
		write func(*printer)
	}{
		{
			// The numbers are the README's own. What is pinned is the sentence
			// they sit in.
			name: "the connection note",
			write: func(p *printer) {
				writeConnectionNote(p, connections{{
					side: "destination", flag: "dest-connections", asked: 16, got: 12,
				}})
			},
		},
		{
			name: "the throttle note",
			write: func(p *printer) {
				writeThrottleNote(p, throttle.Stats{
					BytesPerSec: 1 << 20,
					Waited:      4*time.Minute + 12*time.Second,
					Moved:       1120986464,
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			tc.write(&printer{w: &buf})
			want := strings.TrimSpace(buf.String())

			if !strings.Contains(doc, want) {
				t.Errorf("the README does not quote %s as it is printed.\n\n"+
					"What the code prints:\n%s\n\n"+
					"Paste that into the fenced block in README.md, and check the paragraph\n"+
					"around it still describes what the tool does — the wording changing is\n"+
					"usually a sign that the behaviour did.",
					tc.name, want)
			}
		})
	}
}
