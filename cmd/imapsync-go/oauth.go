package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/hilli/imapsync-go/internal/oauthx"
)

type oauthLoginFlags struct {
	clientFile   string
	clientID     string
	clientSecret string
	authURL      string
	tokenURL     string
	scope        []string

	out      string
	keychain string
	stdout   bool

	account string
	timeout time.Duration
	noOpen  bool
}

func newOAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oauth",
		Short: "Obtain and store OAuth credentials",
		Long: "Obtain and store OAuth credentials.\n\n" +
			"Providers that speak XOAUTH2 hand out access tokens that expire within the\n" +
			"hour, which is shorter than a migration. `oauth login` walks the consent\n" +
			"flow once and stores the long-lived half, so a sync can mint access tokens\n" +
			"for itself as it runs.",
	}
	cmd.AddCommand(newOAuthLoginCmd())
	return cmd
}

func newOAuthLoginCmd() *cobra.Command {
	var f oauthLoginFlags

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Consent once and store a refresh credential",
		Long: "Consent once in a browser and store the resulting refresh credential.\n\n" +
			"The credential is one JSON document. Give it to a sync with\n" +
			"--source-oauth-refresh-file, --source-oauth-refresh-keychain or\n" +
			"--source-oauth-refresh-env, or the matching --dest- flags, or name it\n" +
			"under `oauth.refresh` in a configuration file.\n\n" +
			"You need an OAuth client of your own. Neither provider allows a shared\n" +
			"one: Google's mail scope is restricted and needs verification plus an\n" +
			"annual security assessment, and Microsoft's IMAP scope needs tenant\n" +
			"admin consent whoever asks. Register a desktop or native client, allow\n" +
			"the redirect http://localhost, and pass its details here.",
		Example: "  # Gmail, from the JSON the Google console hands you\n" +
			"  imapsync-go oauth login --client-file client_secret.json \\\n" +
			"      --scope https://mail.google.com/ --out gmail.oauth.json\n\n" +
			"  # Exchange Online, storing the result in the macOS keychain\n" +
			"  imapsync-go oauth login \\\n" +
			"      --client-id $APP_ID \\\n" +
			"      --auth-url https://login.microsoftonline.com/$TENANT/oauth2/v2.0/authorize \\\n" +
			"      --token-url https://login.microsoftonline.com/$TENANT/oauth2/v2.0/token \\\n" +
			"      --scope https://outlook.office365.com/IMAP.AccessAsUser.All \\\n" +
			"      --scope offline_access \\\n" +
			"      --keychain imapsync-work",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runOAuthLogin(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}

	cmd.Flags().StringVar(&f.clientFile, "client-file", "", "JSON client credential downloaded from the Google console")
	cmd.Flags().StringVar(&f.clientID, "client-id", "", "OAuth client ID")
	cmd.Flags().StringVar(&f.clientSecret, "client-secret", "", "OAuth client secret, if the provider issues one")
	cmd.Flags().StringVar(&f.authURL, "auth-url", "", "provider authorization endpoint")
	cmd.Flags().StringVar(&f.tokenURL, "token-url", "", "provider token endpoint")
	cmd.Flags().StringArrayVar(&f.scope, "scope", nil, "scope to request, repeatable")

	cmd.Flags().StringVar(&f.out, "out", "", "write the credential to this file, created readable only by you")
	cmd.Flags().StringVar(&f.keychain, "keychain", "", "store the credential in the macOS keychain under this service name")
	cmd.Flags().BoolVar(&f.stdout, "stdout", false, "print the credential on standard output")

	cmd.Flags().StringVar(&f.account, "account", "", "mailbox to consent as, offered to the provider as a hint")
	cmd.Flags().DurationVar(&f.timeout, "timeout", oauthx.DefaultConsentTimeout, "how long to wait for consent")
	cmd.Flags().BoolVar(&f.noOpen, "no-open", false, "print the URL instead of opening a browser")

	cmd.MarkFlagsMutuallyExclusive("client-file", "client-id")
	cmd.MarkFlagsMutuallyExclusive("out", "keychain", "stdout")

	return cmd
}

func runOAuthLogin(ctx context.Context, out io.Writer, f oauthLoginFlags) error {
	client, err := loginClient(f)
	if err != nil {
		return err
	}
	if len(f.scope) == 0 {
		return errors.New("no --scope given; Gmail wants https://mail.google.com/ and " +
			"Exchange Online wants https://outlook.office365.com/IMAP.AccessAsUser.All and offline_access")
	}

	// Asked for rather than defaulted. A credential good for months should not
	// reach a terminal, a shell history or a scrollback buffer by accident.
	if f.out == "" && f.keychain == "" && !f.stdout {
		return errors.New("say where the credential should go: --out FILE, --keychain SERVICE or --stdout")
	}

	client.Scopes = f.scope
	if err := client.Validate(); err != nil {
		return fmt.Errorf("%w; pass --client-file with the JSON your provider hands you, "+
			"or name every field with --client-id, --auth-url and --token-url", err)
	}

	opts := oauthx.LoginOptions{
		Prompt:    func(url string) { fmt.Fprintf(out, "Open this URL to consent:\n\n%s\n\n", url) },
		Timeout:   f.timeout,
		LoginHint: f.account,
	}
	if f.noOpen {
		// Not nil, which means "use the platform opener".
		opts.OpenBrowser = func(context.Context, string) error { return nil }
	}

	creds, err := oauthx.Login(ctx, client, opts)
	if err != nil {
		return err
	}

	blob, err := json.MarshalIndent(creds, "", "  ") //nolint:gosec // G117: serialising the credential is what this command is for
	if err != nil {
		return fmt.Errorf("encoding the credential: %w", err)
	}
	blob = append(blob, '\n')

	// The keychain gets the compact form. security(1) reads a secret the way
	// it reads a typed password -- one line, twice -- so a document with
	// newlines in it would be truncated at the first.
	compact, err := json.Marshal(creds) //nolint:gosec // G117: serialising the credential is what this command is for
	if err != nil {
		return fmt.Errorf("encoding the credential: %w", err)
	}

	return storeCredential(ctx, out, f, blob, compact)
}

// loginClient reads the client details from a downloaded file or from flags.
func loginClient(f oauthLoginFlags) (oauthx.Client, error) {
	if f.clientFile != "" {
		raw, err := os.ReadFile(f.clientFile) //nolint:gosec // reading the file the user named is the feature
		if err != nil {
			return oauthx.Client{}, fmt.Errorf("reading the client file: %w", err)
		}
		client, err := oauthx.ParseClientFile(raw)
		if err != nil {
			return oauthx.Client{}, err
		}
		// Flags still win over the file, since the file carries no scope and a
		// tenant-specific endpoint may need overriding.
		if f.authURL != "" {
			client.AuthURL = f.authURL
		}
		if f.tokenURL != "" {
			client.TokenURL = f.tokenURL
		}
		return client, nil
	}

	client := oauthx.Client{
		ID:       f.clientID,
		Secret:   f.clientSecret,
		AuthURL:  f.authURL,
		TokenURL: f.tokenURL,
	}
	return client, nil
}

func storeCredential(ctx context.Context, out io.Writer, f oauthLoginFlags, blob, compact []byte) error {
	switch {
	case f.stdout:
		_, err := out.Write(blob)
		return err

	case f.out != "":
		// 0600 because this is the long-lived half of the credential.
		if err := os.WriteFile(f.out, blob, 0o600); err != nil {
			return fmt.Errorf("writing the credential: %w", err)
		}
		fmt.Fprintf(out, "Credential written to %s.\n\n"+
			"Use it with --source-oauth-refresh-file %s (or --dest-).\n", f.out, f.out)
		return nil

	default:
		if err := keychainStore(ctx, f.keychain, compact); err != nil {
			return err
		}
		fmt.Fprintf(out, "Credential stored in the keychain as %s.\n\n"+
			"Use it with --source-oauth-refresh-keychain %s (or --dest-).\n", f.keychain, f.keychain)
		return nil
	}
}

// keychainStore writes the credential to the macOS keychain, replacing any
// item already held under that service name.
//
// Neither obvious way to call security(1) works. The password cannot go in the
// argument vector, which `ps` can read, and this credential outlives the
// process by months. `-w` with no value prompts instead, but it reads the
// prompt the way getpass does and silently truncates at 128 bytes -- a real
// credential is closer to 350, so it would store a corrupt document and only
// fail on the first sync.
//
// So: interactive mode, where the whole command line arrives on standard
// input, with the value hex-encoded. Hex because the interactive parser splits
// on whitespace and honours quotes, and a client secret is free to contain
// both.
func keychainStore(ctx context.Context, service string, secret []byte) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("--keychain works on macOS, not %s; use --out instead", runtime.GOOS)
	}

	cmd := exec.CommandContext(ctx, "security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n",
		os.Getenv("USER"), service, hex.EncodeToString(secret)))

	outBytes, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("storing the credential in the keychain: %w: %s", err, outBytes)
	}
	// Interactive mode reports a failed sub-command in its output and still
	// exits zero, so the exit status alone cannot be believed.
	if bytes.Contains(outBytes, []byte("returned")) || bytes.Contains(outBytes, []byte("Usage:")) {
		return fmt.Errorf("storing the credential in the keychain: %s", bytes.TrimSpace(outBytes))
	}
	return nil
}
