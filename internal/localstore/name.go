// Package localstore keeps mail in a directory tree and presents it as an
// IMAP connection.
//
// The store is a waypoint between IMAP servers rather than a format for other
// mail software to read: what it must do is hand every message back to an
// arbitrary server intact. See docs/plans/2026-08-30-local-store-design.md.
package localstore

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	// messageExt is what makes a stored message open on a double click.
	// Neither macOS nor Windows recognises a maildir name.
	messageExt = ".eml"

	// uidDigits pads UIDs so that lexical order — which is the order every
	// file browser shows — is UID order. Ten digits covers the 32-bit range.
	uidDigits = 10

	// shardSize is how many messages share a directory once a folder grows
	// past it.
	//
	// Finder enumerates and sorts a directory before it draws anything, which
	// it does not survive at 413,954 entries. APFS is unbothered either way,
	// so this is a limit on human patience rather than on the filesystem.
	shardSize = 10000

	// shardPrefix marks a directory as a shard rather than a subfolder. It is
	// outside the safe set, so a folder genuinely called "+0000010000" is
	// percent-encoded and cannot be mistaken for one.
	shardPrefix = '+'

	dbName  = ".imapsync-folder.db"
	tmpName = ".tmp"

	// dirPerm keeps the store to its owner. A message file arrives at 0600
	// from os.CreateTemp and keeps it across the rename, so a directory anyone
	// could list was the one part of the store that leaked. Maildir has used
	// 0700 for the same reason for thirty years.
	dirPerm = 0o700
)

// messageRel is where a message lives, relative to its folder.
//
// The layout is a pure function of the UID: flat below shardSize, sharded
// above it. Nothing is recorded and nothing is decided, which means no file
// ever moves once written — the property that keeps an incremental backup of
// the store proportional to the mail that actually arrived. A folder holding
// fewer than shardSize messages is entirely flat, so the common case is also
// the legible one.
func messageRel(uid uint32) string {
	name := messageName(uid)
	if uid < shardSize {
		return name
	}
	return shardName(uid) + "/" + name
}

func messageName(uid uint32) string {
	return fmt.Sprintf("%0*d%s", uidDigits, uid, messageExt)
}

// shardName is the directory holding uid, named for the first UID it can take
// so that the relationship to the messages inside it is visible.
func shardName(uid uint32) string {
	return fmt.Sprintf("%c%0*d", shardPrefix, uidDigits, (uid/shardSize)*shardSize)
}

// isShardDir reports whether a directory entry is a shard rather than a
// subfolder.
func isShardDir(name string) bool {
	if len(name) != uidDigits+1 || name[0] != shardPrefix {
		return false
	}
	_, err := strconv.ParseUint(name[1:], 10, 64)
	return err == nil
}

// parseMessageName recovers the UID from a stored message's name. A .eml whose
// name is not a UID is somebody's own file, copied in by hand; it is adopted
// rather than parsed.
func parseMessageName(name string) (uint32, bool) {
	digits, ok := strings.CutSuffix(name, messageExt)
	if !ok || len(digits) != uidDigits {
		return 0, false
	}
	uid, err := strconv.ParseUint(digits, 10, 32)
	if err != nil || uid == 0 {
		return 0, false
	}
	return uint32(uid), true
}

// isMessageName reports whether an entry is a message file at all, whatever
// its name.
func isMessageName(name string) bool {
	return strings.HasSuffix(name, messageExt)
}

// encodeSegment makes one level of a folder name safe to be a directory name.
//
// Non-ASCII bytes pass through, so "Slettet post" and "Arkiv" read as
// themselves. What must not pass through is anything that would change the
// shape of the tree — a separator, or a name that could be mistaken for a
// shard or hidden from the person the layout exists to serve.
func encodeSegment(s string) string {
	if s == "" {
		return "%00"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		leading := i == 0 && (c == shardPrefix || c == '.')
		if safeByte(c) && !leading {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func safeByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c >= 0x80: // UTF-8; keeps æøå legible
		return true
	case c == ' ', c == '.', c == '_', c == '-', c == '(', c == ')', c == ',', c == '\'', c == '!', c == '=':
		return true
	}
	return false
}

// decodeSegment reverses encodeSegment. It is a fallback: the folder's true
// name is kept in its database, because a filesystem may hand back a different
// Unicode normalisation than the one it was given.
func decodeSegment(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// Delim is the hierarchy separator the store reports. The store keeps the
// hierarchy as directories rather than as a character in a name, so a store
// filled from a server that separates with "/" restores to one that separates
// with "." without either of them knowing.
const Delim = "/"

// folderRel maps an IMAP folder name to a path relative to the store root.
func folderRel(name string) string {
	parts := strings.Split(name, Delim)
	encoded := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		encoded = append(encoded, encodeSegment(p))
	}
	return strings.Join(encoded, "/")
}

// folderName reverses folderRel for a path relative to the store root.
func folderName(rel string) string {
	parts := strings.Split(rel, "/")
	decoded := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		decoded = append(decoded, decodeSegment(p))
	}
	return strings.Join(decoded, Delim)
}
