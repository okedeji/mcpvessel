// Package githubauth runs GitHub's OAuth device flow for MCP Registry
// publishing: a proven GitHub identity maps to the io.github.<user>
// namespace. Device flow, not a browser redirect, because a CLI has no
// loopback server to catch one.
package githubauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultBaseURL = "https://github.com"

// scope is the minimum for identity verification: the login name, nothing
// about repositories or organizations.
const scope = "read:user"

// Prompt carries what the operator must do to authorize the device.
type Prompt struct {
	UserCode        string
	VerificationURI string
}

// Config parameterizes the flow. BaseURL defaults to github.com; Notify is
// called once with the code to display before polling begins.
type Config struct {
	ClientID string
	BaseURL  string
	Notify   func(Prompt)
}

// DeviceFlow requests a device code, shows it through cfg.Notify, and polls
// until the operator authorizes or the attempt expires. A missing client id
// is an error; there is no anonymous device flow.
func DeviceFlow(ctx context.Context, cfg Config) (string, error) {
	if cfg.ClientID == "" {
		return "", fmt.Errorf("no GitHub OAuth client id; set %s to a registered app", "AGENTCAGE_GITHUB_CLIENT_ID")
	}
	base := cfg.BaseURL
	if base == "" {
		base = defaultBaseURL
	}

	code, err := requestDeviceCode(ctx, base, cfg.ClientID)
	if err != nil {
		return "", err
	}
	if cfg.Notify != nil {
		cfg.Notify(Prompt{UserCode: code.UserCode, VerificationURI: code.VerificationURI})
	}
	return poll(ctx, base, cfg.ClientID, code)
}

type deviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	Interval        int    `json:"interval"`
	ExpiresIn       int    `json:"expires_in"`
}

func requestDeviceCode(ctx context.Context, base, clientID string) (deviceCode, error) {
	var out deviceCode
	err := postForm(ctx, base+"/login/device/code", url.Values{
		"client_id": {clientID},
		"scope":     {scope},
	}, &out)
	if err != nil {
		return deviceCode{}, fmt.Errorf("requesting a device code: %w", err)
	}
	if out.DeviceCode == "" {
		return deviceCode{}, fmt.Errorf("requesting a device code: github returned no device code")
	}
	return out, nil
}

// poll asks for the token on GitHub's stated interval, honoring slow_down and
// authorization_pending. The deadline is GitHub's own expires_in.
func poll(ctx context.Context, base, clientID string, code deviceCode) (string, error) {
	interval := time.Duration(max(code.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(max(code.ExpiresIn, 1)) * time.Second)

	for {
		var out struct {
			AccessToken string `json:"access_token"`
			Error       string `json:"error"`
		}
		if err := postForm(ctx, base+"/login/oauth/access_token", url.Values{
			"client_id":   {clientID},
			"device_code": {code.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}, &out); err != nil {
			return "", fmt.Errorf("polling for authorization: %w", err)
		}

		switch {
		case out.AccessToken != "":
			return out.AccessToken, nil
		case out.Error == "authorization_pending":
			// Wait the interval and retry.
		case out.Error == "slow_down":
			interval += 5 * time.Second
		case out.Error == "":
			return "", fmt.Errorf("polling for authorization: github returned no token and no error")
		default:
			return "", fmt.Errorf("authorization failed: %s", out.Error)
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("authorization timed out; run 'agentcage login mcp-registry' again")
		}
		if err := sleep(ctx, interval); err != nil {
			return "", err
		}
	}
}

func postForm(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("github returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// sleep waits d or returns early on context cancellation.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
