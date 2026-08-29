package compat

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// disposition is what becomes of one imapsync option.
type disposition int

const (
	// refuse stops the run and says why. It is the zero value on purpose: an
	// option nobody thought about must not slip through doing nothing.
	refuse disposition = iota
	// translate emits a native flag.
	translate
	// ignore accepts the option and reports that it did nothing, with a
	// reason. Reserved for options that ask for what already happens, or for
	// something that cannot mean anything here.
	ignore
	// endpoint is folded into --source-url or --dest-url, which are built from
	// several imapsync options at once.
	endpoint
)

// outcome is a disposition together with whatever it needs.
type outcome struct {
	how    disposition
	native string // for translate
	why    string // for ignore and refuse
}

var specPattern = regexp.MustCompile(`^([a-zA-Z0-9_|-]+)(=[sif][@%]?|!)?$`)

// opt builds a table entry from imapsync's own Getopt::Long spelling, so the
// table can be read against the source it copies. A spec this does not
// understand is a programming error and panics at startup rather than quietly
// leaving an option out, which would make it unknown and refuse.
func opt(spec string, on outcome) *option {
	m := specPattern.FindStringSubmatch(spec)
	if m == nil {
		panic(fmt.Sprintf("compat: unreadable option spec %q", spec))
	}

	kind := noValue
	switch m[2] {
	case "!":
		kind = boolean
	case "=s":
		kind = text
	case "=s@":
		kind = textList
	case "=s%":
		kind = textPairs
	case "=i":
		kind = integer
	case "=f":
		kind = number
	}
	return &option{names: strings.Split(m[1], "|"), kind: kind, on: on}
}

func tr(spec, native string) *option { return opt(spec, outcome{how: translate, native: native}) }
func ig(spec, why string) *option    { return opt(spec, outcome{how: ignore, why: why}) }
func no(spec, why string) *option    { return opt(spec, outcome{how: refuse, why: why}) }
func ep(spec string) *option         { return opt(spec, outcome{how: endpoint}) }

// offRefuses gives the negated spelling its own answer. Several options are
// harmless in one direction and not in the other: --syncinternaldates asks for
// what already happens, while --nosyncinternaldates asks for something this
// tool cannot do.
func (o *option) offRefuses(why string) *option {
	o.off = &outcome{how: refuse, why: why}
	return o
}

func (o *option) offIgnores(why string) *option {
	o.off = &outcome{how: ignore, why: why}
	return o
}

func (o *option) offTranslates(native string) *option {
	o.off = &outcome{how: translate, native: native}
	return o
}

// answer returns what this option means in the polarity it was given.
func (o *option) answer(on bool) outcome {
	if !on && o.off != nil {
		return *o.off
	}
	return o.on
}

const (
	notImplemented   = "this tool does not implement it"
	stateInsteadOf   = "the state database records what has been copied, so this is not needed"
	perConnection    = "connection tuning; this tool opens several connections instead, see --source-connections and --dest-connections"
	logToStderr      = "this tool logs to stderr; redirect it, or use --log-json"
	alreadyTheCase   = "this is already what happens"
	changesSelection = "it changes which messages are copied, and this tool would copy all of them instead"
)

// table is every option imapsync accepts, in the order its own two GetOptions
// calls declare them, so the two can be compared by eye.
//
// Nothing is left out. An option missing from here is unknown, and unknown
// refuses — which is right for a typo and wrong for a real imapsync flag,
// because the person typing it would have no idea whether it had been
// considered and rejected or simply forgotten.
func table() []*option {
	groups := [][]*option{
		endpointOptions(),
		selectionOptions(),
		messageOptions(),
		deletionOptions(),
		reportingOptions(),
		machineryOptions(),
	}

	n := 0
	for _, g := range groups {
		n += len(g)
	}

	t := make([]*option, 0, n)
	for _, g := range groups {
		t = append(t, g...)
	}
	return t
}

// endpointOptions are the ones that describe where the mail is.
func endpointOptions() []*option {
	return []*option{
		ep("host1=s"), ep("host2=s"),
		ep("port1=i"), ep("port2=i"),
		ep("user1=s"), ep("user2=s"),
		ep("password1=s"), ep("password2=s"),
		ep("passfile1=s"), ep("passfile2=s"),
		ep("ssl1!"), ep("ssl2!"),
		ep("tls1!"), ep("tls2!"),

		// --office1 is documented as exactly three other options, all three of
		// which this tool has, so it can be honoured in full.
		ep("office1"), ep("office2"),

		// --exchange1 prints a line saying it does nothing. Reproducing that
		// faithfully costs nothing at all.
		ig("exchange1", "it does nothing in imapsync either"),
		ig("exchange2", "it does nothing in imapsync either"),

		no("gmail1", "--gmail1 is a bundle: it sets a byte-rate limit, rewrites [Gmail] out of folder names, "+
			"matches on X-Gmail-Received and synchronises labels. Labels and rate limiting are not implemented here, "+
			"so give --host1/--ssl1 yourself and decide about the rest"),
		no("gmail2", "--gmail2 is a bundle; see --gmail1"),
		no("domino1", "it sets a backslash folder separator and a namespace prefix, neither of which this tool can be told to use"),
		no("domino2", "it sets a backslash folder separator and a namespace prefix, neither of which this tool can be told to use"),

		no("domain1=s", "authentication domains are not implemented; put the whole login in --host1's user part"),
		no("domain2=s", "authentication domains are not implemented; put the whole login in --host2's user part"),
		no("authmech1=s", "this tool authenticates with LOGIN or AUTHENTICATE PLAIN and cannot be told to do otherwise"),
		no("authmech2=s", "this tool authenticates with LOGIN or AUTHENTICATE PLAIN and cannot be told to do otherwise"),
		no("authuser1=s", "authenticating as one user to act as another is not implemented"),
		no("authuser2=s", "authenticating as one user to act as another is not implemented"),
		no("proxyauth1", "PROXYAUTH is not implemented"),
		no("proxyauth2", "PROXYAUTH is not implemented"),
		no("oauthdirect1=s", "OAuth is not implemented"),
		no("oauthdirect2=s", "OAuth is not implemented"),
		no("oauthaccesstoken1=s", "OAuth is not implemented"),
		no("oauthaccesstoken2=s", "OAuth is not implemented"),
		no("oauthrefreshcmd1=s", "OAuth is not implemented"),
		no("oauthrefreshcmd2=s", "OAuth is not implemented"),

		ig("authmd51!", "this tool does not offer CRAM-MD5, so there is nothing to turn off").
			offIgnores("this tool does not offer CRAM-MD5"),
		ig("authmd52!", "this tool does not offer CRAM-MD5, so there is nothing to turn off").
			offIgnores("this tool does not offer CRAM-MD5"),
		ig("authmd5!", "this tool does not offer CRAM-MD5, so there is nothing to turn off").
			offIgnores("this tool does not offer CRAM-MD5"),

		// --sslcheck probes for a TLS port. Turning it off is the only half
		// that reaches this tool, as a refusal to verify certificates.
		ig("sslcheck!", alreadyTheCase).
			offTranslates("--source-insecure --dest-insecure"),
		no("ssl1_ssl_version=s", "the TLS version is negotiated and cannot be pinned"),
		no("ssl2_ssl_version=s", "the TLS version is negotiated and cannot be pinned"),
		no("sslargs1=s%", "arbitrary IO::Socket::SSL arguments have no equivalent"),
		no("sslargs2=s%", "arbitrary IO::Socket::SSL arguments have no equivalent"),

		no("inet4|ipv4", "the address family cannot be pinned"),
		no("inet6|ipv6", "the address family cannot be pinned"),

		tr("timeout=f", "--dial-timeout"),
		ig("timeout1=f", "one timeout covers both ends here; use --dial-timeout"),
		ig("timeout2=f", "one timeout covers both ends here; use --dial-timeout"),

		ig("compress1!", "COMPRESS=DEFLATE is not implemented; this costs bandwidth, not correctness").
			offIgnores("COMPRESS=DEFLATE is not implemented, so it is already off"),
		ig("compress2!", "COMPRESS=DEFLATE is not implemented; this costs bandwidth, not correctness").
			offIgnores("COMPRESS=DEFLATE is not implemented, so it is already off"),
		ig("keepalive1!", perConnection).offIgnores(perConnection),
		ig("keepalive2!", perConnection).offIgnores(perConnection),
		ig("fastio1!", perConnection).offIgnores(perConnection),
		ig("fastio2!", perConnection).offIgnores(perConnection),
		ig("split1=i", perConnection),
		ig("split2=i", perConnection),
		ig("buffersize=i", perConnection),
		ig("reconnectretry1=i", "this tool has its own retry policy"),
		ig("reconnectretry2=i", "this tool has its own retry policy"),
		ig("trylogin!", alreadyTheCase).offIgnores("logging in is not optional"),
	}
}

// selectionOptions choose which folders take part.
func selectionOptions() []*option {
	return []*option{
		tr("folder=s@", "--folder"),
		tr("include=s", "--include"),
		tr("exclude=s", "--exclude"),
		tr("subfolder2=s", "--subfolder2"),
		tr("automap!", "--automap"),
		tr("f1f2=s@", "--map"),

		no("nof1f2", "--f1f2 has no default here to cancel; leave --map off instead"),
		no("noexclude", "--exclude has no default here to cancel; leave it off instead"),
		no("folderrec=s@", "recursive folder selection is not implemented; --include takes a regular expression, "+
			"so --include '^Parent(/|$)' does the same job"),
		no("subfolder1=s", "reading from a subtree of the source is not implemented; select the folders with --folder or --include"),
		no("prefix1=s", "namespace prefixes are detected rather than configured"),
		no("prefix2=s", "namespace prefixes are detected rather than configured"),
		no("sep1=s", "the hierarchy separator is read from the server and cannot be overridden"),
		no("sep2=s", "the hierarchy separator is read from the server and cannot be overridden"),
		no("regextrans2=s@", "folder-name rewriting by regular expression is not implemented; --map renames folders one at a time"),
		no("fixslash2!", "folder-name rewriting is not implemented; --map renames folders one at a time"),
		no("fixInboxINBOX!", "folder-name rewriting is not implemented; --map renames folders one at a time"),
		no("mixfolders!", "this tool always copies into the mapped destination folder"),
		no("skipemptyfolders!", "an empty source folder is always created on the destination, so that the two trees match"),
		no("justautomap!", "printing the folder mapping and stopping is not implemented; --dry-run reports the whole plan instead"),
		no("delete2foldersonly=s", "this tool never deletes folders"),
		no("delete2foldersbutnot=s", "this tool never deletes folders"),

		ig("folderfirst=s", "folders are copied concurrently, so their order is not something this tool can promise"),
		ig("folderlast=s", "folders are copied concurrently, so their order is not something this tool can promise"),
		ig("checkselectable!", alreadyTheCase).offRefuses("unselectable folders are always skipped"),
		ig("checkfoldersexist!", alreadyTheCase).offIgnores(alreadyTheCase),
		ig("justfolderlists!", "folder lists are reported by --dry-run"),
	}
}

// messageOptions decide which messages move and what they look like on arrival.
func messageOptions() []*option {
	return []*option{
		tr("dry!", "--dry-run"),
		tr("resyncflags!", "--resyncflags"),
		tr("subscribe!", "--subscribe"),

		ig("dry1!", "this tool never writes to the source").offIgnores(alreadyTheCase),
		ig("syncinternaldates!", "this tool always gives the copy the source's INTERNALDATE").
			offRefuses("this tool cannot be asked to stamp copies with the time they arrived instead"),
		ig("useuid!", stateInsteadOf).offRefuses(stateInsteadOf),
		ig("uid1!", stateInsteadOf).offIgnores(stateInsteadOf),
		ig("uid2!", stateInsteadOf).offIgnores(stateInsteadOf),
		ig("addheader!", "this tool stamps only the messages that have no Message-ID, and its digest covers a fixed "+
			"list of header fields, so a stamp cannot change how anything is matched").
			offIgnores("messages with a Message-ID are never stamped; ones without it cannot be found again any other way"),
		ig("skipsize!", "message sizes are never compared").offIgnores("message sizes are never compared"),
		ig("allowsizemismatch!", "message sizes are never compared").offIgnores("message sizes are never compared"),
		ig("appendlimit=i", "the server's own APPENDLIMIT is read and obeyed, so there is nothing to state by hand; "+
			"use --max-size to be stricter than the server is"),
		ig("maxlinelength=i", "messages are copied byte for byte, whatever their lines look like"),
		ig("checkmessageexists!", "an interrupted append is always looked for before it is copied again").
			offRefuses("an interrupted append is always looked for before it is copied again, and skipping that check duplicates mail"),
		ig("expungeaftereach!", "deletions are expunged by UID as they are made").offIgnores(alreadyTheCase),
		ig("abletosearch!", alreadyTheCase).offIgnores(alreadyTheCase),
		ig("abletosearch1!", alreadyTheCase).offIgnores(alreadyTheCase),
		ig("abletosearch2!", alreadyTheCase).offIgnores(alreadyTheCase),
		ig("checknoabletosearch!", alreadyTheCase).offIgnores(alreadyTheCase),
		ig("fixcolonbug!", "message bodies are copied byte for byte").offIgnores(alreadyTheCase),
		ig("create_folder_old!", "folders are created the modern way").offIgnores(alreadyTheCase),
		ig("fetch_hash_set=s", "an internal fetch tuning knob with no equivalent"),
		// "sanitize" is imapsync's spelling of its own option, not prose.
		//nolint:misspell // renaming it would stop the option working.
		ig("sanitize!", "output is already safe to redirect to a file").offIgnores(alreadyTheCase),
		ig("showpasswords!", "this tool never prints passwords").offIgnores(alreadyTheCase),

		tr("minsize=i", "--min-size"),
		tr("maxsize=i", "--max-size"),
		tr("maxage=f", "--max-age"),
		tr("minage=f", "--min-age"),
		no("search=s", changesSelection),
		no("search1=s", changesSelection),
		no("search2=s", changesSelection),
		no("skipmess=s", changesSelection),
		no("regexmess=s", "rewriting messages in flight is not implemented, and would make the copy differ from the original"),
		no("noregexmess", "--regexmess has no default here to cancel"),
		no("pipemess=s", "piping messages through a command is not implemented"),
		no("pipemesscheck!", "piping messages through a command is not implemented"),
		no("truncmess=i", "truncating messages is not implemented"),
		no("disarmreadreceipts!", "rewriting messages in flight is not implemented"),
		no("useheader=s@", "the headers a message is identified by are fixed; changing them changes which messages count as duplicates"),
		no("skipheader=s", "the headers a message is identified by are fixed; changing them changes which messages count as duplicates"),
		no("wholeheaderifneeded!", "the headers a message is identified by are fixed"),
		no("messageidnodomain!", "the headers a message is identified by are fixed"),
		no("regexflag=s@", "rewriting flags is not implemented"),
		no("noregexflag", "--regexflag has no default here to cancel"),
		no("filterflags!", "flags are copied as the source has them, and rejected by the destination if it will not take them"),
		no("filterbuggyflags!", "flags are copied as the source has them"),
		no("flagscase!", "flags are copied as the source has them"),
		no("syncflagsaftercopy!", "flags are set as part of the copy, not afterwards"),
		no("synclabels!", "Gmail labels are not implemented"),
		no("resynclabels!", "Gmail labels are not implemented"),
		no("syncacls!", "access control lists are not copied"),
		no("idatefromheader!", "this tool takes INTERNALDATE from the source's INTERNALDATE, not from the Date header"),
		no("syncduplicates!", "duplicate handling is not configurable"),
		no("skipcrossduplicates!", "duplicate handling is not configurable"),
		no("debugcrossduplicates!", "duplicate handling is not configurable"),
		no("maxmessagespersecond=f", "rate limiting is not implemented; reduce --source-connections and --dest-connections instead"),
		no("maxbytespersecond=i", "rate limiting is not implemented; reduce --source-connections and --dest-connections instead"),
		no("maxbytesafter=i", "rate limiting is not implemented; reduce --source-connections and --dest-connections instead"),
		no("maxsleep=f", "rate limiting is not implemented"),
		no("subscribed", "copying only subscribed folders is not implemented; name them with --folder or --include"),
		no("subscribeall|subscribe_all!", "this tool subscribes the folders it creates and leaves every other folder alone"),
	}
}

// deletionOptions are the ones that can lose mail.
func deletionOptions() []*option {
	return []*option{
		tr("delete2!", "--delete2"),

		ig("expunge2!", "--delete2 expunges by UID as part of deleting").
			offRefuses("this tool cannot mark messages deleted and leave them; --delete2 expunges what it deletes"),
		ig("uidexpunge2!", "--delete2 always expunges by UID, so that a message the owner marked deleted by hand survives").
			offRefuses("plain EXPUNGE would purge messages the owner marked deleted by hand, so this tool will not do it"),

		no("delete1!", "this tool opens the source read-only and will not delete from it"),
		no("expunge1|expunge!", "this tool opens the source read-only and will not expunge it"),
		no("delete2duplicates!", "deleting duplicates on the destination is not implemented"),
		no("delete1emptyfolders", "this tool never deletes folders"),
		no("delete2folders!", "this tool never deletes folders"),
	}
}

// reportingOptions change what gets said, not what gets done.
func reportingOptions() []*option {
	return []*option{
		tr("debug!", "--log-level debug"),
		tr("debuglist!", "--log-level debug"),
		tr("debugcontent!", "--log-level debug"),
		tr("debugflags!", "--log-level debug"),
		tr("debugfolders!", "--log-level debug"),
		tr("debugmemory!", "--log-level debug"),
		tr("debugenv!", "--log-level debug"),
		tr("debugsig!", "--log-level debug"),
		tr("debuglabels!", "--log-level debug"),
		tr("debugcache!", "--log-level debug"),
		tr("debugimap!", "--trace"),
		tr("debugimap1!", "--trace"),
		tr("debugimap2!", "--trace"),

		ig("debugssl=i", "TLS negotiation is not traced separately"),
		ig("log!", logToStderr).offIgnores(logToStderr),
		ig("loglogfile!", logToStderr).offIgnores(logToStderr),
		ig("logfile=s", logToStderr),
		ig("logdir=s", logToStderr),
		ig("tail!", logToStderr).offIgnores(logToStderr),
		ig("foldersizes!", "folder sizes are not counted up front; the run reports what it did as it goes").
			offIgnores("folder sizes are not counted up front"),
		ig("foldersizesatend!", "folder sizes are not counted").offIgnores(alreadyTheCase),
		ig("errorsmax=i", "a run gives up after a run of failures with nothing copied in between, which is not a count of errors"),
		ig("errorsdump!", "failures are logged as they happen").offIgnores(alreadyTheCase),
		ig("releasecheck!", "this tool does not phone home").offIgnores(alreadyTheCase),
		ig("modulesversion!", "there are no modules to report").offIgnores(alreadyTheCase),
		ig("id!", "the IMAP ID command is not sent").offIgnores(alreadyTheCase),
		ig("dockercontext!", "this is imapsync's own packaging").offIgnores(alreadyTheCase),
		ig("emailreportfrom=s", "this tool does not send mail; redirect its output instead"),
		ig("emailreport1!", "this tool does not send mail").offIgnores(alreadyTheCase),
		ig("emailreport2!", "this tool does not send mail").offIgnores(alreadyTheCase),

		// probe answers all three of these better than imapsync does, reporting
		// capabilities and how many connections the server will actually give
		// you. It takes one endpoint at a time, which is why this points at it
		// rather than translating: running it twice is clearer than a flag
		// that silently checked only one end.
		no("justbanner!", "run: imapsync-go probe --url imaps://user@host"),
		no("justlogin!", "run: imapsync-go probe --url imaps://user@host --password-env VAR"),
		no("justconnect!", "run: imapsync-go probe --url imaps://user@host"),

		no("justfolders!", "copying folders without their messages is not implemented; --dry-run reports the plan instead"),
		no("justfoldersizes!", "folder sizes are not counted"),
		no("version", "run: imapsync-go --version"),
		no("help", "run: imapsync-go compat --help"),
	}
}

// machineryOptions are about how the program runs.
func machineryOptions() []*option {
	return []*option{
		ig("tmpdir=s", "this tool writes no temporary files").
			offIgnores(alreadyTheCase),
		ig("pidfile=s", "this tool writes no pid file"),
		ig("pidfilelocking!", "this tool writes no pid file").offIgnores(alreadyTheCase),
		ig("usecache!", stateInsteadOf).offIgnores("the state database is not optional"),
		ig("cacheaftercopy!", stateInsteadOf).offRefuses("the state database is written as the copy proceeds and cannot be turned off"),

		no("abort", "there is no run to abort; stop the process"),
		no("abortbyfile", "there is no run to abort; stop the process"),
		no("exitwhenover=i", "stopping after a number of bytes is not implemented, and ignoring it would transfer more than asked"),
		no("exitonload!", "stopping when the machine is busy is not implemented"),
		no("sigexit=s@", "signal handling is not configurable; SIGINT and SIGTERM stop the run cleanly"),
		no("sigreconnect=s@", "signal handling is not configurable"),
		no("sigignore=s@", "signal handling is not configurable"),
		no("simulong=f", "an imapsync test hook"),
		no("debugsleep=f", "an imapsync test hook"),
		no("memorystress!", "an imapsync test hook"),
		no("tests!", "an imapsync test hook"),
		no("testsdebug!", "an imapsync test hook"),
		no("testsunit=s@", "an imapsync test hook"),
		no("testslive!", "an imapsync test hook"),
		no("testslive2!", "an imapsync test hook"),
		no("testslive6!", "an imapsync test hook"),
		no("var=s@", "an imapsync internal"),
		no("extra=s@", "an imapsync internal"),
		no("f1f2h=s%", "an imapsync internal; use --f1f2 to map folders"),
	}
}

// NativeFlags lists every flag this table can emit.
//
// It exists so a test elsewhere can check each one against the command that
// has to accept it. A native flag renamed on one side of the package boundary
// would otherwise produce a translation that looks perfectly reasonable and
// dies at the far end complaining about a flag the user never typed.
func NativeFlags() []string {
	seen := map[string]bool{}
	var out []string
	for _, o := range table() {
		for _, out2 := range []*outcome{&o.on, o.off} {
			if out2 == nil || out2.how != translate {
				continue
			}
			for _, w := range strings.Fields(out2.native) {
				name := strings.TrimPrefix(strings.SplitN(w, "=", 2)[0], "--")
				if strings.HasPrefix(w, "--") && !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}
