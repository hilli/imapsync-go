package selection_test

import (
	"testing"
	"time"

	"github.com/hilli/imapsync-go/internal/selection"
)

// now is a fixed instant so that "ten days old" means one thing throughout.
var now = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// Wants takes a message's two dates, the Date: header and the internal date.
// Most tests here pass the same value for both, because they are about the
// bounds rather than about which date the bounds read; the ones that are about
// that pass dates which disagree, and say so in their names.

func daysOld(d float64) time.Time {
	return now.Add(-time.Duration(d * float64(selection.Day)))
}

func TestNothingIsExcludedWithoutAFilter(t *testing.T) {
	var f selection.Filter
	if f.Active() {
		t.Error("the zero filter reports itself active")
	}
	if !f.Wants(0, time.Time{}, time.Time{}, now) {
		t.Error("the zero filter excluded a message")
	}
	if !f.Wants(1<<40, daysOld(9999), daysOld(9999), now) {
		t.Error("the zero filter excluded a huge ancient message")
	}
}

// The size bounds exclude their own edge. imapsync skips a message "larger (or
// equal)" than maxsize, so 1000 bytes with --maxsize 1000 is skipped.
func TestASizeBoundExcludesAMessageSittingExactlyOnIt(t *testing.T) {
	max := selection.Filter{MaxSize: 1000}
	if !max.Wants(999, now, now, now) {
		t.Error("--maxsize 1000 skipped a 999-byte message")
	}
	if max.Wants(1000, now, now, now) {
		t.Error("--maxsize 1000 copied a 1000-byte message; imapsync skips larger *or equal*")
	}

	min := selection.Filter{MinSize: 1000}
	if !min.Wants(1001, now, now, now) {
		t.Error("--minsize 1000 skipped a 1001-byte message")
	}
	if min.Wants(1000, now, now, now) {
		t.Error("--minsize 1000 copied a 1000-byte message; imapsync skips smaller *or equal*")
	}
}

func TestASizeWindowKeepsWhatIsBetween(t *testing.T) {
	f := selection.Filter{MinSize: 100, MaxSize: 1000}
	for _, size := range []int64{101, 500, 999} {
		if !f.Wants(size, now, now, now) {
			t.Errorf("size %d was skipped, but it is inside the window", size)
		}
	}
	for _, size := range []int64{0, 100, 1000, 5000} {
		if f.Wants(size, now, now, now) {
			t.Errorf("size %d was copied, but it is outside the window", size)
		}
	}
}

// The age bounds include their own edge, unlike the size bounds. This is
// imapsync's asymmetry, not a tidy-up waiting to happen.
func TestAnAgeBoundIncludesAMessageSittingExactlyOnIt(t *testing.T) {
	max := selection.Filter{MaxAge: 10 * selection.Day}
	if !max.Wants(0, daysOld(10), daysOld(10), now) {
		t.Error("--maxage 10 skipped a message exactly 10 days old; imapsync keeps it")
	}
	if max.Wants(0, daysOld(10.5), daysOld(10.5), now) {
		t.Error("--maxage 10 copied a message older than 10 days")
	}
	if !max.Wants(0, daysOld(1), daysOld(1), now) {
		t.Error("--maxage 10 skipped a one-day-old message")
	}

	min := selection.Filter{MinAge: 10 * selection.Day}
	if !min.Wants(0, daysOld(10), daysOld(10), now) {
		t.Error("--minage 10 skipped a message exactly 10 days old; imapsync keeps it")
	}
	if min.Wants(0, daysOld(9.5), daysOld(9.5), now) {
		t.Error("--minage 10 copied a message newer than 10 days")
	}
	if !min.Wants(0, daysOld(90), daysOld(90), now) {
		t.Error("--minage 10 skipped a 90-day-old message")
	}
}

// The intersection case, from imapsync's own help: --maxage 20 --minage 10
// keeps messages 10 to 20 days old.
func TestOverlappingAgeBoundsKeepTheBandTheyShare(t *testing.T) {
	f := selection.Filter{MaxAge: 20 * selection.Day, MinAge: 10 * selection.Day}
	for _, age := range []float64{10, 15, 20} {
		if !f.Wants(0, daysOld(age), daysOld(age), now) {
			t.Errorf("%.0f days old was skipped, but --maxage 20 --minage 10 keeps 10 to 20", age)
		}
	}
	for _, age := range []float64{0, 5, 25, 400} {
		if f.Wants(0, daysOld(age), daysOld(age), now) {
			t.Errorf("%.0f days old was copied, but --maxage 20 --minage 10 keeps only 10 to 20", age)
		}
	}
}

// The union case, which imapsync's help labels "magic!": --maxage 10 --minage
// 20 keeps 0 to 10 days old *and* 20 days and older, excluding what is between.
//
// This is the test the whole drop-in claim rests on. An implementation that
// reasoned from the option names alone would take the intersection here, find
// it empty, and copy nothing at all.
func TestReversedAgeBoundsKeepBothEndsAndDropTheMiddle(t *testing.T) {
	f := selection.Filter{MaxAge: 10 * selection.Day, MinAge: 20 * selection.Day}
	for _, age := range []float64{0, 5, 10, 20, 30, 400} {
		if !f.Wants(0, daysOld(age), daysOld(age), now) {
			t.Errorf("%.0f days old was skipped, but --maxage 10 --minage 20 keeps both ends", age)
		}
	}
	for _, age := range []float64{11, 15, 19} {
		if f.Wants(0, daysOld(age), daysOld(age), now) {
			t.Errorf("%.0f days old was copied, but --maxage 10 --minage 20 excludes the middle", age)
		}
	}
}

// Equal bounds are the intersection branch, and select a single instant rather
// than nothing. imapsync switches on minage <= maxage, so the equal case is
// deliberately on the intersection side of the line.
func TestEqualAgeBoundsSelectTheInstantTheyShare(t *testing.T) {
	f := selection.Filter{MaxAge: 10 * selection.Day, MinAge: 10 * selection.Day}
	if !f.Wants(0, daysOld(10), daysOld(10), now) {
		t.Error("--maxage 10 --minage 10 skipped a message exactly 10 days old")
	}
	if f.Wants(0, daysOld(9), daysOld(9), now) || f.Wants(0, daysOld(11), daysOld(11), now) {
		t.Error("--maxage 10 --minage 10 kept something other than exactly 10 days old")
	}
}

func TestSizeAndAgeMustBothAgree(t *testing.T) {
	f := selection.Filter{MaxSize: 1000, MaxAge: 10 * selection.Day}
	if !f.Wants(500, daysOld(5), daysOld(5), now) {
		t.Error("a small recent message was skipped")
	}
	if f.Wants(5000, daysOld(5), daysOld(5), now) {
		t.Error("a recent message was copied despite being too large")
	}
	if f.Wants(500, daysOld(50), daysOld(50), now) {
		t.Error("a small message was copied despite being too old")
	}
}

func TestAFilterKnowsWhetherItExcludesAnything(t *testing.T) {
	for _, f := range []selection.Filter{
		{MaxSize: 1},
		{MinSize: 1},
		{MaxAge: time.Second},
		{MinAge: time.Second},
	} {
		if !f.Active() {
			t.Errorf("%+v reports itself inactive", f)
		}
	}
}

func TestASizeWindowWithNoRoomInItIsRefused(t *testing.T) {
	// Nothing is both larger than 1000 and smaller than 1000.
	if err := (selection.Filter{MinSize: 1000, MaxSize: 1000}).Validate(); err == nil {
		t.Error("an empty size window was accepted")
	}
	// Nor larger than 1000 and smaller than 1001: that leaves no integer.
	if err := (selection.Filter{MinSize: 1000, MaxSize: 1001}).Validate(); err == nil {
		t.Error("a size window containing no possible size was accepted")
	}
	// 1002 leaves exactly one: a message of 1001 bytes.
	f := selection.Filter{MinSize: 1000, MaxSize: 1002}
	if err := f.Validate(); err != nil {
		t.Errorf("a window containing one size was refused: %v", err)
	}
	if !f.Wants(1001, now, now, now) {
		t.Error("the one size the window admits was skipped")
	}
}

// Reversed age bounds must not be refused: they are the union case, which is a
// documented feature rather than a mistake.
func TestReversedAgeBoundsAreNotAnError(t *testing.T) {
	if err := (selection.Filter{MaxAge: 10 * selection.Day, MinAge: 20 * selection.Day}).Validate(); err != nil {
		t.Errorf("the documented union form was refused: %v", err)
	}
}

func TestAppendLimitBecomesASizeBound(t *testing.T) {
	// A message of exactly APPENDLIMIT bytes is within the limit and must
	// still be copied; the bound sits one byte above.
	f := selection.Filter{}.CapSize(1000)
	if !f.Wants(1000, now, now, now) {
		t.Error("a message of exactly APPENDLIMIT bytes was skipped; the limit is inclusive")
	}
	if f.Wants(1001, now, now, now) {
		t.Error("a message above APPENDLIMIT was copied")
	}
	if !f.Active() {
		t.Error("a filter carrying an APPENDLIMIT reports itself inactive")
	}
}

func TestAppendLimitAndMaxSizeTakeWhicheverIsStricter(t *testing.T) {
	// APPENDLIMIT lower than --maxsize: APPENDLIMIT wins.
	f := selection.Filter{MaxSize: 5000}.CapSize(1000)
	if f.Wants(2000, now, now, now) {
		t.Error("APPENDLIMIT 1000 did not lower --maxsize 5000")
	}

	// --maxsize lower than APPENDLIMIT: --maxsize stands.
	f = selection.Filter{MaxSize: 500}.CapSize(1000)
	if f.MaxSize != 500 {
		t.Errorf("APPENDLIMIT 1000 raised --maxsize 500 to %d", f.MaxSize)
	}
	if f.Wants(600, now, now, now) {
		t.Error("APPENDLIMIT loosened a stricter --maxsize")
	}
}

func TestAServerThatReportsNoAppendLimitChangesNothing(t *testing.T) {
	f := selection.Filter{MaxSize: 500}
	if got := f.CapSize(0); got != f {
		t.Errorf("an unreported APPENDLIMIT changed the filter to %+v", got)
	}
	if (selection.Filter{}).CapSize(0).Active() {
		t.Error("an unreported APPENDLIMIT made an empty filter active")
	}
}

// The default basis is the Date: header, not the internal date.
//
// This is imapsync's default and the reverse of the obvious guess: its
// --noabletosearch option is documented as making --maxage and --minage "use
// the internal dates given by a FETCH imap command instead of the Date: header",
// so searching for the header is what it does unless told otherwise.
func TestAgeIsMeasuredFromTheSentDateByDefault(t *testing.T) {
	f := selection.Filter{MaxAge: 30 * selection.Day}

	// An old message that arrived recently: forwarded, restored from a backup,
	// or migrated by a tool that did not preserve internal dates.
	if f.Wants(0, daysOld(200), daysOld(1), now) {
		t.Error("--maxage 30 copied a message sent 200 days ago; the default basis is the Date: header")
	}
	// A recent message with an old internal date cannot arise honestly, but it
	// pins the direction of the choice.
	if !f.Wants(0, daysOld(1), daysOld(200), now) {
		t.Error("--maxage 30 skipped a message sent yesterday")
	}
}

func TestTheInternalBasisMeasuresArrivalInstead(t *testing.T) {
	f := selection.Filter{MaxAge: 30 * selection.Day, Basis: selection.BasisInternal}

	if !f.Wants(0, daysOld(200), daysOld(1), now) {
		t.Error("--age-basis internal skipped a message that arrived yesterday")
	}
	if f.Wants(0, daysOld(1), daysOld(200), now) {
		t.Error("--age-basis internal copied a message that arrived 200 days ago")
	}
}

// A message with no usable Date: header is judged by its internal date rather
// than excluded.
//
// Both alternatives lose mail silently. Treating an undated message as
// infinitely old hides it from --max-age for ever; treating it as brand new
// hides it from --min-age for ever. Drafts and script-generated mail routinely
// have no Date:, so this is not a rare corner.
func TestAMessageWithNoSentDateFallsBackToItsArrival(t *testing.T) {
	f := selection.Filter{MaxAge: 30 * selection.Day}

	if !f.Wants(0, time.Time{}, daysOld(1), now) {
		t.Error("--maxage 30 skipped an undated message that arrived yesterday")
	}
	if f.Wants(0, time.Time{}, daysOld(200), now) {
		t.Error("--maxage 30 copied an undated message that arrived 200 days ago")
	}
}

// NeedsSentDate exists so a dry run knows whether to pay for a header it would
// otherwise not fetch. Claiming to need it when it does not costs a fetch of
// every message being previewed.
func TestOnlyAnAgeBoundOnTheSentDateNeedsTheHeader(t *testing.T) {
	if (selection.Filter{}).NeedsSentDate() {
		t.Error("the zero filter asked for the Date: header")
	}
	if (selection.Filter{MaxSize: 1000}).NeedsSentDate() {
		t.Error("a size-only filter asked for the Date: header")
	}
	if (selection.Filter{MaxAge: selection.Day, Basis: selection.BasisInternal}).NeedsSentDate() {
		t.Error("an internal-basis age filter asked for the Date: header")
	}
	if !(selection.Filter{MaxAge: selection.Day}).NeedsSentDate() {
		t.Error("--max-age on the default basis did not ask for the Date: header")
	}
	if !(selection.Filter{MinAge: selection.Day}).NeedsSentDate() {
		t.Error("--min-age on the default basis did not ask for the Date: header")
	}
}
