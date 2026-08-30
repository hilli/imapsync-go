package localstore

import (
	"bytes"
	"fmt"
	"net/mail"
	"os"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

// evaluate applies a search key to a folder's messages.
//
// A local search is a scan, which on an IMAP server would be unthinkable and
// here is merely I/O. It is also rarely the expensive case: a criterion made
// only of flags, dates and sizes is answered entirely from the folder database
// without opening a single message, and those are the criteria --maxage,
// --minage and --maxsize produce.
func evaluate(c *imap.SearchCriteria, records []record, path func(uint32) string) ([]uint32, error) {
	if c == nil {
		out := make([]uint32, 0, len(records))
		for _, r := range records {
			out = append(out, r.uid)
		}
		return out, nil
	}

	var out []uint32
	for i, r := range records {
		m := &message{record: r, seq: uint32(i + 1), path: path(r.uid)}
		ok, err := matches(c, m)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, r.uid)
		}
	}
	return out, nil
}

// message is one candidate, which reads itself from disk only if the criteria
// ask something the folder database cannot answer.
type message struct {
	record
	seq  uint32
	path string

	loaded bool
	raw    []byte
	err    error

	headerParsed bool
	sent         time.Time
}

func (m *message) load() ([]byte, error) {
	if !m.loaded {
		m.loaded = true
		m.raw, m.err = os.ReadFile(m.path)
		if m.err != nil {
			m.err = fmt.Errorf("reading message %d: %w", m.uid, m.err)
		}
	}
	return m.raw, m.err
}

func (m *message) header() ([]byte, error) {
	raw, err := m.load()
	if err != nil {
		return nil, err
	}
	if i := endOfHeader(raw); i >= 0 {
		return raw[:i], nil
	}
	return raw, nil
}

func (m *message) body() ([]byte, error) {
	raw, err := m.load()
	if err != nil {
		return nil, err
	}
	if i := endOfHeader(raw); i >= 0 {
		return raw[i:], nil
	}
	return nil, nil
}

// sentDate is the Date header, which is what SENTSINCE and SENTBEFORE mean —
// as distinct from INTERNALDATE, which is when the message arrived.
func (m *message) sentDate() (time.Time, error) {
	if m.headerParsed {
		return m.sent, nil
	}
	m.headerParsed = true
	header, err := m.header()
	if err != nil {
		return time.Time{}, err
	}
	for _, v := range headerValues(header, "Date") {
		if t, err := mail.ParseDate(v); err == nil {
			m.sent = t
			break
		}
	}
	return m.sent, nil
}

// matches reports whether a message satisfies every key in a criteria.
//
// The keys are grouped by what they cost to answer, and asked in that order:
// numbers and flags come from the index, dates and sizes from what the scan
// already knew, and only the last group opens the file.
func matches(c *imap.SearchCriteria, m *message) (bool, error) {
	if !matchesIndex(c, m) {
		return false, nil
	}
	if ok, err := matchesContent(c, m); err != nil || !ok {
		return false, err
	}
	return matchesNested(c, m)
}

// matchesIndex answers the keys that need nothing but the row.
func matchesIndex(c *imap.SearchCriteria, m *message) bool {
	for _, set := range c.SeqNum {
		if !set.Contains(m.seq) {
			return false
		}
	}
	for _, set := range c.UID {
		if !set.Contains(imap.UID(m.uid)) {
			return false
		}
	}

	// IMAP compares dates by day, ignoring the time of day on both sides.
	if !c.Since.IsZero() && day(m.date).Before(day(c.Since)) {
		return false
	}
	if !c.Before.IsZero() && !day(m.date).Before(day(c.Before)) {
		return false
	}

	if c.Larger > 0 && m.size <= c.Larger {
		return false
	}
	if c.Smaller > 0 && m.size >= c.Smaller {
		return false
	}

	for _, f := range c.Flag {
		if !hasFlag(m.flags, string(f)) {
			return false
		}
	}
	for _, f := range c.NotFlag {
		if hasFlag(m.flags, string(f)) {
			return false
		}
	}
	return true
}

// matchesContent answers the keys that have to read the message.
func matchesContent(c *imap.SearchCriteria, m *message) (bool, error) {
	if !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		sent, err := m.sentDate()
		if err != nil {
			return false, err
		}
		if !c.SentSince.IsZero() && day(sent).Before(day(c.SentSince)) {
			return false, nil
		}
		if !c.SentBefore.IsZero() && !day(sent).Before(day(c.SentBefore)) {
			return false, nil
		}
	}

	for _, h := range c.Header {
		header, err := m.header()
		if err != nil {
			return false, err
		}
		if !headerContains(header, h.Key, h.Value) {
			return false, nil
		}
	}

	for _, needle := range c.Body {
		body, err := m.body()
		if err != nil {
			return false, err
		}
		if !containsFold(body, needle) {
			return false, nil
		}
	}
	for _, needle := range c.Text {
		raw, err := m.load()
		if err != nil {
			return false, err
		}
		if !containsFold(raw, needle) {
			return false, nil
		}
	}
	return true, nil
}

// matchesNested answers NOT and OR, each of which is a criteria of its own.
func matchesNested(c *imap.SearchCriteria, m *message) (bool, error) {
	for i := range c.Not {
		ok, err := matches(&c.Not[i], m)
		if err != nil {
			return false, err
		}
		if ok {
			return false, nil
		}
	}

	for _, pair := range c.Or {
		left, err := matches(&pair[0], m)
		if err != nil {
			return false, err
		}
		if left {
			continue
		}
		right, err := matches(&pair[1], m)
		if err != nil {
			return false, err
		}
		if !right {
			return false, nil
		}
	}

	return true, nil
}

func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

func containsFold(haystack []byte, needle string) bool {
	if needle == "" {
		return true
	}
	return bytes.Contains(bytes.ToLower(haystack), bytes.ToLower([]byte(needle)))
}
