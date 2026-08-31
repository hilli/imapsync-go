package imapx

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestTraceWriterRedactsCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "LOGIN password is removed but the user is kept",
			input: "a001 LOGIN \"me@icloud.com\" \"abcd-efgh-ijkl-mnop\"\r\n",
			want:  "a001 LOGIN \"me@icloud.com\" <redacted>\r\n",
		},
		{
			name:  "lowercase login is still redacted",
			input: "a001 login user hunter2\r\n",
			want:  "a001 login user <redacted>\r\n",
		},
		{
			name:  "SASL initial response is removed",
			input: "a002 AUTHENTICATE PLAIN AGZvbwBiYXI=\r\n",
			want:  "a002 AUTHENTICATE PLAIN <redacted>\r\n",
		},
		{
			name:  "the client's answer to a continuation is removed",
			input: "a002 AUTHENTICATE PLAIN\r\n+ \r\nAGZvbwBiYXI=\r\n",
			want:  "a002 AUTHENTICATE PLAIN\r\n+ \r\n<redacted SASL payload>\r\n",
		},
		{
			name:  "ordinary traffic passes through untouched",
			input: "a003 LIST \"\" \"*\"\r\n* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\na003 OK done\r\n",
			want:  "a003 LIST \"\" \"*\"\r\n* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\na003 OK done\r\n",
		},
		{
			name:  "a server line quoting LOGIN is not mistaken for a command",
			input: "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready\r\n",
			want:  "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] ready\r\n",
		},
		{
			name:  "bare LF is preserved",
			input: "a001 LOGIN user secret\n",
			want:  "a001 LOGIN user <redacted>\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			w := newTraceWriter(&buf)
			if _, err := w.Write([]byte(tt.input)); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("trace output\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// TestTraceWriterRedactsAcrossFragments matters because the debug writer is fed
// whatever chunks the socket produced, not whole lines. A password split across
// two reads must not slip through.
func TestTraceWriterRedactsAcrossFragments(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newTraceWriter(&buf)

	for _, chunk := range []string{"a001 LOG", "IN user hun", "ter2\r\n"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	got := buf.String()
	if strings.Contains(got, "hunter2") {
		t.Fatalf("password leaked across fragment boundary: %q", got)
	}
	if got != "a001 LOGIN user <redacted>\r\n" {
		t.Errorf("trace output = %q", got)
	}
}

// TestTraceWriterFlushesLongLiterals guards against holding an entire message
// in memory waiting for a newline that a binary literal may never contain.
func TestTraceWriterFlushesLongLiterals(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newTraceWriter(&buf)

	blob := bytes.Repeat([]byte{'x'}, maxTraceLine+1)
	if _, err := w.Write(blob); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if buf.Len() != len(blob) {
		t.Errorf("buffered %d bytes, want the %d-byte literal flushed", buf.Len(), len(blob))
	}
}

func TestNewTraceWriterDisabledByDefault(t *testing.T) {
	t.Parallel()

	// Dial passes DialOptions.DebugWriter straight through, so a nil writer has
	// to stay nil rather than becoming a non-nil wrapper that go-imap would then
	// tee every byte into.
	if w := newTraceWriter(nil); w != nil {
		t.Errorf("newTraceWriter(nil) = %v, want nil", w)
	}
}

// TestTraceWriterRedactsInterleavedDirections covers the writer being fed both
// directions of the conversation. go-imap tees sent and received bytes into one
// debug writer, so a server response still missing its newline can have a client
// command appended to it. A tracer that decided "this line starts with * so it
// came from the server" would then print the password.
func TestTraceWriterRedactsInterleavedDirections(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newTraceWriter(&buf)

	// No newline after the server's greeting fragment.
	if _, err := w.Write([]byte("* OK ready ")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, err := w.Write([]byte("a001 LOGIN user hunter2\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	if got := buf.String(); strings.Contains(got, "hunter2") {
		t.Fatalf("password leaked when directions interleaved: %q", got)
	}
}

// TestTraceWriterIsConcurrencySafe guards the invariant that makes the tracer
// usable at all: go-imap writes into it from both the read goroutine and
// whichever goroutine issued the command.
func TestTraceWriterIsConcurrencySafe(t *testing.T) {
	t.Parallel()

	w := newTraceWriter(io.Discard)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				line := fmt.Sprintf("a%03d LOGIN user secret%d\r\n* OK partial", j, i)
				if _, err := w.Write([]byte(line)); err != nil {
					t.Errorf("Write() error = %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestASASLRefusalWithNoChallengeIsShown. Exchange Online refuses XOAUTH2 by
// going straight to a tagged NO -- it sends no challenge at all, so there is no
// client payload after the initial response. Arming the suppression on the
// AUTHENTICATE command instead of on the challenge swallowed that verdict and
// left a trace of an authentication failure with the failure removed.
func TestASASLRefusalWithNoChallengeIsShown(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newTraceWriter(&buf)

	const token = "ya29.a0AfB_super_secret_token"
	_, _ = w.Write([]byte("T2 AUTHENTICATE XOAUTH2 " + token + "\r\n"))
	_, _ = w.Write([]byte("T2 NO AUTHENTICATE failed.\r\n"))

	got := buf.String()
	if strings.Contains(got, token) {
		t.Fatalf("the inline token reached the trace:\n%s", got)
	}
	if !strings.Contains(got, "NO AUTHENTICATE failed.") {
		t.Errorf("the server's refusal is missing from the trace:\n%s", got)
	}
	if !strings.Contains(got, "AUTHENTICATE XOAUTH2") {
		t.Errorf("the trace lost the command that failed:\n%s", got)
	}
}

// TestAChallengedSASLPayloadIsStillHidden guards the other half: when the
// server does ask for more, the client's answer is a credential and must not
// appear. Moving the suppression to the challenge must not have opened this.
func TestAChallengedSASLPayloadIsStillHidden(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := newTraceWriter(&buf)

	const proof = "cj1mNzRhZDIwMSxwPXNlY3JldHByb29m"
	_, _ = w.Write([]byte("T2 AUTHENTICATE SCRAM-SHA-256 bixhPXVzZXI=\r\n"))
	_, _ = w.Write([]byte("+ cj1mNzRhZDIwMSxzPXNhbHQ=\r\n"))
	_, _ = w.Write([]byte(proof + "\r\n"))
	_, _ = w.Write([]byte("T2 OK authenticated\r\n"))

	got := buf.String()
	if strings.Contains(got, proof) {
		t.Fatalf("a challenged SASL payload reached the trace:\n%s", got)
	}
	if !strings.Contains(got, "T2 OK authenticated") {
		t.Errorf("the trace lost the tagged result:\n%s", got)
	}
}
