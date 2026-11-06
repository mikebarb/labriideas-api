package main

// =============================================================================
// ADMIN AUTHENTICATION
// =============================================================================
//
// Bearer-token only. Two credential classes resolve to the same identity model:
//
//   1. STATIC TOKENS  (CLI tools) — from ADMIN_API_TOKENS, resolved at startup
//   2. SESSION TOKENS  (browser)  — minted at login, held in RAM, expire
//
// Both are presented as:  Authorization: Bearer <token>
//
// Why no cookies: the PWA (Cloudflare Pages) and this API (Render) are on
// different registrable domains. A cross-site cookie is a third-party cookie,
// which Safari blocks and Chrome is phasing out. Bearer tokens are immune.
// Tradeoff: a browser-held token is readable by JS, so an XSS flaw could
// exfiltrate it. Accepted — the admin UI renders no untrusted HTML, and
// sessions are short-lived (SESSION_TTL_HOURS, default 24).
//
// Every credential resolves to a UserID so administrative actions can be
// attributed in logs. No database: sessions live in RAM and a server restart
// invalidates all browser sessions (static CLI tokens are unaffected).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// contextKey is a private type so request-context keys cannot collide with
// keys set by other packages.
type contextKey string

const userIDContextKey contextKey = "userID"

// Session is the record stored for each authenticated browser login.
type Session struct {
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// credential pairs a UserID with the SHA-256 hash of a secret. The plaintext
// secret is never retained after startup.
type credential struct {
	UserID string
	Hash   [32]byte
}

var (
	sessionStore        sync.Map     // token -> Session (dynamic browser sessions)
	passwordCredentials []credential // password hash -> UserID
	tokenCredentials    []credential // static token hash -> UserID
)

// =============================================================================
// CONFIGURATION
// =============================================================================

// sessionTTL is how long a browser session stays valid.
// Override with SESSION_TTL_HOURS; defaults to 24 hours.
func sessionTTL() time.Duration {
	if h := os.Getenv("SESSION_TTL_HOURS"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 {
			return time.Duration(n) * time.Hour
		}
	}
	return 24 * time.Hour
}

// envList reads a comma-separated env var into a trimmed, non-empty slice.
func envList(key string) []string {
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// adminPasswords returns the raw tagged password entries ("uid:secret").
// Used by the startup guard in main().
func adminPasswords() []string { return envList("ADMIN_PASSWORDS") }

// adminAPITokens returns the raw tagged static-token entries ("uid:secret").
func adminAPITokens() []string { return envList("ADMIN_API_TOKENS") }

// parseTaggedSecrets converts ["uid:secret", ...] into credential records,
// storing only the SHA-256 hash of each secret. Entries without exactly one
// colon are skipped — a deliberate choice so a stray comma cannot silently
// register a malformed credential.
func parseTaggedSecrets(entries []string) []credential {
	out := make([]credential, 0, len(entries))
	for _, entry := range entries {
		parts := strings.Split(entry, ":")
		if len(parts) != 2 {
			log.Printf("WARNING: skipping malformed credential entry (expected uid:secret)")
			continue
		}
		uid := strings.TrimSpace(parts[0])
		secret := parts[1] // NOT trimmed: whitespace may be intentional
		if uid == "" || secret == "" {
			log.Printf("WARNING: skipping credential entry with empty uid or secret")
			continue
		}
		out = append(out, credential{UserID: uid, Hash: sha256.Sum256([]byte(secret))})
	}
	return out
}

// InitAuth loads passwords and static tokens into memory. Call once, at
// startup, before serving traffic. It logs counts but never logs any secret.
func InitAuth() {
	passwordCredentials = parseTaggedSecrets(adminPasswords())
	tokenCredentials = parseTaggedSecrets(adminAPITokens())

	log.Printf("Auth initialized: %d password(s), %d static token(s)",
		len(passwordCredentials), len(tokenCredentials))
}

// =============================================================================
// CREDENTIAL VERIFICATION
// =============================================================================

// matchCredential reports whether the SHA-256 of candidate matches any entry,
// returning the associated UserID. Comparison is constant-time against every
// entry: the loop never returns early, so a match on the first entry costs the
// same as no match at all. This closes the timing side-channel that a
// short-circuiting loop would open.
func matchCredential(candidate string, creds []credential) (string, bool) {
	candidateHash := sha256.Sum256([]byte(candidate))
	userID := ""
	matched := false
	for _, cred := range creds {
		if subtle.ConstantTimeCompare(candidateHash[:], cred.Hash[:]) == 1 {
			userID = cred.UserID
			matched = true
			// Deliberately no break — keep timing uniform across the list.
		}
	}
	return userID, matched
}

// =============================================================================
// LOGIN RATE LIMITING
// =============================================================================
// More than loginMaxFailures failures from one IP inside loginWindow locks
// that IP out until the window rolls over.

const (
	loginMaxFailures = 10
	loginWindow      = 15 * time.Minute
)

type loginAttempts struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

var loginGuard = &loginAttempts{failures: make(map[string][]time.Time)}

func (l *loginAttempts) tooManyFailures(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-loginWindow)
	kept := l.failures[ip][:0] // in-place filter over the same backing array
	for _, t := range l.failures[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.failures[ip] = kept
	return len(kept) >= loginMaxFailures
}

func (l *loginAttempts) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[ip] = append(l.failures[ip], time.Now())
}

func (l *loginAttempts) clear(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

// clientIP extracts the caller's address for rate limiting AND statistics.
// Behind Render's proxy the direct peer is the load balancer, so
// X-Forwarded-For is the authoritative source. Only trustworthy while the
// server is not directly exposed; do not reuse this helper if that ever
// changes.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.Index(fwd, ","); i >= 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// =============================================================================
// TOKEN HANDLING
// =============================================================================

// newSessionToken returns a 256-bit random, URL-safe token.
func newSessionToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// bearerToken extracts the token from "Authorization: Bearer <token>".
// Returns "" if the header is absent or malformed.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(prefix):])
}

// authorize resolves the request to a UserID.
//
// Returns (userID, allowed). Checks the static CLI token list first, then
// dynamic browser sessions. Static tokens are not stored in sessionStore, so
// a restart never invalidates them.
func authorize(r *http.Request) (string, bool) {
	token := bearerToken(r)
	if token == "" {
		return "", false
	}

	// 1. Static CLI token?
	if userID, ok := matchCredential(token, tokenCredentials); ok {
		return userID, true
	}

	// 2. Dynamic browser session? Sessions are keyed by the raw token value,
	// so the plaintext token IS the lookup key here (never stored elsewhere).
	if val, ok := sessionStore.Load(token); ok {
		sess := val.(Session)
		if time.Now().Before(sess.ExpiresAt) {
			return sess.UserID, true
		}
		sessionStore.Delete(token) // expired — purge on access
	}

	return "", false
}

// =============================================================================
// HTTP HANDLERS
// =============================================================================

// loginHandler exchanges a password for a dynamic session token.
// POST /api/auth/login  {"password": "..."}
// 200 -> {"token": "...", "expiresAt": "RFC3339", "userID": "..."}
func loginHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ip := clientIP(r)
	if loginGuard.tooManyFailures(ip) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "too many attempts, try again later"})
		return
	}

	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	userID, ok := matchCredential(req.Password, passwordCredentials)
	if !ok {
		loginGuard.recordFailure(ip)
		log.Printf("Failed admin login from %s", ip)
		// Stats: failed login attempt. IP ONLY — the attempted password is
		// never recorded, anywhere.
		stats.Record(Event{Kind: "admin_login_failed", IP: ip})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid credentials"})
		return
	}

	loginGuard.clear(ip)
	// Stats: successful login, attributed to the resolved UserID.
	stats.Record(Event{Kind: "admin_login", UserID: userID, IP: ip})

	token, err := newSessionToken()
	if err != nil {
		log.Printf("Failed to generate session token: %v", err)
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}

	expiry := time.Now().Add(sessionTTL())
	sessionStore.Store(token, Session{
		UserID:    userID,
		CreatedAt: time.Now(),
		ExpiresAt: expiry,
	})

	log.Printf("Admin login succeeded: user=%s from %s", userID, ip)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"token":     token,
		"expiresAt": expiry.Format(time.RFC3339),
		"userID":    userID,
	})
}

// authStatusHandler reports whether the presented token is valid. The UI calls
// it on load to decide whether to show admin affordances. It never returns
// 401 — "not admin" is a normal state, not an error.
// GET /api/auth/status   (Authorization: Bearer <token>)
func authStatusHandler(w http.ResponseWriter, r *http.Request) {
	userID, ok := authorize(r)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"isAdmin": ok,
		"userID":  userID, // empty string when not admin
	})
}

// logoutHandler revokes the caller's dynamic session token server-side.
// Static CLI tokens cannot be revoked here — they are config, not state.
// POST /api/auth/logout   (Authorization: Bearer <token>)
func logoutHandler(w http.ResponseWriter, r *http.Request) {
	// CHANGED: resolve the identity BEFORE deleting the session, so the
	// logout can be attributed in stats. A logout presented with a static
	// CLI token is a no-op server-side (nothing to delete) but still
	// resolves — and is recorded — as that user.
	userID, _ := authorize(r)
	if token := bearerToken(r); token != "" {
		sessionStore.Delete(token)
	}
	if userID != "" {
		stats.Record(Event{Kind: "admin_logout", UserID: userID, IP: clientIP(r)})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"isAdmin": false})
}

// =============================================================================
// MIDDLEWARE
// =============================================================================

// authMiddleware guards every write-capable endpoint. It runs inside
// corsMiddleware, so CORS headers are already set and preflight OPTIONS
// requests have been answered before this executes.
//
// On success the resolved UserID is placed in the request context, so
// downstream handlers can attribute actions without re-parsing credentials:
//
//	userID, _ := r.Context().Value(userIDContextKey).(string)
func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := authorize(r)
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}

		// Never log the token itself — only the resolved identity.
		log.Printf("[ADMIN] user=%s %s %s", userID, r.Method, r.URL.Path)

		// Stats: the structured, persistent version of the console line
		// above. RecordAdminRequest internally skips operational endpoints
		// (crawl orchestration — machine overhead, not admin actions; the
		// crawl-status poller would otherwise flood the stats stream).
		stats.RecordAdminRequest(userID, r.Method, r.URL.Path, clientIP(r))

		ctx := context.WithValue(r.Context(), userIDContextKey, userID)
		next(w, r.WithContext(ctx))
	}
}
