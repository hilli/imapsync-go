package searchkey_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/searchkey"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// parse fails the test rather than returning an error, for the many cases that
// are about what a valid search means rather than about what is rejected.
func parse(t *testing.T, s string) imap.SearchCriteria {
	t.Helper()
	k, err := searchkey.Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", s, err)
	}
	return *k.Criteria()
}

// wants compares the whole parsed criteria rather than the one field under
// test, so that a key which sets something extra by accident is caught. A
// parser that quietly adds a bound would otherwise pass every test that only
// looked where it expected the answer.
func wants(t *testing.T, in string, want imap.SearchCriteria) {
	t.Helper()
	if got := parse(t, in); !reflect.DeepEqual(got, want) {
		t.Errorf("Parse(%q) =\n  %+v\nwant\n  %+v", in, got, want)
	}
}

func refuses(t *testing.T, in, mustMention string) {
	t.Helper()
	k, err := searchkey.Parse(in)
	if err == nil {
		t.Fatalf("Parse(%q) = %v, want an error", in, k)
	}
	if !strings.Contains(err.Error(), mustMention) {
		t.Errorf("Parse(%q) error = %q, want it to mention %q", in, err, mustMention)
	}
}

func TestTheZeroKeyIsTheAbsenceOfASearch(t *testing.T) {
	var k searchkey.Key
	if !k.IsZero() {
		t.Error("the zero Key does not report itself as no search")
	}
	if k.Criteria() != nil {
		t.Error("the zero Key has criteria to send")
	}

	parsed, err := searchkey.Parse("ALL")
	if err != nil {
		t.Fatalf("Parse(ALL) error = %v", err)
	}
	if parsed.IsZero() {
		t.Error("a parsed search reports itself as no search")
	}
}

// ALL is the identity, and it must parse to an empty criteria rather than to
// something: go-imap encodes an empty criteria as ALL, so the round trip only
// holds if nothing is set here.
func TestAllSetsNothing(t *testing.T) {
	wants(t, "ALL", imap.SearchCriteria{})
}

func TestSearchKeysAreCaseInsensitive(t *testing.T) {
	upper := parse(t, "UNSEEN SMALLER 1000")
	for _, spelling := range []string{"unseen smaller 1000", "UnSeen SmAlLeR 1000"} {
		if got := parse(t, spelling); !reflect.DeepEqual(got, upper) {
			t.Errorf("Parse(%q) = %+v, want the same as the upper-case spelling %+v", spelling, got, upper)
		}
	}
}

func TestEachSystemFlagKeyTestsItsOwnFlag(t *testing.T) {
	for in, want := range map[string]imap.Flag{
		"ANSWERED": imap.FlagAnswered,
		"DELETED":  imap.FlagDeleted,
		"DRAFT":    imap.FlagDraft,
		"FLAGGED":  imap.FlagFlagged,
		"SEEN":     imap.FlagSeen,
	} {
		wants(t, in, imap.SearchCriteria{Flag: []imap.Flag{want}})
		wants(t, "UN"+in, imap.SearchCriteria{NotFlag: []imap.Flag{want}})
	}
}

func TestKeywordCarriesTheFlagAsWritten(t *testing.T) {
	wants(t, "KEYWORD $Junk", imap.SearchCriteria{Flag: []imap.Flag{"$Junk"}})
	wants(t, "UNKEYWORD $Junk", imap.SearchCriteria{NotFlag: []imap.Flag{"$Junk"}})
}

// The five header keys with names of their own become header searches, because
// that is the only field go-imap has for them; it turns them back into BCC,
// CC, FROM, SUBJECT and TO on the wire.
func TestTheNamedHeaderKeysSearchTheirHeader(t *testing.T) {
	for _, name := range []string{"BCC", "CC", "FROM", "SUBJECT", "TO"} {
		wants(t, name+" invoice", imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: name, Value: "invoice"}},
		})
	}
}

func TestHeaderTakesItsOwnFieldName(t *testing.T) {
	wants(t, "HEADER X-Spam-Flag YES", imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "X-Spam-Flag", Value: "YES"}},
	})
}

func TestBodyAndTextSearchSeparately(t *testing.T) {
	wants(t, "BODY hello", imap.SearchCriteria{Body: []string{"hello"}})
	wants(t, "TEXT hello", imap.SearchCriteria{Text: []string{"hello"}})
}

func TestQuotedValuesKeepTheirSpaces(t *testing.T) {
	wants(t, `SUBJECT "quarterly report"`, imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "SUBJECT", Value: "quarterly report"}},
	})
	wants(t, `SUBJECT "say \"hello\""`, imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "SUBJECT", Value: `say "hello"`}},
	})
}

// A quoted string can hold anything, including something that would otherwise
// be a search key. Reading it as a key would search for the wrong thing
// entirely.
func TestAQuotedValueIsNeverReadAsAKey(t *testing.T) {
	wants(t, `SUBJECT "SEEN"`, imap.SearchCriteria{
		Header: []imap.SearchCriteriaHeaderField{{Key: "SUBJECT", Value: "SEEN"}},
	})
	refuses(t, `"SEEN"`, "quoted")
}

func TestTheFourPlainDateKeys(t *testing.T) {
	d := day(2020, time.February, 1)
	wants(t, "SINCE 1-Feb-2020", imap.SearchCriteria{Since: d})
	wants(t, "BEFORE 1-Feb-2020", imap.SearchCriteria{Before: d})
	wants(t, "SENTSINCE 1-Feb-2020", imap.SearchCriteria{SentSince: d})
	wants(t, "SENTBEFORE 1-Feb-2020", imap.SearchCriteria{SentBefore: d})
}

// ON has no field of its own: it is a day, expressed as the half-open interval
// go-imap turns back into ON. The gap has to be exactly a day or it is encoded
// as a SINCE and a BEFORE instead, which is a different search.
func TestOnBecomesTheDayItNames(t *testing.T) {
	d := day(2020, time.February, 1)
	wants(t, "ON 1-Feb-2020", imap.SearchCriteria{Since: d, Before: d.AddDate(0, 0, 1)})
	wants(t, "SENTON 1-Feb-2020", imap.SearchCriteria{SentSince: d, SentBefore: d.AddDate(0, 0, 1)})

	got := parse(t, "ON 1-Feb-2020")
	if gap := got.Before.Sub(got.Since); gap != 24*time.Hour {
		t.Errorf("ON spans %v, want exactly 24h or it is not encoded as ON", gap)
	}
}

// A day either side of a spring-forward, where a calendar day is 23 hours
// long. Dates are parsed in UTC precisely so that the 24-hour gap ON depends
// on is not at the mercy of the local zone.
func TestOnSpansADayEvenAcrossADaylightSavingChange(t *testing.T) {
	for _, date := range []string{"29-Mar-2020", "25-Oct-2020", "8-Mar-2020"} {
		got := parse(t, "ON "+date)
		if gap := got.Before.Sub(got.Since); gap != 24*time.Hour {
			t.Errorf("ON %s spans %v, want 24h", date, gap)
		}
	}
}

func TestDatesAcceptTheSpellingsRealPeopleUse(t *testing.T) {
	want := day(2020, time.February, 1)
	for _, spelling := range []string{"1-Feb-2020", "01-Feb-2020", `"1-Feb-2020"`, "1-feb-2020", "1-FEB-2020"} {
		if got := parse(t, "SINCE "+spelling); !got.Since.Equal(want) {
			t.Errorf("SINCE %s = %v, want %v", spelling, got.Since, want)
		}
	}
}

func TestSizeBoundsCarryTheirNumber(t *testing.T) {
	wants(t, "LARGER 1000", imap.SearchCriteria{Larger: 1000})
	wants(t, "SMALLER 1000", imap.SearchCriteria{Smaller: 1000})
}

// A bound of zero is dropped by go-imap rather than sent, so SMALLER 0 — which
// matches nothing — would arrive as a search matching everything. That
// inversion is the reason this is refused rather than passed along.
func TestAZeroSizeBoundIsRefusedRatherThanInverted(t *testing.T) {
	refuses(t, "SMALLER 0", "matches everything")
	refuses(t, "LARGER 0", "matches everything")
}

func TestUidSetsAreParsedAsWritten(t *testing.T) {
	var single imap.UIDSet
	single.AddNum(5)
	wants(t, "UID 5", imap.SearchCriteria{UID: []imap.UIDSet{single}})

	var mixed imap.UIDSet
	mixed.AddNum(1)
	mixed.AddRange(10, 20)
	mixed.AddNum(30)
	wants(t, "UID 1,10:20,30", imap.SearchCriteria{UID: []imap.UIDSet{mixed}})
}

// "*" is the highest UID the mailbox holds, which go-imap and the protocol
// both spell as zero.
func TestAStarInAUidSetMeansTheHighestUid(t *testing.T) {
	var open imap.UIDSet
	open.AddRange(100, 0)
	wants(t, "UID 100:*", imap.SearchCriteria{UID: []imap.UIDSet{open}})
}

func TestNotWrapsTheKeyAfterIt(t *testing.T) {
	wants(t, "NOT SEEN", imap.SearchCriteria{
		Not: []imap.SearchCriteria{{Flag: []imap.Flag{imap.FlagSeen}}},
	})
}

func TestOrTakesTwoKeys(t *testing.T) {
	wants(t, "OR SEEN FLAGGED", imap.SearchCriteria{
		Or: [][2]imap.SearchCriteria{{
			{Flag: []imap.Flag{imap.FlagSeen}},
			{Flag: []imap.Flag{imap.FlagFlagged}},
		}},
	})
}

// This is the whole reason parentheses exist in the grammar: without the
// group, OR would take SEEN and FLAGGED and leave DELETED dangling as a third
// top-level key, which is a different search.
func TestParenthesesGroupKeysForOr(t *testing.T) {
	wants(t, "OR (SEEN FLAGGED) DELETED", imap.SearchCriteria{
		Or: [][2]imap.SearchCriteria{{
			{Flag: []imap.Flag{imap.FlagSeen, imap.FlagFlagged}},
			{Flag: []imap.Flag{imap.FlagDeleted}},
		}},
	})
}

func TestNestedGroupsAreFollowed(t *testing.T) {
	wants(t, "NOT (OR SEEN (FLAGGED DELETED))", imap.SearchCriteria{
		Not: []imap.SearchCriteria{{
			Or: [][2]imap.SearchCriteria{{
				{Flag: []imap.Flag{imap.FlagSeen}},
				{Flag: []imap.Flag{imap.FlagFlagged, imap.FlagDeleted}},
			}},
		}},
	})
}

// Keys in a row are ANDed. Getting this wrong in the other direction — taking
// the last key, or ORing them — would silently copy a different set of
// messages, which is exactly the failure a drop-in must not have.
func TestKeysInARowAreAnded(t *testing.T) {
	wants(t, "UNSEEN SMALLER 1000 FROM boss", imap.SearchCriteria{
		NotFlag: []imap.Flag{imap.FlagSeen},
		Smaller: 1000,
		Header:  []imap.SearchCriteriaHeaderField{{Key: "FROM", Value: "boss"}},
	})
}

// Two bounds of the same kind intersect rather than the later one winning.
// Anything else would answer a question nobody asked: the messages since June
// are a subset of those since January, so the AND is the later date.
func TestRepeatedBoundsIntersect(t *testing.T) {
	got := parse(t, "SINCE 1-Jan-2020 SINCE 1-Jun-2020")
	if want := day(2020, time.June, 1); !got.Since.Equal(want) {
		t.Errorf("SINCE twice = %v, want the later date %v", got.Since, want)
	}
	if got := parse(t, "BEFORE 1-Jun-2020 BEFORE 1-Jan-2020"); !got.Before.Equal(day(2020, time.January, 1)) {
		t.Errorf("BEFORE twice = %v, want the earlier date", got.Before)
	}
	if got := parse(t, "SENTSINCE 1-Jan-2020 SENTSINCE 1-Jun-2020"); !got.SentSince.Equal(day(2020, time.June, 1)) {
		t.Errorf("SENTSINCE twice = %v, want the later date", got.SentSince)
	}
	if got := parse(t, "SENTBEFORE 1-Jun-2020 SENTBEFORE 1-Jan-2020"); !got.SentBefore.Equal(day(2020, time.January, 1)) {
		t.Errorf("SENTBEFORE twice = %v, want the earlier date", got.SentBefore)
	}

	if got := parse(t, "SMALLER 5000 SMALLER 1000"); got.Smaller != 1000 {
		t.Errorf("SMALLER twice = %d, want the smaller bound 1000", got.Smaller)
	}
	if got := parse(t, "LARGER 1000 LARGER 5000"); got.Larger != 5000 {
		t.Errorf("LARGER twice = %d, want the larger bound 5000", got.Larger)
	}
}

// go-imap's own SearchCriteria.And erases a SMALLER bound as soon as it is
// ANDed with a key that has none, because it spells "no bound" as zero and
// then finds zero to be the smaller of the two. A search built with it would
// silently lose its upper size bound and match every message however large —
// widening the search, which is the dangerous direction. This is the
// regression test for not using it.
func TestASizeBoundSurvivesTheKeysAfterIt(t *testing.T) {
	for _, in := range []string{
		"SMALLER 1000 UNSEEN",
		"SMALLER 1000 FROM boss SEEN",
		"UNSEEN SMALLER 1000 UNDELETED",
	} {
		if got := parse(t, in); got.Smaller != 1000 {
			t.Errorf("Parse(%q).Smaller = %d, want the bound to survive as 1000", in, got.Smaller)
		}
	}
	for _, in := range []string{"LARGER 1000 UNSEEN", "UNSEEN LARGER 1000 SEEN"} {
		if got := parse(t, in); got.Larger != 1000 {
			t.Errorf("Parse(%q).Larger = %d, want the bound to survive as 1000", in, got.Larger)
		}
	}
	for _, in := range []string{"SINCE 1-Feb-2020 UNSEEN", "BEFORE 1-Feb-2020 UNSEEN"} {
		got := parse(t, in)
		if got.Since.IsZero() && got.Before.IsZero() {
			t.Errorf("Parse(%q) lost its date bound", in)
		}
	}
}

// \Recent is a session's opinion, not a message's property: the server clears
// it for whichever client looks first, so the same search gives different
// answers on consecutive runs. go-imap has no encoding for it either, so the
// alternative to refusing is sending KEYWORD \Recent, which searches for a
// keyword no message carries.
func TestTheRecentKeysAreRefusedRatherThanMistranslated(t *testing.T) {
	for _, key := range []string{"RECENT", "NEW", "OLD"} {
		refuses(t, key, `\Recent`)
	}
}

// Sequence numbers are positions, and positions move. A set that named the
// right messages when the command was typed can name different ones by the
// time the search runs.
func TestABareSequenceSetIsRefusedAndPointsAtUid(t *testing.T) {
	refuses(t, "1:100", "UID 1:100")
	refuses(t, "5", "UID 5")
}

func TestAnUnknownKeyIsNamedInTheError(t *testing.T) {
	refuses(t, "PLEASE", `"PLEASE" is not an IMAP search key`)
	refuses(t, "SEEN NONSENSE", `"NONSENSE"`)
}

func TestAnEmptySearchIsRefused(t *testing.T) {
	refuses(t, "", "empty")
	refuses(t, "   ", "empty")
}

func TestUnbalancedParenthesesAreRefused(t *testing.T) {
	refuses(t, "(SEEN", "never closed")
	refuses(t, "SEEN)", "unmatched")
	refuses(t, "()", "no search key")
}

func TestAKeyMissingItsValueIsRefused(t *testing.T) {
	refuses(t, "SUBJECT", "nothing after it")
	refuses(t, "HEADER X-Thing", "nothing after it")
	refuses(t, "NOT", "NOT needs a search key")
	refuses(t, "OR SEEN", "OR needs two search keys")
	refuses(t, "SUBJECT (SEEN)", "rather than a value")
}

func TestMalformedValuesAreRefused(t *testing.T) {
	refuses(t, "SINCE yesterday", "1-Feb-2020")
	refuses(t, "SINCE 31-Feb-2020", "1-Feb-2020")
	refuses(t, "LARGER lots", "number of bytes")
	refuses(t, "UID 1:apple", "set of UIDs")
	refuses(t, "UID 0", "set of UIDs")
}

// A literal is a length followed by bytes on a network connection. A command
// line has no second half to deliver them, so accepting the syntax would only
// produce a search that hangs waiting for them.
func TestLiteralsAreRefusedWithAnExplanation(t *testing.T) {
	refuses(t, "SUBJECT {5}", "command line")
}

func TestAnUnterminatedQuotedStringIsRefused(t *testing.T) {
	refuses(t, `SUBJECT "unfinished`, "unterminated")
	refuses(t, `SUBJECT "trailing\`, "backslash")
}

// The text is kept so that messages about a search can quote it. A search that
// matched nothing is a puzzle without it and an answer with it.
func TestTheKeyRemembersHowItWasWritten(t *testing.T) {
	k, err := searchkey.Parse("  UNSEEN SMALLER 1000  ")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := k.String(), "UNSEEN SMALLER 1000"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
