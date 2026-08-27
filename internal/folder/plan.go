// Package folder decides which source mailbox is copied to which destination
// mailbox.
//
// The mapping is a pure function of the two folder listings and the user's
// options, so it can be shown by --dry-run and tested without a server. Getting
// it wrong is expensive in a way message copying is not: two source folders
// pointed at one destination merge silently, and nothing later in the pipeline
// can tell that happened.
package folder

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/hilli/imapsync-go/internal/imapx"
)

// Role is the purpose a mailbox serves, as an RFC 6154 special-use attribute.
type Role string

// Roles. RoleInbox is not an RFC 6154 attribute; INBOX is special in RFC 3501
// itself and needs no marking.
const (
	RoleNone      Role = ""
	RoleInbox     Role = "INBOX"
	RoleAll       Role = `\All`
	RoleArchive   Role = `\Archive`
	RoleDrafts    Role = `\Drafts`
	RoleFlagged   Role = `\Flagged`
	RoleImportant Role = `\Important`
	RoleJunk      Role = `\Junk`
	RoleSent      Role = `\Sent`
	RoleTrash     Role = `\Trash`
)

// Virtual reports whether a role names a view over messages stored elsewhere
// rather than a mailbox of its own.
//
// Gmail's \All is the case that matters: it lists every message in the account,
// so copying it duplicates everything already copied from the real folders.
// imapsync maps \All like any other special folder and leaves users to find out.
// This is a deliberate divergence — the default skips these and says so, and
// IncludeVirtual re-enables them for anyone who wants the duplicates.
func (r Role) Virtual() bool {
	switch r {
	case RoleAll, RoleFlagged, RoleImportant:
		return true
	default:
		return false
	}
}

// nameRoles maps a mailbox name to the role it almost certainly serves, for
// servers that do not mark their folders. iCloud is one: it returns \Sent and
// \Trash but leaves Drafts, Junk, Archive and Notes bare, so names carry the
// rest of the account.
//
// Keys are compared lowercased. Names are UTF-8 here: go-imap decodes modified
// UTF-7 before a mailbox name ever reaches us.
var nameRoles = map[string]Role{
	"all":          RoleAll,
	"all mail":     RoleAll,
	"all messages": RoleAll,

	"archive":  RoleArchive,
	"archives": RoleArchive,
	"arkiv":    RoleArchive,

	"drafts":     RoleDrafts,
	"draft":      RoleDrafts,
	"kladder":    RoleDrafts,
	"entwürfe":   RoleDrafts,
	"brouillons": RoleDrafts,
	"borradores": RoleDrafts,
	"bozze":      RoleDrafts,
	"szkice":     RoleDrafts,

	"flagged": RoleFlagged,
	"starred": RoleFlagged,

	"important": RoleImportant,

	"junk":           RoleJunk,
	"junk e-mail":    RoleJunk,
	"junk email":     RoleJunk,
	"spam":           RoleJunk,
	"bulk mail":      RoleJunk,
	"uønsket e-mail": RoleJunk,

	"sent":               RoleSent,
	"sent mail":          RoleSent,
	"sent messages":      RoleSent,
	"sent items":         RoleSent,
	"sendt":              RoleSent,
	"sendt post":         RoleSent,
	"gesendet":           RoleSent,
	"gesendete elemente": RoleSent,
	"gesendete objekte":  RoleSent,
	"éléments envoyés":   RoleSent,
	"elementos enviados": RoleSent,
	"posta inviata":      RoleSent,

	"trash":            RoleTrash,
	"deleted":          RoleTrash,
	"deleted items":    RoleTrash,
	"deleted messages": RoleTrash,
	"papirkurv":        RoleTrash,
	"slettet post":     RoleTrash,
	"papierkorb":       RoleTrash,
	"corbeille":        RoleTrash,
	"papelera":         RoleTrash,
	"cestino":          RoleTrash,
	"kosz":             RoleTrash,
}

// Options configures the mapping. The defaults match imapsync's except where
// noted.
type Options struct {
	// SourceDelim and DestDelim are the hierarchy separators the two servers
	// report. An empty value means the server gave none, and names are used
	// unsplit.
	SourceDelim string
	DestDelim   string

	// SourcePrefix and DestPrefix are personal-namespace prefixes, as from
	// NAMESPACE. A source prefix is stripped and a destination prefix added, so
	// a Courier "INBOX.Work" becomes a Dovecot "Work".
	SourcePrefix string
	DestPrefix   string

	// Automap resolves each source folder's role and uses whatever the
	// destination calls that role.
	Automap bool

	// IncludeVirtual copies mailboxes that are views over other mailboxes, such
	// as Gmail's All Mail. Off by default; see Role.Virtual.
	IncludeVirtual bool

	// Only restricts the run to these exact source mailbox names.
	Only []string
	// Include, when non-empty, keeps only source mailboxes matching one of the
	// patterns. Exclude then drops any that match.
	Include []*regexp.Regexp
	Exclude []*regexp.Regexp

	// Mappings are explicit source-to-destination overrides, applied ahead of
	// everything else.
	Mappings map[string]string

	// DestSubfolder nests the whole copied tree under one destination mailbox.
	DestSubfolder string
}

// Pair is one source mailbox and the destination it is copied to.
type Pair struct {
	Source string
	Dest   string
	Role   Role
	// CreateDest reports that the destination mailbox does not exist yet.
	CreateDest bool
}

// Skip is a source mailbox left out of the run, with the reason.
type Skip struct {
	Source string
	Reason string
	// ByRequest marks a skip the caller asked for with --folder, --include or
	// --exclude. Those are expected and a report can summarise them; a skip
	// with ByRequest false is imapsync-go's own decision and worth reading.
	ByRequest bool
}

// Plan is the full mapping for one run.
type Plan struct {
	Pairs []Pair
	Skips []Skip
	// Creates lists destination mailboxes to create, parents before children.
	// RFC 3501 says a server *may* create missing parents, so relying on it
	// works until it meets a server that does not.
	Creates []string
}

// Build maps source mailboxes onto destination mailboxes.
//
// It fails rather than guessing when two source mailboxes resolve to one
// destination. Merging two folders is not something a later stage can detect or
// undo, and the resulting diff looks exactly like an ordinary first sync.
func Build(source, dest []imapx.Folder, opts Options) (Plan, error) {
	destByRole := rolesOf(dest, opts.DestPrefix)
	destNames := make(map[string]string, len(dest))
	for _, f := range dest {
		destNames[foldName(f.Name)] = f.Name
	}

	var plan Plan
	claimed := make(map[string][]string)

	for _, f := range source {
		rel := stripPrefix(f.Name, opts.SourcePrefix)
		role := detectRole(f, rel)

		if reason, byRequest := filterReason(f, role, opts); reason != "" {
			plan.Skips = append(plan.Skips, Skip{Source: f.Name, Reason: reason, ByRequest: byRequest})
			continue
		}

		target, err := destinationFor(f.Name, rel, role, opts, destByRole)
		if err != nil {
			plan.Skips = append(plan.Skips, Skip{Source: f.Name, Reason: err.Error()})
			continue
		}

		existing, exists := destNames[foldName(target)]
		if exists {
			// Adopt the destination's own spelling, so a case difference in INBOX
			// does not look like a mailbox that needs creating.
			target = existing
		}

		claimed[foldName(target)] = append(claimed[foldName(target)], f.Name)
		plan.Pairs = append(plan.Pairs, Pair{
			Source:     f.Name,
			Dest:       target,
			Role:       role,
			CreateDest: !exists,
		})
	}

	if err := checkCollisions(claimed); err != nil {
		return Plan{}, err
	}

	sort.Slice(plan.Pairs, func(i, j int) bool { return plan.Pairs[i].Source < plan.Pairs[j].Source })
	sort.Slice(plan.Skips, func(i, j int) bool { return plan.Skips[i].Source < plan.Skips[j].Source })
	plan.Creates = createOrder(plan.Pairs, destNames, opts.DestDelim)

	return plan, nil
}

// filterReason returns why a source mailbox is left out, or "" to keep it.
// The bool reports whether the skip is the direct consequence of a filter the
// caller asked for. Those are expected and can be summarised; the rest are
// decisions imapsync-go made on its own and are worth reading individually.
func filterReason(f imapx.Folder, role Role, opts Options) (string, bool) {
	if !f.Selectable {
		// A \Noselect mailbox holds no messages. Its children are listed
		// separately and mapped on their own merits, so nothing is lost by
		// skipping the node itself.
		return "not selectable", false
	}
	if role.Virtual() && !opts.IncludeVirtual {
		return fmt.Sprintf("%s is a view over other mailboxes; copying it would duplicate them (use --include-virtual to override)", role), false
	}
	if len(opts.Only) > 0 && !slices.Contains(opts.Only, f.Name) {
		return "not in --folder", true
	}
	if len(opts.Include) > 0 && !matchesAny(opts.Include, f.Name) {
		return "does not match --include", true
	}
	if matchesAny(opts.Exclude, f.Name) {
		return "matches --exclude", true
	}
	return "", false
}

// destinationFor resolves one source mailbox's destination name.
func destinationFor(name, rel string, role Role, opts Options, destByRole map[Role]string) (string, error) {
	if mapped, ok := opts.Mappings[name]; ok {
		return withSubfolder(mapped, opts), nil
	}

	// INBOX names the same mailbox on every server and is case-insensitive by
	// RFC 3501, so it never goes through prefix or name translation.
	if role == RoleInbox {
		return withSubfolder("INBOX", opts), nil
	}

	if opts.Automap && role != RoleNone {
		if named, ok := destByRole[role]; ok {
			return withSubfolder(named, opts), nil
		}
	}

	translated, err := translate(rel, opts.SourceDelim, opts.DestDelim)
	if err != nil {
		return "", err
	}
	return withSubfolder(opts.DestPrefix+translated, opts), nil
}

func withSubfolder(name string, opts Options) string {
	if opts.DestSubfolder == "" {
		return name
	}
	delim := opts.DestDelim
	if delim == "" {
		delim = "/"
	}
	return opts.DestSubfolder + delim + name
}

// translate rewrites a hierarchy from the source's delimiter to the
// destination's.
//
// A name component containing the destination's delimiter cannot be carried
// across: writing it out would silently split one mailbox into two levels of a
// tree nobody asked for. No encoding fixes this, so the folder is skipped and
// reported rather than restructured behind the operator's back.
func translate(rel, srcDelim, dstDelim string) (string, error) {
	if dstDelim == "" {
		return rel, nil
	}
	if srcDelim == "" {
		if strings.Contains(rel, dstDelim) {
			return "", fmt.Errorf("name contains the destination delimiter %q, which would create an unintended hierarchy", dstDelim)
		}
		return rel, nil
	}
	if srcDelim == dstDelim {
		return rel, nil
	}

	parts := strings.Split(rel, srcDelim)
	for _, p := range parts {
		if strings.Contains(p, dstDelim) {
			return "", fmt.Errorf("name component %q contains the destination delimiter %q, which would create an unintended hierarchy", p, dstDelim)
		}
	}
	return strings.Join(parts, dstDelim), nil
}

// detectRole resolves what a mailbox is for: the server's own attribute if it
// gave one, otherwise its name.
//
// Attributes are read whether or not the server advertises SPECIAL-USE, because
// iCloud sends them without advertising it. Name matching uses the whole path
// below the namespace prefix, never the last component: "Archive" is the
// archive, "2019/Archive" is a year's mail that must not be merged into it.
func detectRole(f imapx.Folder, rel string) Role {
	if strings.EqualFold(f.Name, "INBOX") {
		return RoleInbox
	}
	if role := roleFromAttrs(f.Attrs); role != RoleNone {
		return role
	}
	return nameRoles[strings.ToLower(rel)]
}

func roleFromAttrs(attrs []string) Role {
	for _, attr := range attrs {
		switch Role(attr) {
		case RoleAll, RoleArchive, RoleDrafts, RoleFlagged, RoleImportant, RoleJunk, RoleSent, RoleTrash:
			return Role(attr)
		}
	}
	return RoleNone
}

// rolesOf indexes a folder listing by role, so automap can ask the destination
// what it calls its own Sent folder instead of assuming a name.
func rolesOf(folders []imapx.Folder, prefix string) map[Role]string {
	byRole := make(map[Role]string)
	marked := make(map[Role]bool)

	for _, f := range folders {
		if !f.Selectable {
			continue
		}
		role := detectRole(f, stripPrefix(f.Name, prefix))
		if role == RoleNone || role == RoleInbox {
			continue
		}
		fromAttr := roleFromAttrs(f.Attrs) == role

		// An attribute the server set outranks a guess from a name. Among equals
		// the first in sorted order wins, so the result does not depend on the
		// order the server happened to list its mailboxes in.
		if existing, ok := byRole[role]; ok {
			if marked[role] && !fromAttr {
				continue
			}
			if marked[role] == fromAttr && existing < f.Name {
				continue
			}
		}
		byRole[role] = f.Name
		marked[role] = fromAttr
	}
	return byRole
}

// createOrder lists the destination mailboxes to create, parents first, so a
// server that does not create missing parents still ends up with the tree.
func createOrder(pairs []Pair, existing map[string]string, dstDelim string) []string {
	needed := make(map[string]bool)
	for _, p := range pairs {
		if !p.CreateDest {
			continue
		}
		needed[p.Dest] = true
		if dstDelim == "" {
			continue
		}
		parts := strings.Split(p.Dest, dstDelim)
		for i := 1; i < len(parts); i++ {
			parent := strings.Join(parts[:i], dstDelim)
			if parent == "" {
				continue
			}
			if _, ok := existing[foldName(parent)]; !ok {
				needed[parent] = true
			}
		}
	}

	out := make([]string, 0, len(needed))
	for name := range needed {
		out = append(out, name)
	}
	// Within a hierarchy an ancestor always has fewer separators than its
	// descendants, so ordering by depth puts every parent before its children.
	sort.Slice(out, func(i, j int) bool {
		di, dj := depth(out[i], dstDelim), depth(out[j], dstDelim)
		if di != dj {
			return di < dj
		}
		return out[i] < out[j]
	})
	return out
}

func depth(name, delim string) int {
	if delim == "" {
		return 0
	}
	return strings.Count(name, delim)
}

func checkCollisions(claimed map[string][]string) error {
	var collisions []string
	for dest, sources := range claimed {
		if len(sources) > 1 {
			sort.Strings(sources)
			collisions = append(collisions, fmt.Sprintf("%q <- %s", dest, strings.Join(sources, ", ")))
		}
	}
	if len(collisions) == 0 {
		return nil
	}
	sort.Strings(collisions)
	return fmt.Errorf("these source folders would be merged into one destination, which cannot be undone: %s",
		strings.Join(collisions, "; "))
}

func stripPrefix(name, prefix string) string {
	if prefix == "" || !strings.HasPrefix(name, prefix) {
		return name
	}
	stripped := strings.TrimPrefix(name, prefix)
	if stripped == "" {
		// The prefix is itself a mailbox, typically INBOX on a server that nests
		// everything below it. Stripping it to nothing would lose the folder.
		return name
	}
	return stripped
}

// foldName normalises a mailbox name for comparison. Only INBOX is folded:
// RFC 3501 makes INBOX alone case-insensitive, and folding everything would
// merge a "Work" and a "work" that the server keeps apart.
func foldName(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

func matchesAny(patterns []*regexp.Regexp, name string) bool {
	for _, p := range patterns {
		if p.MatchString(name) {
			return true
		}
	}
	return false
}
