package client

// =============================================================================
// ADMIN AUTHENTICATION (client side)
// =============================================================================
//
// The server accepts two credential classes, both presented as
// "Authorization: Bearer <token>":
//
//   1. STATIC TOKENS  — from the server's ADMIN_API_TOKENS. Permanent, survive
//                       restarts. Intended for unattended runs.
//   2. SESSION TOKENS — minted by POST /api/auth/login from a password, held
//                       in server RAM, expire after SESSION_TTL_HOURS (24h
//                       default), and are LOST whenever the server restarts
//                       (e.g. a Render redeploy mid-run).
//
// EnsureAuthenticated resolves a credential in this order, first hit wins:
//
//   1. PUBLISHER_API_TOKEN  — explicit single token; preserves existing setups
//                             and lets a launcher hand a session down to a
//                             child process through the environment.
//   2. ADMIN_API_TOKENS     — first "uid:secret" entry. The same names the
//                             server uses, so one .env can configure both.
//   3. ADMIN_PASSWORDS      — first "uid:secret" entry, exchanged for a session.
//   4. Interactive prompt   — no echo, held in memory only, never written to
//                             disk and never logged.
//
// ADMIN_USER (optional) selects a uid from the uid:secret lists when more
// than one entry is present. Without it, the first entry wins.
//
// SECRET HYGIENE: the password and token are never printed, never passed as
// command-line arguments (visible in the process list and shell history), and
// never written by client.Printf — which fans out to the log FILE as well as
// the terminal (see logger.go). Only the resolved identity is ever displayed.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// errNoCredential is returned by refreshSession when a 401 arrives but no
// password is held in memory to re-authenticate with.
var errNoCredential = errors.New(
	"session expired and no credential is available to renew it — re-run the tool",
)

// SetToken installs a token directly (NewFromEnv, or a launcher passing a
// session down to a child process).
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apiToken = token
}

// tokenSnapshot returns the current token under lock. Used by do() so the
// token that was actually sent can be compared with the one after a refresh.
func (c *Client) tokenSnapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.apiToken
}

// Identity describes who the tool is running as, for the startup banner.
// Never includes the token or password itself — only the resolved UserID and,
// when known, which credential source won (so an operator can tell why a
// password prompt did or did not appear).
func (c *Client) Identity() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	src := c.credSource
	if src == "" {
		src = "unknown source"
	}

	switch {
	case c.apiToken == "":
		return "Not authenticated — public endpoints only"
	case c.ownsSession && !c.expiresAt.IsZero():
		return fmt.Sprintf("Authenticated as %q via %s (session expires %s)",
			c.user, src, c.expiresAt.UTC().Format("2006-01-02 15:04 UTC"))
	case c.user == "":
		// PUBLISHER_API_TOKEN is a bare token with no uid attached, so no
		// identity can be reported — say so rather than printing "".
		return fmt.Sprintf("Authenticated via %s (identity unknown — single token)", src)
	default:
		return fmt.Sprintf("Authenticated as %q via %s (static token)", c.user, src)
	}
}

// EnsureAuthenticated obtains a usable credential, or explains why it can't.
// Safe to call once per run; individual admin calls then need no auth logic.
func (c *Client) EnsureAuthenticated() error {
	// Already supplied by NewFromEnv or by a launcher via SetToken.
	if c.tokenSnapshot() != "" {
		return nil
	}

	wantUID := osGetenv("ADMIN_USER")

	// 1. Single explicit token — existing setups keep working unchanged.
	if tok := strings.TrimSpace(osGetenv("PUBLISHER_API_TOKEN")); tok != "" {
		c.mu.Lock()
		c.apiToken = tok
		c.credSource = "PUBLISHER_API_TOKEN"
		c.mu.Unlock()
		return nil
	}

	// 2. Static token from the server-style list.
	if raw := osGetenv("ADMIN_API_TOKENS"); raw != "" {
		if secret, uid := pickCredential(raw, wantUID); secret != "" {
			c.mu.Lock()
			c.apiToken = secret
			c.user = uid
			c.credSource = "ADMIN_API_TOKENS"
			c.mu.Unlock()
			return nil
		}
	}

	// 3. Password exchanged for a session (unattended).
	if raw := osGetenv("ADMIN_PASSWORDS"); raw != "" {
		if secret, _ := pickCredential(raw, wantUID); secret != "" {
			if err := c.Login(secret); err != nil {
				return err
			}
			c.mu.Lock()
			c.credSource = "ADMIN_PASSWORDS"
			c.mu.Unlock()
			return nil
		}
	}

	// 4. Interactive fallback — a distributed .exe has no .env.
	password, err := promptPassword()
	if err != nil {
		return err
	}
	if err := c.Login(password); err != nil {
		return err
	}
	c.mu.Lock()
	c.credSource = "interactive prompt"
	c.mu.Unlock()
	return nil
}

// Login exchanges a password for a session token and stores the identity.
// The password is retained in memory only, so a mid-run 401 (server restart)
// can be renewed transparently — see refreshSession.
func (c *Client) Login(password string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginLocked(password)
}

// loginLocked performs the login exchange. Caller must hold c.mu.
// It deliberately does NOT route through do(): a login cannot itself be
// authenticated, and doing so would risk recursion on a 401.
func (c *Client) loginLocked(password string) error {
	payload, _ := json.Marshal(map[string]string{"password": password})

	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/auth/login", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("could not build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Distinguish "server unreachable" from "credentials rejected" — the
		// two have completely different fixes.
		return fmt.Errorf("cannot reach the server at %s (%s): %w", c.BaseURL, EnvironmentLabel, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The server returns {"error": "..."} with a human-readable reason.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var apiErr struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &apiErr)
		msg := strings.TrimSpace(apiErr.Error)
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		if msg == "" {
			msg = fmt.Sprintf("server returned %d", resp.StatusCode)
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("login rejected (401): %s — check ADMIN_PASSWORDS", msg)
		case http.StatusTooManyRequests:
			return fmt.Errorf("login rate-limited (429): %s — wait 15 minutes", msg)
		}
		return fmt.Errorf("login failed (%d): %s", resp.StatusCode, msg)
	}

	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expiresAt"`
		UserID    string `json:"userID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("login succeeded but the response could not be read: %w", err)
	}
	if out.Token == "" {
		return errors.New("login succeeded but the server returned no token")
	}

	c.apiToken = out.Token
	c.user = out.UserID
	c.ownsSession = true
	c.password = password // memory only — enables transparent renewal
	if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
		c.expiresAt = t
	}
	return nil
}

// refreshSession renews an invalidated session, serialised so parallel workers
// hit the login endpoint once rather than N times (the server allows only 10
// failures per 15 minutes per IP).
func (c *Client) refreshSession(stale string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Another worker refreshed while this one waited for the lock.
	if c.apiToken != "" && c.apiToken != stale {
		return c.apiToken, nil
	}
	if c.password == "" {
		return "", errNoCredential
	}
	if err := c.loginLocked(c.password); err != nil {
		return "", err
	}
	return c.apiToken, nil
}

// Logout revokes a session token server-side. Static tokens are config, not
// state, so there is nothing to revoke and this is a no-op for them.
func (c *Client) Logout() error {
	c.mu.Lock()
	if !c.ownsSession || c.apiToken == "" {
		c.mu.Unlock()
		return nil
	}
	token := c.apiToken
	c.mu.Unlock()

	req, err := http.NewRequest(http.MethodPost, c.BaseURL+"/api/auth/logout", nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+token)
		client := &http.Client{Timeout: 10 * time.Second}
		if resp, doErr := client.Do(req); doErr == nil {
			resp.Body.Close()
		}
	}

	c.mu.Lock()
	c.apiToken = ""
	c.user = ""
	c.ownsSession = false
	c.expiresAt = time.Time{}
	c.mu.Unlock()
	return nil
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

// pickCredential extracts a secret from a comma-separated "uid:secret" list
// (the shape ADMIN_PASSWORDS / ADMIN_API_TOKENS already use on the server).
// wantUID selects a specific entry; otherwise the first valid one wins.
// A bare value with no colon is accepted verbatim, so a single-entry env var
// such as ADMIN_PASSWORDS=hunter2 also works.
func pickCredential(raw, wantUID string) (secret, uid string) {
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		colon := strings.Index(entry, ":")
		if colon < 0 {
			if wantUID == "" {
				return entry, ""
			}
			continue
		}
		u := strings.TrimSpace(entry[:colon])
		s := entry[colon+1:] // NOT trimmed — whitespace may be intentional
		if s == "" {
			continue
		}
		if wantUID == "" || strings.EqualFold(u, wantUID) {
			return s, u
		}
	}
	return "", ""
}

// promptPassword reads a password without echo when stdin is a terminal, and
// falls back to a single line read when input is piped (scripted runs).
func promptPassword() (string, error) {
	fmt.Fprint(os.Stderr, "Admin password: ")

	if term.IsTerminal(int(os.Stdin.Fd())) {
		raw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr) // ReadPassword swallows the newline
		if err != nil {
			return "", fmt.Errorf("could not read password: %w", err)
		}
		return string(raw), nil
	}

	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		fmt.Fprintln(os.Stderr)
		return strings.TrimRight(scanner.Text(), "\r\n"), nil
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("could not read password: %w", err)
	}
	return "", errors.New("no password supplied on stdin")
}
