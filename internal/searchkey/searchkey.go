// Package searchkey parses an IMAP SEARCH key into the structured form the
// client library sends.
//
// imapsync's --search options take a raw IMAP search string and hand it to the
// server unread. This tool cannot do that: go-imap builds SEARCH from a
// structured value and keeps its command encoder unexported, so the string has
// to be understood here before it can be sent.
//
// That is a cost, and it buys two things. A string passed through unread is
// diagnosed only by the far server, in whatever terms that server chooses,
// after the run has connected and started; a string parsed here is diagnosed
// before the first connection is made, in terms that name what was typed. And
// the few keys this tool cannot express honestly are refused rather than sent
// in a form that means something else — see RECENT below.
//
// The grammar is RFC 3501's search-key production. Where this parser is
// narrower than the RFC it says so at the point of refusal, rather than
// accepting the input and quietly searching for something else.
package searchkey

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
)

// dateLayout is the IMAP date format, "1-Feb-2020". It is the layout go-imap
// formats with, so a date parsed here reaches the server as it was written.
const dateLayout = "2-Jan-2006"

// Key is a parsed IMAP search key, ready to send. The zero value is no search
// at all, which is how a run given no search option says so.
//
// The original text is kept because every message about a search reads better
// for quoting it: "the search matched no messages" is a puzzle, and "SEEN
// UNDELETED matched no messages" is an answer.
type Key struct {
	criteria *imap.SearchCriteria
	text     string
}

// IsZero reports whether this is the absence of a search rather than a search.
func (k Key) IsZero() bool { return k.criteria == nil }

// String is the search as it was written.
func (k Key) String() string { return k.text }

// Criteria is the parsed form, for the connection to send. It is nil for the
// zero Key, which no caller should be sending.
func (k Key) Criteria() *imap.SearchCriteria { return k.criteria }

// Parse reads an IMAP search key.
//
// Several keys in a row are ANDed, as IMAP defines them to be, so
// "SEEN SMALLER 10000" means both conditions and not either.
func Parse(s string) (Key, error) {
	toks, err := lex(s)
	if err != nil {
		return Key{}, err
	}
	p := &parser{toks: toks}
	criteria, n, err := p.keys()
	if err != nil {
		return Key{}, err
	}
	if n == 0 {
		return Key{}, errors.New("the search is empty; give a search key such as UNSEEN, or leave the option out")
	}
	if t, ok := p.peek(); ok {
		return Key{}, fmt.Errorf("unmatched %q in the search", t.text)
	}
	return Key{criteria: &criteria, text: strings.TrimSpace(s)}, nil
}

type tokenKind int

const (
	tokAtom tokenKind = iota
	tokQuoted
	tokOpen
	tokClose
)

type token struct {
	kind tokenKind
	text string
}

// lex splits a search key into atoms, quoted strings and parentheses.
//
// It is deliberately looser than RFC 3501's atom, which excludes "*" and "%"
// among others. Sequence sets need "*", real servers accept far more than the
// grammar allows, and rejecting an atom here that the parser would reject
// anyway only moves the error message somewhere less useful.
func lex(s string) ([]token, error) {
	var toks []token
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case ' ', '\t', '\r', '\n':
			i++
		case '(':
			toks = append(toks, token{tokOpen, "("})
			i++
		case ')':
			toks = append(toks, token{tokClose, ")"})
			i++
		case '"':
			text, next, err := quoted(s, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, token{tokQuoted, text})
			i = next
		case '{':
			// An IMAP literal is {n}, a line break, and n bytes on the wire.
			// There is no wire here, only an argument, so the bytes it
			// promises can never arrive.
			return nil, errors.New(`literals like {5} cannot be used in a search given on the command line; quote the value instead`)
		default:
			j := i
			for j < len(s) && !strings.ContainsRune(" \t\r\n()\"", rune(s[j])) {
				j++
			}
			toks = append(toks, token{tokAtom, s[i:j]})
			i = j
		}
	}
	return toks, nil
}

// quoted reads a quoted string starting at its opening quote.
func quoted(s string, start int) (text string, next int, err error) {
	var b strings.Builder
	for i := start + 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			// IMAP allows only \" and \\, but anything else escaped is
			// someone meaning the character itself, and refusing that would
			// be pedantry.
			if i+1 >= len(s) {
				return "", 0, errors.New("the search ends with a backslash, which escapes nothing")
			}
			i++
			b.WriteByte(s[i])
		case '"':
			return b.String(), i + 1, nil
		default:
			b.WriteByte(s[i])
		}
	}
	return "", 0, fmt.Errorf("unterminated quoted string in the search, starting at character %d", start+1)
}

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() (token, bool) {
	if p.pos >= len(p.toks) {
		return token{}, false
	}
	return p.toks[p.pos], true
}

func (p *parser) next() (token, bool) {
	t, ok := p.peek()
	if ok {
		p.pos++
	}
	return t, ok
}

// keys parses a run of search keys and ANDs them, stopping at ")" or the end.
//
// The count comes back separately because an empty run is an error whose
// wording depends on where it happened: an empty search and an empty group are
// different mistakes.
func (p *parser) keys() (imap.SearchCriteria, int, error) {
	var out imap.SearchCriteria
	n := 0
	for {
		t, ok := p.peek()
		if !ok || t.kind == tokClose {
			return out, n, nil
		}
		c, err := p.key()
		if err != nil {
			return out, n, err
		}
		and(&out, c)
		n++
	}
}

// and intersects two criteria, because several search keys in a row mean all
// of them.
//
// go-imap has its own And and it cannot be used here. It intersects the size
// bounds with
//
//	if criteria.Smaller == 0 || other.Smaller < criteria.Smaller
//
// and zero is its own spelling of "no bound", so ANDing "SMALLER 1000" with
// any later key — which has no SMALLER of its own, and therefore zero —
// satisfies the second test and erases the bound. "SMALLER 1000 UNSEEN" would
// come out as UNSEEN alone and copy every message however large. Larger
// escapes the same fault only by accident of its comparison running the other
// way, and the date bounds escape it because those are intersected by a helper
// that checks for the zero value first.
//
// The failure is silent and it widens the search, which is the worse of the
// two directions for a tool that deletes.
func and(dst *imap.SearchCriteria, src imap.SearchCriteria) {
	dst.SeqNum = append(dst.SeqNum, src.SeqNum...)
	dst.UID = append(dst.UID, src.UID...)

	dst.Since = later(dst.Since, src.Since)
	dst.SentSince = later(dst.SentSince, src.SentSince)
	dst.Before = earlier(dst.Before, src.Before)
	dst.SentBefore = earlier(dst.SentBefore, src.SentBefore)

	dst.Header = append(dst.Header, src.Header...)
	dst.Body = append(dst.Body, src.Body...)
	dst.Text = append(dst.Text, src.Text...)
	dst.Flag = append(dst.Flag, src.Flag...)
	dst.NotFlag = append(dst.NotFlag, src.NotFlag...)

	if src.Larger > dst.Larger {
		dst.Larger = src.Larger
	}
	if src.Smaller > 0 && (dst.Smaller == 0 || src.Smaller < dst.Smaller) {
		dst.Smaller = src.Smaller
	}

	dst.Not = append(dst.Not, src.Not...)
	dst.Or = append(dst.Or, src.Or...)
}

// later is the intersection of two "on or after" bounds, treating the zero
// time as no bound.
func later(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.After(a)) {
		return b
	}
	return a
}

// earlier is the intersection of two "before" bounds, treating the zero time
// as no bound.
func earlier(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

func (p *parser) key() (imap.SearchCriteria, error) {
	var c imap.SearchCriteria

	t, ok := p.next()
	if !ok {
		return c, errors.New("the search ends where a search key was expected")
	}

	switch t.kind {
	case tokOpen:
		return p.group()
	case tokClose:
		return c, errors.New(`unmatched ")" in the search`)
	case tokQuoted:
		return c, fmt.Errorf("%q is quoted where a search key was expected; search keys are bare words such as SEEN or SUBJECT", t.text)
	}

	name := strings.ToUpper(t.text)

	if done, err := p.flagKey(&c, name); done || err != nil {
		return c, err
	}
	if done, err := p.textKey(&c, name); done || err != nil {
		return c, err
	}
	if done, err := p.boundKey(&c, name); done || err != nil {
		return c, err
	}

	switch name {
	case "ALL":
		// Nothing to set: an empty criteria goes out as ALL.
		return c, nil

	case "NOT":
		inner, err := p.key()
		if err != nil {
			return c, fmt.Errorf("NOT needs a search key after it: %w", err)
		}
		c.Not = []imap.SearchCriteria{inner}
	case "OR":
		left, err := p.key()
		if err != nil {
			return c, fmt.Errorf("OR needs two search keys: %w", err)
		}
		right, err := p.key()
		if err != nil {
			return c, fmt.Errorf("OR needs two search keys: %w", err)
		}
		c.Or = [][2]imap.SearchCriteria{{left, right}}

	case "RECENT", "NEW", "OLD":
		// \Recent is not a property of a message but of a session: it means
		// "arrived since some other client last looked", and the server clears
		// it for whichever client gets there first. Two runs of this tool
		// against one mailbox would disagree, and the second would be wrong.
		//
		// It is refused rather than approximated because the library has no
		// encoding for it — \Recent is not among the flags it renders as
		// search keys, so it would go out as KEYWORD \Recent, which is a
		// syntax error at best and a search for a keyword no message carries
		// at worst. IMAP4rev2 removed \Recent for much the same reasons.
		return c, fmt.Errorf(`%s cannot be used: it depends on \Recent, which means "arrived since another client last looked" and so answers differently on every run`, name)

	default:
		if isNumericSet(t.text) {
			// A bare set in a search key is sequence numbers, which are
			// positions in the mailbox rather than names for messages: they
			// shift whenever anything is expunged, so a set that meant the
			// right messages when the run was typed may mean different ones by
			// the time the search reaches the server.
			return c, fmt.Errorf("%q is a sequence-number set, which names positions rather than messages; write \"UID %s\" if you meant those UIDs", t.text, t.text)
		}
		return c, fmt.Errorf("%q is not an IMAP search key", t.text)
	}
	return c, nil
}

// flagKey handles the search keys that test a flag, and reports whether name
// was one of them.
//
// Split out of key for no better reason than that one switch over thirty keys
// counts as complex however plainly it reads. The grouping is at least real:
// these are the keys whose argument is a flag, or which have no argument.
func (p *parser) flagKey(c *imap.SearchCriteria, name string) (bool, error) {
	switch name {
	case "ANSWERED", "DELETED", "DRAFT", "FLAGGED", "SEEN":
		c.Flag = []imap.Flag{systemFlag(name)}
	case "UNANSWERED", "UNDELETED", "UNDRAFT", "UNFLAGGED", "UNSEEN":
		c.NotFlag = []imap.Flag{systemFlag(strings.TrimPrefix(name, "UN"))}
	case "KEYWORD", "UNKEYWORD":
		v, err := p.value(name)
		if err != nil {
			return true, err
		}
		if name == "KEYWORD" {
			c.Flag = []imap.Flag{imap.Flag(v)}
		} else {
			c.NotFlag = []imap.Flag{imap.Flag(v)}
		}
	default:
		return false, nil
	}
	return true, nil
}

// textKey handles the search keys whose argument is a string to look for, and
// reports whether name was one of them.
func (p *parser) textKey(c *imap.SearchCriteria, name string) (bool, error) {
	switch name {
	case "BCC", "CC", "FROM", "SUBJECT", "TO":
		v, err := p.value(name)
		if err != nil {
			return true, err
		}
		c.Header = []imap.SearchCriteriaHeaderField{{Key: name, Value: v}}
	case "HEADER":
		field, err := p.value("HEADER")
		if err != nil {
			return true, err
		}
		v, err := p.value("HEADER " + field)
		if err != nil {
			return true, err
		}
		c.Header = []imap.SearchCriteriaHeaderField{{Key: field, Value: v}}
	case "BODY":
		v, err := p.value(name)
		if err != nil {
			return true, err
		}
		c.Body = []string{v}
	case "TEXT":
		v, err := p.value(name)
		if err != nil {
			return true, err
		}
		c.Text = []string{v}
	default:
		return false, nil
	}
	return true, nil
}

// group reads the rest of a parenthesised list, the opening ( having already
// been taken.
func (p *parser) group() (imap.SearchCriteria, error) {
	inner, n, err := p.keys()
	if err != nil {
		return inner, err
	}
	if _, ok := p.next(); !ok {
		return inner, errors.New("a ( in the search is never closed")
	}
	if n == 0 {
		return inner, errors.New("() in the search contains no search key")
	}
	return inner, nil
}

// boundKey handles the search keys whose argument is a date, a size or a set
// of UIDs, and reports whether name was one of them.
func (p *parser) boundKey(c *imap.SearchCriteria, name string) (bool, error) {
	switch name {
	case "SINCE", "BEFORE", "ON", "SENTSINCE", "SENTBEFORE", "SENTON":
		d, err := p.date(name)
		if err != nil {
			return true, err
		}
		setDate(c, name, d)
	case "LARGER", "SMALLER":
		n, err := p.size(name)
		if err != nil {
			return true, err
		}
		if name == "LARGER" {
			c.Larger = n
		} else {
			c.Smaller = n
		}
	case "UID":
		set, err := p.uidSet()
		if err != nil {
			return true, err
		}
		c.UID = []imap.UIDSet{set}
	default:
		return false, nil
	}
	return true, nil
}

// systemFlag is the flag each flag-naming search key tests.
//
// Spelled out rather than derived from the key, because the two differ in more
// than case — \Answered against ANSWERED is a coincidence that stops holding
// the moment anything is added here.
func systemFlag(name string) imap.Flag {
	switch name {
	case "ANSWERED":
		return imap.FlagAnswered
	case "DELETED":
		return imap.FlagDeleted
	case "DRAFT":
		return imap.FlagDraft
	case "FLAGGED":
		return imap.FlagFlagged
	case "SEEN":
		return imap.FlagSeen
	}
	panic("searchkey: no system flag for " + name)
}

// setDate applies one of the six date keys.
//
// ON and SENTON have no field of their own. They are a half-open day, and
// go-imap encodes exactly that back into ON when the two bounds are a day
// apart — so setting both is not a workaround but the representation.
func setDate(c *imap.SearchCriteria, name string, d time.Time) {
	switch name {
	case "SINCE":
		c.Since = d
	case "BEFORE":
		c.Before = d
	case "ON":
		c.Since, c.Before = d, d.AddDate(0, 0, 1)
	case "SENTSINCE":
		c.SentSince = d
	case "SENTBEFORE":
		c.SentBefore = d
	case "SENTON":
		c.SentSince, c.SentBefore = d, d.AddDate(0, 0, 1)
	}
}

// value reads the argument of a key that takes a string.
func (p *parser) value(key string) (string, error) {
	t, ok := p.next()
	if !ok {
		return "", fmt.Errorf("%s in the search has nothing after it to search for", key)
	}
	if t.kind == tokOpen || t.kind == tokClose {
		return "", fmt.Errorf("%s in the search is followed by %q rather than a value", key, t.text)
	}
	return t.text, nil
}

func (p *parser) date(key string) (time.Time, error) {
	v, err := p.value(key)
	if err != nil {
		return time.Time{}, err
	}
	d, err := time.Parse(dateLayout, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s in the search needs a date like 1-Feb-2020, not %q", key, v)
	}
	return d, nil
}

func (p *parser) size(key string) (int64, error) {
	v, err := p.value(key)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s in the search needs a number of bytes, not %q", key, v)
	}
	if n == 0 {
		// go-imap reads a bound of zero as no bound, so this key would be
		// dropped on the way to the wire and the search would match more than
		// it was asked for — for SMALLER 0, which matches nothing, that is the
		// entire mailbox instead. A silent inversion is worse than a refusal.
		return 0, fmt.Errorf("%s 0 in the search would be dropped rather than sent, leaving a search that matches everything; give a non-zero size", key)
	}
	return int64(n), nil
}

// uidSet reads the argument of UID: a comma-separated list of UIDs and ranges,
// in which "*" is the highest UID the mailbox holds.
func (p *parser) uidSet() (imap.UIDSet, error) {
	v, err := p.value("UID")
	if err != nil {
		return nil, err
	}
	var set imap.UIDSet
	for _, part := range strings.Split(v, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		start, err := uidNum(lo, v)
		if err != nil {
			return nil, err
		}
		if !isRange {
			set.AddNum(start)
			continue
		}
		stop, err := uidNum(hi, v)
		if err != nil {
			return nil, err
		}
		set.AddRange(start, stop)
	}
	return set, nil
}

// uidNum reads one UID, with "*" meaning the highest in the mailbox — which
// go-imap, like the protocol's own encoding, spells as zero.
func uidNum(s, whole string) (imap.UID, error) {
	if s == "*" {
		return 0, nil
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("UID %q in the search is not a set of UIDs; expected something like 1:100 or 5,9,12:*", whole)
	}
	return imap.UID(n), nil
}

// isNumericSet reports whether an atom looks like a sequence set, so a bare one
// can be refused by name rather than as an unknown key.
func isNumericSet(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(r rune) bool {
		return (r < '0' || r > '9') && r != ':' && r != ',' && r != '*'
	}) < 0
}
