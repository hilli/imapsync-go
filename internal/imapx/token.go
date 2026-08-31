package imapx

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// DefaultTokenTimeout bounds a minting command.
//
// It is not a formality. Commit 584f87e found that resolving a keychain secret
// could block for ever because security(1) may put a prompt in front of the
// user, and `az login` does the same. This one is worse than the keychain case
// because it is consulted mid-run, with a pool of workers blocked behind it.
const DefaultTokenTimeout = 30 * time.Second

// tokenSource produces a fresh access token.
type tokenSource func(ctx context.Context) (string, error)

// tokenCredential holds one access token and re-mints it when the server says
// it is no longer good.
//
// Expiry is discovered by being refused rather than by watching a clock. A
// lifetime is a claim by the issuer, and this project has twice had to unlearn
// trusting that kind of claim: the connection ceiling moved between probes,
// and the state database cannot be believed over the filesystem. The cost is
// one refused login per expiry, per pool.
type tokenCredential struct {
	source tokenSource

	mu   sync.Mutex
	held string
}

func (c *tokenCredential) Mechanism() Mechanism { return MechanismXOAUTH2 }

// Secret answers with the held token, minting one if there is none.
//
// Minting happens under the lock so that a pool opening forty connections at
// once runs the command once rather than forty times. The blocked dials are
// not delayed by it in any meaningful sense: not one of them could proceed
// without the token they are waiting for.
func (c *tokenCredential) Secret(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.held != "" {
		return c.held, nil
	}

	token, err := c.source(ctx)
	if err != nil {
		return "", err
	}
	c.held = token
	return token, nil
}

// Refresh replaces the token the server refused, unless someone else already
// has.
//
// The comparison against stale is the whole of the single-flight. When a pool
// meets an expiry every worker offers the same dead token; the first to arrive
// mints, and the rest find what they were holding is no longer what is held
// and take the replacement without minting anything.
func (c *tokenCredential) Refresh(ctx context.Context, stale string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.held != "" && c.held != stale {
		return c.held, true, nil
	}

	fresh, err := c.source(ctx)
	if err != nil {
		return "", false, err
	}

	// A token just minted and already refused is the provider's answer rather
	// than an expiry, and offering it again would ask the same question.
	if fresh == stale {
		return "", false, nil
	}

	c.held = fresh
	return fresh, true, nil
}

// CommandToken returns a credential that mints access tokens by running a
// command and reading the token from its standard output.
//
// A command rather than a built-in OAuth2 client: every provider-specific
// detail -- client id, secret, tenant, refresh token, scope -- then lives
// outside this tool and outside its configuration file. No OAuth library, no
// client secret at rest, and no callback listener.
func CommandToken(command string, timeout time.Duration) Credential {
	if timeout <= 0 {
		timeout = DefaultTokenTimeout
	}

	return &tokenCredential{source: func(ctx context.Context) (string, error) {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// Through a shell because the command arrives as one line of a
		// configuration file, quoting and all, and splitting it here would
		// break the first argument containing a space.
		cmd := exec.CommandContext(runCtx, "sh", "-c", command) //nolint:gosec // running the command the user configured is the feature

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			// Stderr only. Stdout is the token, and a command that fails after
			// printing one would otherwise put it in the logs.
			if reason := strings.TrimSpace(stderr.String()); reason != "" {
				return "", fmt.Errorf("running the token command: %w: %s", err, reason)
			}
			return "", fmt.Errorf("running the token command: %w", err)
		}

		token := strings.TrimSpace(stdout.String())
		if token == "" {
			return "", fmt.Errorf("the token command printed nothing")
		}
		return token, nil
	}}
}

// FileToken returns a credential that reads its access token from a file,
// re-reading it whenever the one it holds is refused.
//
// This is for a token maintained by something else -- an agent that refreshes
// it on a timer, a mounted secret. Re-reading is all that "renew" has to mean,
// so it needs no special handling of its own.
func FileToken(path string) Credential {
	return &tokenCredential{source: func(context.Context) (string, error) {
		raw, err := os.ReadFile(path) //nolint:gosec // reading the file the user named is the feature
		if err != nil {
			return "", fmt.Errorf("reading the token file: %w", err)
		}

		token := strings.TrimSpace(string(raw))
		if token == "" {
			return "", fmt.Errorf("the token file %s is empty", path)
		}
		return token, nil
	}}
}
