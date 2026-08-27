package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/probe"
)

func TestWriteReportFlagsUnusableListStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		caps         imapx.Caps
		wantContains string
	}{
		{
			// iCloud advertises LIST-STATUS without LIST-EXTENDED. LIST-STATUS
			// is a LIST return option, so the claim is unusable, and reporting
			// it as a saving would send someone looking for a round trip they
			// cannot avoid.
			name:         "LIST-STATUS without LIST-EXTENDED is called out",
			caps:         imapx.Caps{ListStatus: true},
			wantContains: "unusable without LIST-EXTENDED",
		},
		{
			name:         "LIST-STATUS with LIST-EXTENDED is reported as useful",
			caps:         imapx.Caps{ListStatus: true, ListExtended: true},
			wantContains: "folder counts without a round trip each",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			p := printer{w: &buf}
			writeReport(&p, "source", &probe.Report{Caps: tt.caps, SpecialUse: map[string]string{}})
			if p.err != nil {
				t.Fatalf("writeReport() error = %v", p.err)
			}
			if !strings.Contains(buf.String(), tt.wantContains) {
				t.Errorf("report missing %q:\n%s", tt.wantContains, buf.String())
			}
		})
	}
}

func TestWriteReportWarnsOnUnadvertisedSpecialUse(t *testing.T) {
	t.Parallel()

	const warning = "may be incomplete"

	// iCloud returns \Sent and \Trash but leaves Drafts, Junk and Archive
	// unmarked, and never advertises the extension. A partial mapping presented
	// as authoritative is how folders end up silently unmatched.
	report := &probe.Report{
		Caps:       imapx.Caps{},
		SpecialUse: map[string]string{`\Sent`: "Sent Messages"},
	}

	var buf bytes.Buffer
	p := printer{w: &buf}
	writeReport(&p, "source", report)
	if p.err != nil {
		t.Fatalf("writeReport() error = %v", p.err)
	}
	if !strings.Contains(buf.String(), warning) {
		t.Errorf("report did not caveat an unadvertised special-use mapping:\n%s", buf.String())
	}

	// With the capability advertised the mapping is trustworthy, so the caveat
	// would just be noise.
	report.Caps.SpecialUse = true
	buf.Reset()
	p = printer{w: &buf}
	writeReport(&p, "source", report)
	if strings.Contains(buf.String(), warning) {
		t.Errorf("report caveated a mapping the server vouched for:\n%s", buf.String())
	}
}
