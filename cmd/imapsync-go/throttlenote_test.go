package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/throttle"
)

// The throttle note has one job the other notes do not: it has to make a slow
// run explicable. Without it a throttled run and a slow server read the same
// way, and the reader cannot tell whether the thing holding them up is their
// own brake or the network.
//
// So it speaks whenever a limit was set rather than only when the limit bound.
// That rule is not fastidiousness — the connection note spoke only when a pool
// shrank, and two full runs of three quarters of a million messages went by
// without answering the questions it existed to answer, because "the pool held"
// and "nothing was measured" printed identically as nothing at all.
func TestTheThrottleNoteSaysWhatWasAskedAndWhatItCost(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		stats  throttle.Stats
		want   []string
		reject []string
	}{
		{
			name:   "no limit says nothing",
			stats:  throttle.Stats{},
			reject: []string{"Rate limited"},
		},
		{
			name: "a byte limit that bound",
			stats: throttle.Stats{
				BytesPerSec: 2 << 20,
				Waited:      90 * time.Second,
				Moved:       512 << 20,
			},
			want:   []string{"Rate limited to 2MiB/second.", "waited 1m30s on it", "512MiB of message data"},
			reject: []string{"messages/second", "Nothing ever waited"},
		},
		{
			name:   "a message limit that never bound",
			stats:  throttle.Stats{MessagesPerSec: 12},
			want:   []string{"Rate limited to 12 messages/second.", "Nothing ever waited on it"},
			reject: []string{"MiB/second", "KiB/second", "B/second"},
		},
		{
			// A message limit that did bind still moved bytes, and has to say
			// how many. A live run reported "moved 0B" of eighty kilobytes
			// because the volume was counted only where the byte allowance was.
			name: "a message limit that bound still names the volume",
			stats: throttle.Stats{
				MessagesPerSec: 5,
				Waited:         2500 * time.Millisecond,
				Moved:          80 << 10,
			},
			want:   []string{"Rate limited to 5 messages/second.", "waited 2.5s on it", "80KiB of message data"},
			reject: []string{"Nothing ever waited", "moved 0B"},
		},
		{
			name: "both limits are named",
			stats: throttle.Stats{
				BytesPerSec: 512 << 10, MessagesPerSec: 0.5,
				Waited: time.Second, Moved: 1 << 10,
			},
			want: []string{"Rate limited to 512KiB/second and 0.5 messages/second."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer
			p := &printer{w: &out}
			writeThrottleNote(p, tc.stats)
			if p.err != nil {
				t.Fatalf("writing the note: %v", p.err)
			}
			got := out.String()

			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the note does not say %q:\n%s", want, got)
				}
			}
			for _, reject := range tc.reject {
				if strings.Contains(got, reject) {
					t.Errorf("the note says %q, which does not apply here:\n%s", reject, got)
				}
			}
		})
	}
}

// TestTheReportedLimitCanBePastedBackIntoTheFlag.
//
// The number in the report is the number someone will copy into their next
// command line, so it has to be spelled the way the flag reads it. A report
// saying "2097152 bytes/second" would be correct and useless.
//
// The claim is made of the note rather than only of flagBytes, because
// asserting it of the function alone let a mutation through: swapping the call
// site to humanBytes — the approximate spelling, which the flag rejects — broke
// nothing, since the only test of the distinction never went near the report.
func TestTheReportedLimitCanBePastedBackIntoTheFlag(t *testing.T) {
	t.Parallel()

	// 1500 bytes is the discriminating case. humanBytes calls it 1.465KiB,
	// which is a fair description of it and is not a number the flag accepts.
	var buf bytes.Buffer
	writeThrottleNote(&printer{w: &buf}, throttle.Stats{BytesPerSec: 1500})
	limit, ok := strings.CutSuffix(strings.Fields(strings.SplitN(buf.String(), "Rate limited to ", 2)[1])[0], "/second.")
	if !ok {
		t.Fatalf("cannot find the limit in the note:\n%s", buf.String())
	}
	if back, err := parseBytes(limit); err != nil {
		t.Errorf("the report names a limit of %q, which --max-bytes-per-second will not accept: %v", limit, err)
	} else if back != 1500 {
		t.Errorf("the report names a limit of %q, which reads back as %d rather than 1500", limit, back)
	}

	// Round limits, awkward ones, and the boundaries between units. The
	// awkward ones are the point: 1500 bytes is 1.465KiB, which is a fair
	// description and is rejected by the flag it describes.
	for _, n := range []int64{1, 999, 1023, 1 << 10, 1500, (1 << 20) - 1, 2 << 20, 512 << 10, 3 << 30, (3 << 30) + 7} {
		got := flagBytes(n)
		back, err := parseBytes(got)
		if err != nil {
			t.Errorf("flagBytes(%d) = %q, which --max-bytes-per-second will not accept: %v", n, got, err)
			continue
		}
		if back != n {
			t.Errorf("flagBytes(%d) = %q, which reads back as %d", n, got, back)
		}
	}
}
