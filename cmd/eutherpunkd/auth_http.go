package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type authService struct {
	required       bool
	publicURL      string
	oxideStatusURL string
	oxideHost      string
	store          *authStore
	client         *http.Client
}

type oxideSessionStatus struct {
	Authenticated      bool   `json:"authenticated"`
	User               string `json:"user"`
	Admin              bool   `json:"admin"`
	EutherIDVerified   bool   `json:"eutherIdVerified"`
	EutherIDVerifiedAt int64  `json:"eutherIdVerifiedAt"`
}

func newAuthService(required bool, publicURL, oxideStatusURL string, store *authStore) (*authService, error) {
	publicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	parsedPublic, err := url.Parse(publicURL)
	if err != nil || parsedPublic.Scheme != "https" || parsedPublic.Host == "" {
		return nil, errors.New("EutherPunk public URL must be an https URL")
	}
	parsedStatus, err := url.Parse(strings.TrimSpace(oxideStatusURL))
	if err != nil || (parsedStatus.Scheme != "http" && parsedStatus.Scheme != "https") || parsedStatus.Host == "" {
		return nil, errors.New("EutherOxide status URL must be an http or https URL")
	}
	return &authService{
		required:       required,
		publicURL:      publicURL,
		oxideStatusURL: parsedStatus.String(),
		oxideHost:      parsedPublic.Host,
		store:          store,
		client: &http.Client{
			Timeout: 5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (auth *authService) browserPrincipal(r *http.Request) (authPrincipal, int64, error) {
	cookie := strings.TrimSpace(r.Header.Get("Cookie"))
	if cookie == "" {
		return authPrincipal{}, 0, errors.New("EutherID login required")
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, auth.oxideStatusURL, nil)
	if err != nil {
		return authPrincipal{}, 0, err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = auth.oxideHost
	resp, err := auth.client.Do(req)
	if err != nil {
		return authPrincipal{}, 0, fmt.Errorf("validate EutherID session: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return authPrincipal{}, 0, errors.New("EutherID login required")
	}
	var status oxideSessionStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&status); err != nil {
		return authPrincipal{}, 0, fmt.Errorf("decode EutherID session status: %w", err)
	}
	if !status.Authenticated || !status.EutherIDVerified || status.User == "" || status.EutherIDVerifiedAt <= 0 {
		return authPrincipal{}, 0, errors.New("an EutherID-verified browser session is required")
	}
	return authPrincipal{
		User:     status.User,
		Admin:    status.Admin,
		Scopes:   []string{"eutherpunk:*"},
		AuthMode: "eutherid_browser",
	}, status.EutherIDVerifiedAt, nil
}

func (auth *authService) authenticateRequest(r *http.Request) (authPrincipal, bool) {
	if token := bearerToken(r); token != "" {
		return auth.store.authenticate(token)
	}
	principal, _, err := auth.browserPrincipal(r)
	return principal, err == nil
}

func (auth *authService) protect(scope string, admin bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.required {
			principal := authPrincipal{
				User:     "local",
				Admin:    true,
				Scopes:   []string{"eutherpunk:*"},
				AuthMode: "disabled",
			}
			next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal)))
			return
		}
		principal, ok := auth.authenticateRequest(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="eutherpunk"`)
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "authentication_required",
			})
			return
		}
		if admin && !principal.Admin {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "admin_required"})
			return
		}
		if principal.AuthMode == "cli_token" && !hasScope(principal, scope) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "insufficient_scope"})
			return
		}
		if principal.AuthMode == "eutherid_browser" &&
			r.Method != http.MethodGet && r.Method != http.MethodHead &&
			!auth.validBrowserOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "same_origin_required"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal)))
	}
}

func (auth *authService) validBrowserOrigin(r *http.Request) bool {
	origin := strings.TrimRight(strings.TrimSpace(r.Header.Get("Origin")), "/")
	if origin != "" {
		return origin == auth.publicURL
	}
	referer := strings.TrimSpace(r.Header.Get("Referer"))
	return strings.HasPrefix(referer, auth.publicURL+"/")
}

func (auth *authService) handleDeviceStart() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			ClientName    string `json:"client_name"`
			CodeChallenge string `json:"code_challenge"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		grant, deviceCode, err := auth.store.startDeviceGrant(input.ClientName, input.CodeChallenge)
		if err != nil {
			writeError(w, http.StatusTooManyRequests, err)
			return
		}
		verify := auth.publicURL + "/eutherpunk/cli/authorize?user_code=" + url.QueryEscape(grant.UserCode)
		writeJSON(w, http.StatusCreated, map[string]any{
			"device_code":               deviceCode,
			"user_code":                 grant.UserCode,
			"verification_uri":          auth.publicURL + "/eutherpunk/cli/authorize",
			"verification_uri_complete": verify,
			"expires_in":                int(authDeviceTTL / time.Second),
			"interval":                  2,
		})
	}
}

func (auth *authService) handleAuthorizeGet() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := normalizeUserCode(r.URL.Query().Get("user_code"))
		principal, verifiedAt, err := auth.browserPrincipal(r)
		if err != nil {
			loginURL := auth.publicURL + "/login"
			body := fmt.Sprintf(
				`Du måste först <a href="%s">logga in med EutherID</a> i den här webbläsaren och sedan öppna CLI-länken igen.`,
				html.EscapeString(loginURL),
			)
			writeAuthHTML(w, http.StatusUnauthorized, "EutherID krävs", body)
			return
		}
		grant, csrf, err := auth.store.authorizationPage(code)
		if err != nil {
			writeAuthHTML(w, http.StatusGone, "Begäran har gått ut", "Starta /auth login i EutherPunk igen.")
			return
		}
		_ = verifiedAt
		body := fmt.Sprintf(`
<p>Inloggad som <strong>%s</strong> med EutherID.</p>
<p>Vill du tillåta <strong>%s</strong> att använda EutherPunk-chatten?</p>
<p class="scope">Behörighet: chatta och skapa media som %s. Ingen admin-, lokal fil- eller kommandobehörighet.</p>
<form method="post" action="/eutherpunk/cli/authorize">
  <input type="hidden" name="user_code" value="%s">
  <input type="hidden" name="csrf" value="%s">
  <button type="submit" name="decision" value="approve">Godkänn CLI</button>
  <button class="secondary" type="submit" name="decision" value="deny">Neka</button>
</form>`,
			html.EscapeString(principal.User),
			html.EscapeString(grant.ClientName),
			html.EscapeString(principal.User),
			html.EscapeString(grant.UserCode),
			html.EscapeString(csrf),
		)
		writeAuthHTML(w, http.StatusOK, "Godkänn EutherPunk CLI", body)
	}
}

func (auth *authService) handleAuthorizePost() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !auth.validBrowserOrigin(r) {
			writeAuthHTML(w, http.StatusForbidden, "Nekad", "Godkännandet måste komma från EutherPunks egen sida.")
			return
		}
		if err := r.ParseForm(); err != nil {
			writeAuthHTML(w, http.StatusBadRequest, "Ogiltig begäran", "Formuläret kunde inte läsas.")
			return
		}
		principal, verifiedAt, err := auth.browserPrincipal(r)
		if err != nil {
			writeAuthHTML(w, http.StatusUnauthorized, "EutherID krävs", html.EscapeString(err.Error()))
			return
		}
		if r.FormValue("decision") != "approve" {
			if err := auth.store.deny(r.FormValue("user_code"), r.FormValue("csrf")); err != nil {
				writeAuthHTML(w, http.StatusBadRequest, "Kunde inte neka", html.EscapeString(err.Error()))
				return
			}
			writeAuthHTML(w, http.StatusOK, "Nekad", "CLI:t fick ingen åtkomst.")
			return
		}
		if err := auth.store.approve(r.FormValue("user_code"), r.FormValue("csrf"), principal, verifiedAt); err != nil {
			writeAuthHTML(w, http.StatusBadRequest, "Kunde inte godkänna", html.EscapeString(err.Error()))
			return
		}
		writeAuthHTML(w, http.StatusOK, "CLI godkänt", "Du kan stänga den här fliken och återgå till EutherPunk.")
	}
}

func (auth *authService) handleToken() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			DeviceCode   string `json:"device_code"`
			CodeVerifier string `json:"code_verifier"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		tokens, code, err := auth.store.exchange(input.DeviceCode, input.CodeVerifier)
		if err != nil {
			status := http.StatusBadRequest
			if code == "authorization_pending" {
				status = http.StatusPreconditionRequired
			} else if code == "access_denied" {
				status = http.StatusForbidden
			} else if code == "expired_token" {
				status = http.StatusGone
			} else if code == "server_error" {
				status = http.StatusInternalServerError
			}
			writeJSON(w, status, map[string]any{"error": code, "message": err.Error()})
			return
		}
		writeTokenResponse(w, tokens)
	}
}

func (auth *authService) handleRefresh() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		tokens, err := auth.store.refresh(input.RefreshToken)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid_grant"})
			return
		}
		writeTokenResponse(w, tokens)
	}
}

func (auth *authService) handleMe() http.HandlerFunc {
	return auth.protect("eutherpunk:chat", false, func(w http.ResponseWriter, r *http.Request) {
		principal, _ := principalFromContext(r.Context())
		writeJSON(w, http.StatusOK, principal)
	})
}

func (auth *authService) handleRevoke() http.HandlerFunc {
	return auth.protect("eutherpunk:chat", false, func(w http.ResponseWriter, r *http.Request) {
		if token := bearerToken(r); token != "" {
			if err := auth.store.revoke(token); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
}

func bearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
}

func writeTokenResponse(w http.ResponseWriter, tokens issuedTokens) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  tokens.AccessToken,
		"refresh_token": tokens.RefreshToken,
		"token_type":    "Bearer",
		"expires_in":    int(authAccessTTL / time.Second),
		"expires_at":    tokens.ExpiresAt,
		"user":          tokens.User,
		"scopes":        tokens.Scopes,
	})
}

func writeAuthHTML(w http.ResponseWriter, status int, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="sv"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>%s</title><style>
body{margin:0;background:#07120b;color:#e9fff0;font:16px system-ui;display:grid;min-height:100vh;place-items:center}
main{width:min(560px,calc(100%% - 40px));background:#0d1e13;border:1px solid #2d6b3e;border-radius:18px;padding:28px;box-shadow:0 18px 60px #0008}
h1{color:#5cff5c;margin-top:0}.scope{color:#a9cdb2}button{border:0;border-radius:10px;padding:12px 18px;background:#43df62;color:#031006;font-weight:800;cursor:pointer}
button.secondary{margin-left:8px;background:#243d2b;color:#d9ffe2}a{color:#5cff5c}
</style></head><body><main><h1>%s</h1>%s</main></body></html>`,
		html.EscapeString(title), html.EscapeString(title), body)
}
