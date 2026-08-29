package prometheusinference

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tokenRefreshMargin re-requests a token this long before it actually expires, so a request never
// races an about-to-expire token. Safe against the shortest role TTL (5 min for role "app"): a
// margin this small never causes more than one extra request per real expiry.
const tokenRefreshMargin = 15 * time.Second

// TokenSource obtains and caches OAuth2 client_credentials tokens from Prometheus's auth-service.
// There is no refresh_token grant (confirmed with the platform team) — a "refresh" is just
// re-POSTing /oauth2/token with the same client_id/client_secret, which is exactly what happens
// here once the cached token is within tokenRefreshMargin of expiring, or after Invalidate is
// called following a 401 from the gateway.
type TokenSource struct {
	AuthURL      string // e.g. http://127.0.0.1:9000
	ClientID     string
	ClientSecret string
	// Scope determines both general capability (inference:read, inference:stream) and which
	// specific models this client may use (model:<id>) — the auth-service embeds it in the
	// issued JWT and the gateway checks it on every request. See docs/adr/0004.
	Scope      string
	HTTPClient *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

func (t *TokenSource) httpClient() *http.Client {
	if t.HTTPClient != nil {
		return t.HTTPClient
	}
	return http.DefaultClient
}

// Token returns a valid bearer token, requesting a new one if the cached one is missing or close
// to expiry.
func (t *TokenSource) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Now().Before(t.expiresAt.Add(-tokenRefreshMargin)) {
		return t.token, nil
	}
	return t.refreshLocked(ctx)
}

// Invalidate discards the cached token, forcing the next Token call to request a fresh one. Call
// this after a request comes back 401 (see Client.doAuthenticated) — the auth-service's tokens are
// stateless and short-lived, so a 401 usually just means the cached one expired slightly early
// relative to our clock, not that the credentials themselves are bad.
func (t *TokenSource) Invalidate() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.token = ""
}

func (t *TokenSource) refreshLocked(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {t.ClientID},
		"client_secret": {t.ClientSecret},
	}
	if t.Scope != "" {
		form.Set("scope", t.Scope)
	}

	reqURL := strings.TrimRight(t.AuthURL, "/") + "/oauth2/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("prometheus_inference: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("prometheus_inference: requesting token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("prometheus_inference: reading token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("prometheus_inference: token request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var parsed tokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("prometheus_inference: decoding token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("prometheus_inference: token response had no access_token")
	}

	t.token = parsed.AccessToken
	t.expiresAt = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second)
	return t.token, nil
}
