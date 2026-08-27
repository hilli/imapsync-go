// Package ident derives a stable identity for a message from its headers.
//
// This is tiers 3 and 4 of the identity ladder. The UID map answers first and
// authoritatively; identities are what remain when it cannot — on a first sync,
// after a UIDVALIDITY change, after a lost database, and above all when a crash
// between APPEND and the state commit leaves a message that may or may not have
// reached the destination. Getting this wrong duplicates mail, which is the
// failure the whole tool exists to avoid.
package ident

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/textproto"
	"strings"
)

// StampHeader marks a copy whose source had no usable Message-ID, so it can be
// found again without the database.
//
// Adding an unsigned header does not invalidate DKIM: a signature covers a named
// list of headers, and one not on that list cannot affect verification.
const StampHeader = "X-Imapsync-Go-Id"

// Fields are the headers the digest covers.
//
// The list is deliberately short and made of headers that servers do not
// rewrite. Anything a destination might normalise — Received, Content-Type
// parameters, transfer encodings — would make the same message digest
// differently on the two sides, which reads as "not copied yet" and duplicates
// it on the next run.
var Fields = []string{"Message-ID", "Date", "Subject", "From", "To", "Cc"}

// The digest's framing bytes. Both are control characters that RFC 5322 does
// not permit in a header value, so a value can never contain one.
const (
	valueSep  = 1 // between repeats of the same field
	fieldTerm = 2 // after every field, present or absent
)

// Identity is what can be said about a message from its headers alone.
type Identity struct {
	// Digest is a hex SHA-256 over the normalised header fields.
	Digest string
	// MessageID is the raw Message-ID including angle brackets, or "" if the
	// message has none.
	MessageID string
	// Weak reports that too little header survived for the digest to tell this
	// message apart from another. Adoption must not act on a weak identity: a
	// wrong match silently drops a message instead of copying it.
	Weak bool
}

// NeedsStamp reports whether a copy of this message should carry a stamp header
// so it can be found again. Messages with a Message-ID are already findable.
func (i Identity) NeedsStamp() bool { return i.MessageID == "" }

// StampValue is the value to write into StampHeader for this message.
func (i Identity) StampValue() string { return i.Digest }

// Parse derives an identity from a raw header block, as returned by
// FETCH BODY.PEEK[HEADER.FIELDS (...)].
//
// Malformed headers are common in real mail and are not an error here: whatever
// parsed is used, because refusing to identify a message is worse than
// identifying it from less.
func Parse(header []byte) Identity {
	fields := readHeader(header)

	var sb strings.Builder
	var present int
	for _, name := range Fields {
		values := fields[textproto.CanonicalMIMEHeaderKey(name)]

		// Fields are written in the fixed order of Fields and terminated, so a
		// field is identified by its position and its name need not be written.
		// The separators are control bytes that RFC 5322 forbids in a header
		// value, so no arrangement of values can imitate a different one.
		for i, v := range values {
			if i > 0 {
				sb.WriteByte(valueSep)
			}
			sb.WriteString(normalise(v))
		}
		sb.WriteByte(fieldTerm)

		if len(values) > 0 && normalise(strings.Join(values, "")) != "" {
			present++
		}
	}

	sum := sha256.Sum256([]byte(sb.String()))
	id := Identity{
		Digest:    hex.EncodeToString(sum[:]),
		MessageID: normalise(firstValue(fields, "Message-ID")),
	}

	// One surviving field is not enough to distinguish two messages: a shared
	// Subject or a shared Date would collapse them onto the same identity.
	id.Weak = id.MessageID == "" && present < 2
	return id
}

// readHeader parses a header block, tolerating the malformation that real
// mailboxes contain.
//
// ReadMIMEHeader reports io.EOF for a block with no closing blank line, which is
// what a FETCH of selected fields returns from some servers, but it still
// returns every field it read. A malformed line is different: parsing stops
// there and every field after it is lost. Both are worth using what survived
// for, because refusing to identify a message is worse than identifying it from
// less.
func readHeader(header []byte) textproto.MIMEHeader {
	r := textproto.NewReader(bufio.NewReader(bytes.NewReader(header)))
	fields, err := r.ReadMIMEHeader()
	if err != nil && fields == nil {
		return textproto.MIMEHeader{}
	}
	return fields
}

func firstValue(fields textproto.MIMEHeader, name string) string {
	values := fields[textproto.CanonicalMIMEHeaderKey(name)]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// normalise collapses the whitespace differences that folding and re-folding
// introduce. A header refolded at a different column must digest the same, or
// the message looks new every time it crosses a server that refolds.
func normalise(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// SearchTerms returns the header field and value to search a destination for,
// and whether searching is worthwhile at all.
//
// A stamped copy is looked up by its stamp; otherwise by Message-ID. A weak
// identity with neither cannot be searched for safely, and saying so is better
// than a search that matches the wrong message.
func SearchTerms(id Identity, stamped bool) (field, value string, ok bool) {
	switch {
	case stamped:
		return StampHeader, id.StampValue(), true
	case id.MessageID != "":
		return "Message-ID", id.MessageID, true
	default:
		return "", "", false
	}
}

// StampBytes returns the header line to prepend to a message body.
//
// It goes at the very front so that no assumption about the existing header is
// needed: RFC 5322 gives header fields no required order.
func StampBytes(value string) []byte {
	return []byte(StampHeader + ": " + value + "\r\n")
}
