// Package selection decides which messages a run copies.
//
// Folder selection answers "which mailboxes"; this answers "which messages
// within them". The two are kept apart because they fail differently: leaving a
// folder out copies nothing, while getting a message predicate wrong can leave
// a mailbox looking complete when it is not.
//
// Every boundary here is imapsync's, read from its source rather than from its
// documentation, because a drop-in that is off by one message at the edge is a
// drop-in that quietly disagrees.
package selection

import (
	"fmt"
	"time"
)

// Filter is the message-level selection a run was asked for. The zero value
// selects everything.
type Filter struct {
	// MaxSize skips messages of this size or larger. Zero means no limit.
	MaxSize int64
	// MinSize skips messages of this size or smaller. Zero means no limit.
	//
	// Zero therefore cannot express imapsync's --minsize 0, which skips
	// zero-byte messages. That divergence is deliberate and costs nothing
	// real: a zero-byte message has no headers to identify it by and no body
	// to append, so it is refused further down whatever this says.
	MinSize int64

	// MaxAge skips messages older than this. Zero means no limit.
	MaxAge time.Duration
	// MinAge skips messages newer than this. Zero means no limit.
	MinAge time.Duration
}

// Day is imapsync's unit for --maxage and --minage.
const Day = 24 * time.Hour

// Active reports whether this filter excludes anything.
//
// Callers need this rather than comparing against the zero value because the
// answer changes their behaviour beyond the predicate: a run that filters must
// not record a folder as fully mirrored.
func (f Filter) Active() bool {
	return f.MaxSize > 0 || f.MinSize > 0 || f.MaxAge > 0 || f.MinAge > 0
}

// Validate reports a filter that can never select anything.
//
// A size window with no room in it is always a mistake, and one worth catching
// before a run reports having copied nothing and leaves the reader to work out
// why. The age options have no equivalent error: their reversed form is not a
// mistake but a documented feature, described in wantsAge.
func (f Filter) Validate() error {
	if f.MaxSize > 0 && f.MinSize > 0 && f.MinSize+1 >= f.MaxSize {
		return fmt.Errorf("no message can be both larger than %d bytes and smaller than %d", f.MinSize, f.MaxSize)
	}
	return nil
}

// Wants reports whether a message of this size and date is to be copied. now is
// taken from the caller so that a run measures every folder against one instant
// rather than against the clock as it moves through them.
func (f Filter) Wants(size int64, date, now time.Time) bool {
	return f.wantsSize(size) && f.wantsAge(date, now)
}

// wantsSize applies --maxsize and --minsize.
//
// Both bounds are exclusive of the message: imapsync skips a message "larger
// (or equal)" than maxsize and "smaller (or equal)" than minsize, so a message
// exactly on either bound is skipped. This is the opposite of the age bounds
// below, which include their edges. The asymmetry is imapsync's, and tidying it
// would break the drop-in for exactly the messages sitting on a round number.
func (f Filter) wantsSize(size int64) bool {
	if f.MaxSize > 0 && size >= f.MaxSize {
		return false
	}
	if f.MinSize > 0 && size <= f.MinSize {
		return false
	}
	return true
}

// wantsAge applies --maxage and --minage.
//
// --maxage keeps the recent end and --minage keeps the old end, which reads
// backwards until you say it as imapsync's help does: maxage is the greatest
// age a message may have, minage the least.
//
// Both bounds include their edge, unlike the size bounds. imapsync compares
// internal dates against a cutoff epoch with >= and <=, so a message exactly
// maxage old is kept.
//
// When both are given the connective depends on their order, and this is the
// part that surprises people — imapsync's own help calls it "magic":
//
//	--maxage 20 --minage 10   keeps messages 10 to 20 days old   (intersection)
//	--maxage 10 --minage 20   keeps messages 0 to 10 days old
//	                          and messages 20 days and older     (union)
//
// It is less arbitrary than it looks. Each option names a region to keep, and
// when the two regions overlap the sensible reading is the band they share;
// when they do not overlap an intersection would be empty, so the only thing
// the user can have meant is the two regions themselves. imapsync switches on
// minage <= maxage, and so does this.
func (f Filter) wantsAge(date, now time.Time) bool {
	if f.MaxAge == 0 && f.MinAge == 0 {
		return true
	}
	age := now.Sub(date)
	switch {
	case f.MinAge == 0:
		return age <= f.MaxAge
	case f.MaxAge == 0:
		return age >= f.MinAge
	case f.MinAge <= f.MaxAge:
		return age >= f.MinAge && age <= f.MaxAge
	default:
		return age >= f.MinAge || age <= f.MaxAge
	}
}

// CapSize lowers MaxSize to the destination's APPENDLIMIT.
//
// A server that reports APPENDLIMIT is saying it will reject anything larger,
// so a message above it cannot be copied by any means. Trying anyway spends a
// full fetch and a full append to earn a refusal, and records a failure that
// reads like a fault rather than like a rule.
//
// imapsync does the same, taking the smaller of --maxsize and APPENDLIMIT. The
// bound is exclusive here while APPENDLIMIT is inclusive — a message of exactly
// APPENDLIMIT bytes is within the limit — so the cap sits one byte above it.
func (f Filter) CapSize(appendLimit int64) Filter {
	if appendLimit <= 0 {
		return f
	}
	if f.MaxSize == 0 || appendLimit+1 < f.MaxSize {
		f.MaxSize = appendLimit + 1
	}
	return f
}
