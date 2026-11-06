// pkg/client/auth_test.go

//------------------------------------------------------------------------
// Test the client's authentication flow
// Re-auth only fires for session credentials (ownsSession == true, password path).
// With a static token there is nothing to renew, so a restart can't produce a 401.
// To exercise it, two options:
// 1. Racy but real: comment out PUBLISHER_API_TOKEN and ADMIN_API_TOKENS from the
//    client's environment, keep ADMIN_PASSWORDS, confirm the banner says
//    (session expires …), then run a real (non-dry) upload of a handful of files and
//    restart the server while uploads are in flight. GetSignedUploadURL / UpdateMetadata
//    should 401, re-login once, and continue.
//
// 2. Deterministic (better): a Go test with httptest, which removes the timing problem
//    entirely:
//    From the publisher directory, run:
//      go test ./pkg/client -run TestDoReauthenticatesOn401 -v
//
//   That asserts the whole contract: one 401 → exactly one login → one successful replay,
//   no loop. A second test with N goroutines calling concurrently would additionally
//   prove the single-flight lock keeps logins at 1 rather than N — worth having, since
//  that's the property protecting the rate limiter.
//------------------------------------------------------------------------

package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDoReauthenticatesOn401(t *testing.T) {
	var logins, protected int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			atomic.AddInt32(&logins, 1)
			json.NewEncoder(w).Encode(map[string]string{
				"token": "fresh-token", "userID": "dev",
				"expiresAt": "2099-01-01T00:00:00Z",
			})
		case "/api/update-metadata":
			// Reject anything but the renewed token — i.e. simulate a server
			// that restarted and forgot the old session.
			if r.Header.Get("Authorization") != "Bearer fresh-token" {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			atomic.AddInt32(&protected, 1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, apiToken: "stale-session", password: "pw", ownsSession: true}
	if err := c.UpdateMetadata("x.mp3", map[string]string{"title": "t"}); err != nil {
		t.Fatalf("expected transparent re-auth, got %v", err)
	}
	if logins != 1 {
		t.Fatalf("expected exactly 1 login, got %d", logins)
	}
	if protected != 1 {
		t.Fatalf("expected the retried call to succeed once, got %d", protected)
	}
}
