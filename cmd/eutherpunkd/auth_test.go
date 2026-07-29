package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIImageToolRequiresMediaScope(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/eutherpunk/chat/stream", nil)
	request.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	request.Header.Set("X-EutherPunk-Client-Capabilities", "image-tool")

	chatOnly := request.WithContext(context.WithValue(request.Context(), authContextKey{}, authPrincipal{
		User:     "nichlas",
		Scopes:   []string{"eutherpunk:chat"},
		AuthMode: "cli_token",
	}))
	if clientSupportsImageTool(chatOnly) {
		t.Fatal("chat-only token gained the image tool")
	}

	withMedia := request.WithContext(context.WithValue(request.Context(), authContextKey{}, authPrincipal{
		User:     "nichlas",
		Scopes:   []string{"eutherpunk:chat", "eutherpunk:media"},
		AuthMode: "cli_token",
	}))
	if !clientSupportsImageTool(withMedia) {
		t.Fatal("media-scoped CLI did not gain the image tool")
	}
}

func TestAuthStoreDeviceFlowRefreshAndRevoke(t *testing.T) {
	store, err := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0)
	store.now = func() time.Time { return now }
	verifier := strings.Repeat("v", 43)

	grant, deviceCode, err := store.startDeviceGrant("Workstation", pkceChallenge(verifier))
	if err != nil {
		t.Fatal(err)
	}
	_, csrf, err := store.authorizationPage(grant.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	principal := authPrincipal{
		User:     "nichlas",
		Scopes:   []string{"eutherpunk:*"},
		AuthMode: "eutherid_browser",
	}
	if err := store.approve(grant.UserCode, csrf, principal, now.Unix()); err != nil {
		t.Fatal(err)
	}
	tokens, code, err := store.exchange(deviceCode, verifier)
	if err != nil || code != "" {
		t.Fatalf("exchange = %#v, %q, %v", tokens, code, err)
	}
	if tokens.User != "nichlas" || len(tokens.Scopes) != 2 ||
		tokens.Scopes[0] != "eutherpunk:chat" || tokens.Scopes[1] != "eutherpunk:media" {
		t.Fatalf("tokens = %#v", tokens)
	}
	got, ok := store.authenticate(tokens.AccessToken)
	if !ok || got.User != "nichlas" ||
		!hasScope(got, "eutherpunk:chat") || !hasScope(got, "eutherpunk:media") {
		t.Fatalf("principal = %#v, %v", got, ok)
	}

	rotated, err := store.refresh(tokens.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.RefreshToken == tokens.RefreshToken || rotated.AccessToken == tokens.AccessToken {
		t.Fatal("refresh must rotate both credentials")
	}
	if _, err := store.refresh(tokens.RefreshToken); err == nil {
		t.Fatal("old refresh token must be one-time")
	}
	if err := store.revoke(rotated.AccessToken); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.authenticate(rotated.AccessToken); ok {
		t.Fatal("revoked access token still works")
	}
	if _, err := store.refresh(rotated.RefreshToken); err == nil {
		t.Fatal("revocation must remove the token family")
	}

	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{deviceCode, tokens.AccessToken, tokens.RefreshToken, rotated.AccessToken, rotated.RefreshToken} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("auth state persisted a plaintext credential")
		}
	}
}

func TestAuthStoreRequiresApprovalAndPKCE(t *testing.T) {
	store, err := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("a", 43)
	grant, deviceCode, err := store.startDeviceGrant("CLI", pkceChallenge(verifier))
	if err != nil {
		t.Fatal(err)
	}
	if _, code, err := store.exchange(deviceCode, verifier); err == nil || code != "authorization_pending" {
		t.Fatalf("pending exchange code = %q, err = %v", code, err)
	}
	_, csrf, err := store.authorizationPage(grant.UserCode)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.approve(grant.UserCode, csrf, authPrincipal{User: "nichlas", AuthMode: "password"}, 1); err == nil {
		t.Fatal("non-EutherID browser session was accepted")
	}
	if err := store.approve(grant.UserCode, csrf, authPrincipal{User: "nichlas", AuthMode: "eutherid_browser"}, 1); err != nil {
		t.Fatal(err)
	}
	if _, code, err := store.exchange(deviceCode, strings.Repeat("b", 43)); err == nil || code != "invalid_grant" {
		t.Fatalf("wrong PKCE code = %q, err = %v", code, err)
	}
}

func TestAuthMiddlewareEnforcesScopeAndAdmin(t *testing.T) {
	store, err := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	tokens, err := store.issueLocked("nichlas", []string{"eutherpunk:chat"}, "")
	if err == nil {
		err = store.saveLocked()
	}
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := newAuthService(true, "https://example.invalid", "http://127.0.0.1:1/api/app/status", store)
	if err != nil {
		t.Fatal(err)
	}
	okHandler := func(w http.ResponseWriter, r *http.Request) {
		principal, _ := principalFromContext(r.Context())
		writeJSON(w, http.StatusOK, principal)
	}

	req := httptest.NewRequest(http.MethodGet, "/chat", nil)
	rec := httptest.NewRecorder()
	auth.protect("eutherpunk:chat", false, okHandler)(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/chat", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	rec = httptest.NewRecorder()
	auth.protect("eutherpunk:chat", false, okHandler)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	rec = httptest.NewRecorder()
	auth.protect("eutherpunk:settings", false, okHandler)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("settings status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	rec = httptest.NewRecorder()
	auth.protect("eutherpunk:chat", true, okHandler)(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin status = %d", rec.Code)
	}
}

func TestBrowserPrincipalRequiresEutherIDAssurance(t *testing.T) {
	verified := true
	oxide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "euther_session=test" {
			http.Error(w, "missing cookie", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated":      true,
			"user":               "nichlas",
			"admin":              true,
			"eutherIdVerified":   verified,
			"eutherIdVerifiedAt": 1234,
		})
	}))
	defer oxide.Close()
	store, _ := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	auth, err := newAuthService(true, "https://example.invalid", oxide.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	req.Header.Set("Cookie", "euther_session=test")
	principal, at, err := auth.browserPrincipal(req)
	if err != nil || principal.User != "nichlas" || !principal.Admin || at != 1234 {
		t.Fatalf("principal = %#v, at=%d, err=%v", principal, at, err)
	}
	verified = false
	if _, _, err := auth.browserPrincipal(req); err == nil {
		t.Fatal("browser session without EutherID assurance was accepted")
	}
}

func TestBrowserAuthorizationPageApprovesDeviceFlow(t *testing.T) {
	oxide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated":      true,
			"user":               "nichlas",
			"admin":              false,
			"eutherIdVerified":   true,
			"eutherIdVerifiedAt": 1234,
		})
	}))
	defer oxide.Close()
	store, _ := loadAuthStore(filepath.Join(t.TempDir(), "auth.json"))
	auth, err := newAuthService(true, "https://example.invalid", oxide.URL, store)
	if err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("z", 43)
	grant, deviceCode, err := store.startDeviceGrant("Work PC", pkceChallenge(verifier))
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/eutherpunk/cli/authorize?user_code="+url.QueryEscape(grant.UserCode), nil)
	req.Header.Set("Cookie", "euther_session=test")
	rec := httptest.NewRecorder()
	auth.handleAuthorizeGet()(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorize page = %d: %s", rec.Code, rec.Body.String())
	}
	csrf := between(rec.Body.String(), `name="csrf" value="`, `"`)
	if csrf == "" {
		t.Fatal("authorization page did not include CSRF")
	}

	form := url.Values{
		"user_code": {grant.UserCode},
		"csrf":      {csrf},
		"decision":  {"approve"},
	}
	req = httptest.NewRequest(http.MethodPost, "/eutherpunk/cli/authorize", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cookie", "euther_session=test")
	req.Header.Set("Origin", "https://example.invalid")
	rec = httptest.NewRecorder()
	auth.handleAuthorizePost()(rec, req)
	if rec.Code != http.StatusOK {
		body, _ := io.ReadAll(rec.Body)
		t.Fatalf("approval = %d: %s", rec.Code, body)
	}
	if _, code, err := store.exchange(deviceCode, verifier); err != nil || code != "" {
		t.Fatalf("exchange after approval code=%q err=%v", code, err)
	}
}

func between(value, prefix, suffix string) string {
	start := strings.Index(value, prefix)
	if start < 0 {
		return ""
	}
	start += len(prefix)
	end := strings.Index(value[start:], suffix)
	if end < 0 {
		return ""
	}
	return value[start : start+end]
}
