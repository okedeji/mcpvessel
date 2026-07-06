package githubauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubGitHub answers the two device-flow endpoints. polls counts access-token
// requests so a test can make the first poll pending and the second succeed.
func stubGitHub(t *testing.T, accessToken string, pendingFirst bool) string {
	t.Helper()
	polls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_code": "dev-1", "user_code": "WXYZ-1234",
				"verification_uri": "https://github.com/login/device", "interval": 1, "expires_in": 60,
			})
		case "/login/oauth/access_token":
			polls++
			if pendingFirst && polls == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": accessToken})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestDeviceFlow_ReturnsAccessToken(t *testing.T) {
	var shown Prompt
	base := stubGitHub(t, "gh-access-abc", false)
	tok, err := DeviceFlow(context.Background(), Config{
		ClientID: "cid",
		BaseURL:  base,
		Notify:   func(p Prompt) { shown = p },
	})
	if err != nil {
		t.Fatalf("DeviceFlow: %v", err)
	}
	if tok != "gh-access-abc" {
		t.Errorf("token = %q, want the access token", tok)
	}
	if shown.UserCode != "WXYZ-1234" || shown.VerificationURI == "" {
		t.Errorf("prompt = %+v, want the user code and URI", shown)
	}
}

func TestDeviceFlow_PollsThroughPending(t *testing.T) {
	base := stubGitHub(t, "gh-access-abc", true)
	tok, err := DeviceFlow(context.Background(), Config{ClientID: "cid", BaseURL: base})
	if err != nil {
		t.Fatalf("DeviceFlow: %v", err)
	}
	if tok != "gh-access-abc" {
		t.Errorf("token = %q, want the access token after pending", tok)
	}
}

func TestDeviceFlow_NoClientIDFailsClosed(t *testing.T) {
	_, err := DeviceFlow(context.Background(), Config{})
	if err == nil || !strings.Contains(err.Error(), "client id") {
		t.Fatalf("err = %v, want a missing-client-id error", err)
	}
}
