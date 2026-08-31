package imapx

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// maxTraceLine bounds how much unterminated input the tracer holds while
// waiting for a newline. Message literals arrive as multi-megabyte blobs with
// no line structure, and buffering one whole to inspect it for credentials it
// cannot contain would be absurd.
const maxTraceLine = 8 << 10

// newTraceWriter wraps w so credentials never reach it, returning nil for a nil
// writer so tracing stays off by default.
//
// go-imap tees both directions of the connection verbatim into its debug
// writer, which includes the LOGIN command and any SASL exchange. Tracing a
// real migration is the only practical way to diagnose server quirks, but a
// trace that writes a password to a terminal or a log file is a liability, so
// redaction happens here rather than at each call site.
func newTraceWriter(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return &traceWriter{w: w}
}

type traceWriter struct {
	// mu guards everything below. go-imap writes sent and received bytes into
	// the same debug writer from different goroutines, so an unsynchronised
	// buffer here is both a data race and a crash.
	mu  sync.Mutex
	w   io.Writer
	buf []byte

	// authInFlight marks that an AUTHENTICATE command has been sent and its
	// tagged response has not arrived yet.
	authInFlight bool

	// pendingSASL marks that the next client line carries a SASL payload.
	pendingSASL bool
}

// Write forwards complete lines, redacted. Errors from the underlying writer
// are deliberately swallowed: go-imap sends the debug writer through an
// io.MultiWriter alongside the socket, so returning an error here would abort
// the connection itself. A failing trace destination must not be able to kill a
// migration in progress.
func (t *traceWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)

	for {
		i := bytes.IndexByte(t.buf, '\n')
		if i < 0 {
			break
		}
		t.emit(t.buf[:i+1])
		t.buf = t.buf[i+1:]
	}

	if len(t.buf) >= maxTraceLine {
		t.emit(t.buf)
		t.buf = t.buf[:0]
	}
	return len(p), nil
}

func (t *traceWriter) emit(line []byte) {
	body, eol := splitEOL(line)

	redacted := t.redact(body)
	if redacted == body {
		_, _ = t.w.Write(line)
		return
	}
	_, _ = t.w.Write([]byte(redacted + eol))
}

// redact rewrites one protocol line, advancing the SASL state machine.
//
// It deliberately does not trust a line's leading character to tell it who
// spoke. Both directions share one writer, so a server response still missing
// its newline can have a client command appended to it; a rule that skipped
// anything starting with "*" would then wave a LOGIN command straight through.
// Mistaking server chatter for a command costs a mangled line in a diagnostic
// dump, while the reverse mistake costs a password.
func (t *traceWriter) redact(line string) string {
	if strings.TrimSpace(line) != "" {
		switch {
		// Deliberately not excluding a continuation here. Once the payload
		// is expected, hiding it matters more than showing the server line it
		// might have been concatenated with -- and a concatenated "+ " and
		// client response is precisely the case the paranoia above is for.
		case t.pendingSASL:
			t.pendingSASL, t.authInFlight = false, false
			return "<redacted SASL payload>"

		case t.authInFlight && isContinuation(line):
			// The server is asking for the next SASL payload, so the client's
			// following line is the thing to hide.
			t.pendingSASL = true

		case t.authInFlight:
			// Neither a challenge nor a client payload: the exchange is over
			// and this is the server's tagged verdict. Showing it is the
			// point of tracing an authentication that failed.
			t.authInFlight = false
		}
	}

	fields := strings.Fields(line)
	for i, f := range fields {
		switch {
		case strings.EqualFold(f, "LOGIN"):
			// tag LOGIN user password: keep the user, drop the rest. A LOGIN
			// with nothing after the username carries no secret.
			if i+2 >= len(fields) {
				continue
			}
			return strings.Join(fields[:i+2], " ") + " <redacted>"

		case strings.EqualFold(f, "AUTHENTICATE"):
			// tag AUTHENTICATE mechanism [initial-response]: keep the
			// mechanism and drop any inline response.
			//
			// The suppression is armed by the server's continuation rather
			// than here, because that is the only thing a client SASL payload
			// ever follows. Arming here hid the server's tagged refusal
			// whenever it declined without asking anything -- which is what
			// Exchange Online does, so the one line explaining the failure was
			// the one line the trace withheld.
			t.authInFlight = true
			if i+1 >= len(fields) {
				continue
			}
			out := strings.Join(fields[:i+2], " ")
			if len(fields) > i+2 {
				out += " <redacted>"
			}
			return out
		}
	}
	return line
}

// isContinuation reports whether a line is the server's bare "+" prompt, which
// carries no credentials and must not consume the pending SASL suppression.
func isContinuation(line string) bool {
	return line == "+" || strings.HasPrefix(line, "+ ")
}

// splitEOL separates a line from its trailing CRLF, LF, or nothing at all.
func splitEOL(line []byte) (body, eol string) {
	s := string(line)
	switch {
	case strings.HasSuffix(s, "\r\n"):
		return s[:len(s)-2], "\r\n"
	case strings.HasSuffix(s, "\n"):
		return s[:len(s)-1], "\n"
	default:
		return s, ""
	}
}
