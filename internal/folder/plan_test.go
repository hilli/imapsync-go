package folder

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
)

func mailbox(name string, attrs ...string) imapx.Folder {
	f := imapx.Folder{Name: name, Delim: "/", Selectable: true, Attrs: attrs}
	for _, a := range attrs {
		if a == `\Noselect` {
			f.Selectable = false
		}
	}
	return f
}

// icloudFolders reproduces the shape measured against a real account: only
// \Sent and \Trash carry attributes, everything else arrives bare.
func icloudFolders() []imapx.Folder {
	return []imapx.Folder{
		mailbox("INBOX"),
		mailbox("Archive"),
		mailbox("Deleted Messages", `\Trash`),
		mailbox("Drafts"),
		mailbox("Junk"),
		mailbox("Notes"),
		mailbox("Sent Messages", `\Sent`),
		mailbox("Projects/imapsync"),
	}
}

// moxFolders is a modern IMAP4rev2 destination, which marks everything.
func moxFolders() []imapx.Folder {
	return []imapx.Folder{
		mailbox("INBOX"),
		mailbox("Archive", `\Archive`),
		mailbox("Drafts", `\Drafts`),
		mailbox("Junk", `\Junk`),
		mailbox("Sent", `\Sent`),
		mailbox("Trash", `\Trash`),
	}
}

func destFor(t *testing.T, plan Plan, source string) Pair {
	t.Helper()

	for _, p := range plan.Pairs {
		if p.Source == source {
			return p
		}
	}
	for _, s := range plan.Skips {
		if s.Source == source {
			t.Fatalf("%q was skipped: %s", source, s.Reason)
		}
	}
	t.Fatalf("%q appears in neither pairs nor skips", source)
	return Pair{}
}

// TestAutomapUsesTheDestinationsOwnNames is the case the whole package exists
// for. iCloud calls it "Sent Messages" and mox calls it "Sent"; copying by name
// would leave two sent folders on the destination and neither marked correctly.
func TestAutomapUsesTheDestinationsOwnNames(t *testing.T) {
	t.Parallel()

	plan, err := Build(icloudFolders(), moxFolders(), Options{
		SourceDelim: "/", DestDelim: "/", Automap: true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	for _, tc := range []struct{ source, wantDest string }{
		{"INBOX", "INBOX"},
		{"Sent Messages", "Sent"},
		{"Deleted Messages", "Trash"},
		{"Drafts", "Drafts"},
		{"Junk", "Junk"},
		{"Archive", "Archive"},
		{"Notes", "Notes"},
		{"Projects/imapsync", "Projects/imapsync"},
	} {
		if got := destFor(t, plan, tc.source).Dest; got != tc.wantDest {
			t.Errorf("%q maps to %q, want %q", tc.source, got, tc.wantDest)
		}
	}
}

// TestNameHeuristicsAreLoadBearing pins the measured iCloud behaviour: it marks
// only Sent and Trash, so an attribute-only mapper would leave Drafts, Junk and
// Archive to be created as new folders beside the destination's own.
func TestNameHeuristicsAreLoadBearing(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{
		mailbox("Drafts"), // bare, exactly as iCloud sends it
		mailbox("Junk"),
		mailbox("Archive"),
	}
	dest := []imapx.Folder{
		mailbox("Kladder", `\Drafts`), // a destination that names them differently
		mailbox("Spam", `\Junk`),
		mailbox("Arkiv", `\Archive`),
	}

	plan, err := Build(source, dest, Options{SourceDelim: "/", DestDelim: "/", Automap: true})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	for _, tc := range []struct{ source, wantDest string }{
		{"Drafts", "Kladder"},
		{"Junk", "Spam"},
		{"Archive", "Arkiv"},
	} {
		p := destFor(t, plan, tc.source)
		if p.Dest != tc.wantDest {
			t.Errorf("%q maps to %q, want %q", tc.source, p.Dest, tc.wantDest)
		}
		if p.CreateDest {
			t.Errorf("%q would create %q, which already exists on the destination", tc.source, p.Dest)
		}
	}
}

// TestRoleNamesAreNotMatchedOnSubfolders keeps the heuristic from firing on
// nested mailboxes: "2019/Sent" is a year's archive, not the sent folder, and
// mapping it onto the destination's Sent would merge it with real sent mail.
func TestRoleNamesAreNotMatchedOnSubfolders(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{mailbox("INBOX"), mailbox("2019/Sent"), mailbox("Sent Messages", `\Sent`)}

	plan, err := Build(source, moxFolders(), Options{SourceDelim: "/", DestDelim: "/", Automap: true})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got := destFor(t, plan, "2019/Sent").Dest; got != "2019/Sent" {
		t.Errorf("2019/Sent maps to %q, want it left where it is", got)
	}
	if got := destFor(t, plan, "Sent Messages").Dest; got != "Sent" {
		t.Errorf("Sent Messages maps to %q, want Sent", got)
	}
}

// TestMergingSourceFoldersIsRefused covers the one mistake nothing downstream
// can detect. Two sent folders collapsing into one destination looks exactly
// like a large first sync in every subsequent report.
func TestMergingSourceFoldersIsRefused(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{
		mailbox("Sent", `\Sent`),
		mailbox("Sent Messages"), // an older folder the user never cleaned up
	}

	_, err := Build(source, moxFolders(), Options{SourceDelim: "/", DestDelim: "/", Automap: true})
	if err == nil {
		t.Fatal("Build() merged two source folders into one destination without complaint")
	}
	for _, want := range []string{"Sent", "Sent Messages", "merged"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestVirtualFoldersAreSkipped guards against the Gmail duplication trap: All
// Mail contains every message in the account, so copying it doubles everything.
func TestVirtualFoldersAreSkipped(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{
		mailbox("INBOX"),
		mailbox("[Gmail]/All Mail", `\All`),
		mailbox("[Gmail]/Starred", `\Flagged`),
		mailbox("[Gmail]/Sent Mail", `\Sent`),
	}

	plan, err := Build(source, moxFolders(), Options{SourceDelim: "/", DestDelim: "/", Automap: true})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	skipped := map[string]string{}
	for _, s := range plan.Skips {
		skipped[s.Source] = s.Reason
	}
	for _, name := range []string{"[Gmail]/All Mail", "[Gmail]/Starred"} {
		reason, ok := skipped[name]
		if !ok {
			t.Errorf("%q was copied; it duplicates the whole account", name)
			continue
		}
		if !strings.Contains(reason, "duplicate") {
			t.Errorf("%q skipped with reason %q, want it to explain the duplication", name, reason)
		}
	}
	if got := destFor(t, plan, "[Gmail]/Sent Mail").Dest; got != "Sent" {
		t.Errorf("[Gmail]/Sent Mail maps to %q, want Sent", got)
	}

	// The escape hatch has to actually work, or the default is a wall.
	withVirtual, err := Build(source, moxFolders(), Options{
		SourceDelim: "/", DestDelim: "/", Automap: true, IncludeVirtual: true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got := destFor(t, withVirtual, "[Gmail]/All Mail").Dest; got != "[Gmail]/All Mail" {
		// mox has \Archive but no \All, so there is no role to map onto and the
		// literal name is the honest answer.
		t.Errorf("with IncludeVirtual, All Mail maps to %q, want the name copied verbatim", got)
	}
}

func TestDelimiterTranslation(t *testing.T) {
	t.Parallel()

	// A Courier-style source: dot-delimited and nested under INBOX.
	source := []imapx.Folder{
		{Name: "INBOX", Delim: ".", Selectable: true},
		{Name: "INBOX.Work", Delim: ".", Selectable: true},
		{Name: "INBOX.Work.2019", Delim: ".", Selectable: true},
	}

	plan, err := Build(source, []imapx.Folder{mailbox("INBOX")}, Options{
		SourceDelim: ".", SourcePrefix: "INBOX.", DestDelim: "/", Automap: true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	for _, tc := range []struct{ source, wantDest string }{
		{"INBOX", "INBOX"},
		{"INBOX.Work", "Work"},
		{"INBOX.Work.2019", "Work/2019"},
	} {
		if got := destFor(t, plan, tc.source).Dest; got != tc.wantDest {
			t.Errorf("%q maps to %q, want %q", tc.source, got, tc.wantDest)
		}
	}
}

// TestANameCarryingTheDestinationDelimiterIsRefused covers a folder whose own
// name contains the destination's separator. Writing it out would silently turn
// one mailbox into two levels of somebody else's tree.
func TestANameCarryingTheDestinationDelimiterIsRefused(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{
		{Name: "INBOX", Delim: ".", Selectable: true},
		{Name: "INBOX.Q/A", Delim: ".", Selectable: true},
	}

	plan, err := Build(source, []imapx.Folder{mailbox("INBOX")}, Options{
		SourceDelim: ".", SourcePrefix: "INBOX.", DestDelim: "/",
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	for _, p := range plan.Pairs {
		if p.Source == "INBOX.Q/A" {
			t.Fatalf("INBOX.Q/A was mapped to %q, splitting one mailbox into a hierarchy", p.Dest)
		}
	}
	var reason string
	for _, s := range plan.Skips {
		if s.Source == "INBOX.Q/A" {
			reason = s.Reason
		}
	}
	if !strings.Contains(reason, "delimiter") {
		t.Errorf("skip reason %q does not explain the delimiter clash", reason)
	}
}

func TestCreatesListsParentsBeforeChildren(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{
		mailbox("INBOX"),
		mailbox("Projects/imapsync/design"),
		mailbox("Projects/imapsync"),
	}

	plan, err := Build(source, []imapx.Folder{mailbox("INBOX")}, Options{
		SourceDelim: "/", DestDelim: "/", Automap: true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	// RFC 3501 leaves parent creation optional, so the missing intermediate has
	// to be created explicitly and before anything below it.
	want := []string{"Projects", "Projects/imapsync", "Projects/imapsync/design"}
	if len(plan.Creates) != len(want) {
		t.Fatalf("Creates = %v, want %v", plan.Creates, want)
	}
	for i, name := range want {
		if plan.Creates[i] != name {
			t.Errorf("Creates[%d] = %q, want %q", i, plan.Creates[i], name)
		}
	}
}

func TestExistingDestinationsAreNotRecreated(t *testing.T) {
	t.Parallel()

	plan, err := Build(icloudFolders(), moxFolders(), Options{
		SourceDelim: "/", DestDelim: "/", Automap: true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, name := range plan.Creates {
		if name == "Sent" || name == "Trash" || name == "INBOX" {
			t.Errorf("Creates includes %q, which already exists on the destination: %v", name, plan.Creates)
		}
	}
}

// TestInboxIsMatchedCaseInsensitively follows RFC 3501, which makes INBOX and
// only INBOX case-insensitive. Treating "Inbox" as a new mailbox would try to
// create a second one beside the real thing.
func TestInboxIsMatchedCaseInsensitively(t *testing.T) {
	t.Parallel()

	plan, err := Build([]imapx.Folder{mailbox("Inbox")}, []imapx.Folder{mailbox("INBOX")}, Options{
		SourceDelim: "/", DestDelim: "/", Automap: true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	p := destFor(t, plan, "Inbox")
	if p.Dest != "INBOX" {
		t.Errorf("Dest = %q, want the destination's own spelling", p.Dest)
	}
	if p.CreateDest {
		t.Error("CreateDest is true for a mailbox that already exists")
	}
	if len(plan.Creates) != 0 {
		t.Errorf("Creates = %v, want nothing", plan.Creates)
	}
}

// TestOtherNamesAreCaseSensitive is the other half of that rule: a server that
// distinguishes "Work" from "work" must not have them merged.
func TestOtherNamesAreCaseSensitive(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{mailbox("Work"), mailbox("work")}

	plan, err := Build(source, []imapx.Folder{mailbox("INBOX")}, Options{
		SourceDelim: "/", DestDelim: "/", Automap: true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(plan.Pairs) != 2 {
		t.Fatalf("got %d pairs, want both mailboxes kept apart: %+v", len(plan.Pairs), plan.Pairs)
	}
	if destFor(t, plan, "Work").Dest == destFor(t, plan, "work").Dest {
		t.Error("Work and work were merged into one destination")
	}
}

func TestExplicitMappingsWinOverAutomap(t *testing.T) {
	t.Parallel()

	plan, err := Build(icloudFolders(), moxFolders(), Options{
		SourceDelim: "/", DestDelim: "/", Automap: true,
		Mappings: map[string]string{"Sent Messages": "Old Sent"},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	p := destFor(t, plan, "Sent Messages")
	if p.Dest != "Old Sent" {
		t.Errorf("Dest = %q, want the explicit override", p.Dest)
	}
	if !p.CreateDest {
		t.Error("CreateDest is false for a mailbox the destination does not have")
	}
}

func TestFilters(t *testing.T) {
	t.Parallel()

	opts := func(f func(*Options)) Options {
		o := Options{SourceDelim: "/", DestDelim: "/", Automap: true}
		f(&o)
		return o
	}

	for _, tc := range []struct {
		name string
		opts Options
		want []string
	}{
		{
			name: "only",
			opts: opts(func(o *Options) { o.Only = []string{"INBOX", "Notes"} }),
			want: []string{"INBOX", "Notes"},
		},
		{
			name: "include",
			opts: opts(func(o *Options) { o.Include = []*regexp.Regexp{regexp.MustCompile(`^Projects/`)} }),
			want: []string{"Projects/imapsync"},
		},
		{
			name: "exclude",
			opts: opts(func(o *Options) {
				o.Exclude = []*regexp.Regexp{regexp.MustCompile(`^(Notes|Junk|Archive|Drafts|Projects/.*)$`)}
			}),
			want: []string{"Deleted Messages", "INBOX", "Sent Messages"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			plan, err := Build(icloudFolders(), moxFolders(), tc.opts)
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			var got []string
			for _, p := range plan.Pairs {
				got = append(got, p.Source)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("kept %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNoselectFoldersAreSkippedButTheirChildrenAreNot(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{
		mailbox("INBOX"),
		mailbox("[Gmail]", `\Noselect`),
		mailbox("[Gmail]/Sent Mail", `\Sent`),
	}

	plan, err := Build(source, moxFolders(), Options{SourceDelim: "/", DestDelim: "/", Automap: true})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, p := range plan.Pairs {
		if p.Source == "[Gmail]" {
			t.Error("a \\Noselect mailbox was selected for copying")
		}
	}
	if got := destFor(t, plan, "[Gmail]/Sent Mail").Dest; got != "Sent" {
		t.Errorf("the child maps to %q, want Sent", got)
	}
}

func TestDestSubfolderNestsTheWholeTree(t *testing.T) {
	t.Parallel()

	plan, err := Build(icloudFolders(), moxFolders(), Options{
		SourceDelim: "/", DestDelim: "/", Automap: true, DestSubfolder: "icloud",
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	for _, p := range plan.Pairs {
		if !strings.HasPrefix(p.Dest, "icloud/") {
			t.Errorf("%q maps to %q, which escapes the subfolder", p.Source, p.Dest)
		}
	}
	if got := destFor(t, plan, "INBOX").Dest; got != "icloud/INBOX" {
		t.Errorf("INBOX maps to %q, want icloud/INBOX", got)
	}
}

// TestAutomapOffCopiesByName checks the option actually does something, so a
// user who distrusts the mapping can turn it off and get literal names.
func TestAutomapOffCopiesByName(t *testing.T) {
	t.Parallel()

	plan, err := Build(icloudFolders(), moxFolders(), Options{
		SourceDelim: "/", DestDelim: "/", Automap: false,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	p := destFor(t, plan, "Sent Messages")
	if p.Dest != "Sent Messages" {
		t.Errorf("Dest = %q, want the source name copied verbatim", p.Dest)
	}
	if !p.CreateDest {
		t.Error("CreateDest is false for a mailbox the destination does not have")
	}
	// INBOX stays INBOX regardless: it is RFC 3501 special, not an automapping.
	if got := destFor(t, plan, "INBOX").Dest; got != "INBOX" {
		t.Errorf("INBOX maps to %q with automap off, want INBOX", got)
	}
}

// TestDestinationRoleChoiceIsDeterministic covers a destination with two
// candidates for one role. A marked mailbox must win over a guessed one, and
// the answer must not depend on the order the server listed them in.
func TestDestinationRoleChoiceIsDeterministic(t *testing.T) {
	t.Parallel()

	source := []imapx.Folder{mailbox("Sent Messages", `\Sent`)}
	orders := [][]imapx.Folder{
		{mailbox("Sent"), mailbox("Sent Items", `\Sent`)},
		{mailbox("Sent Items", `\Sent`), mailbox("Sent")},
	}

	for _, dest := range orders {
		plan, err := Build(source, dest, Options{SourceDelim: "/", DestDelim: "/", Automap: true})
		if err != nil {
			t.Fatalf("Build() error = %v", err)
		}
		if got := destFor(t, plan, "Sent Messages").Dest; got != "Sent Items" {
			t.Errorf("with destination order %q/%q the source maps to %q, want the marked mailbox Sent Items",
				dest[0].Name, dest[1].Name, got)
		}
	}
}
