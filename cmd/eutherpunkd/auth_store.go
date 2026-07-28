package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	authDeviceTTL  = 5 * time.Minute
	authAccessTTL  = 60 * time.Minute
	authRefreshTTL = 30 * 24 * time.Hour
	maxAuthGrants  = 20
)

type authPrincipal struct {
	User      string   `json:"user"`
	Admin     bool     `json:"admin"`
	Scopes    []string `json:"scopes"`
	AuthMode  string   `json:"auth_mode"`
	ExpiresAt int64    `json:"expires_at,omitempty"`
}

type authContextKey struct{}

func principalFromContext(ctx context.Context) (authPrincipal, bool) {
	principal, ok := ctx.Value(authContextKey{}).(authPrincipal)
	return principal, ok
}

type deviceGrant struct {
	UserCode       string `json:"user_code"`
	DeviceHash     string `json:"device_hash"`
	CodeChallenge  string `json:"code_challenge"`
	ClientName     string `json:"client_name"`
	CSRFHash       string `json:"csrf_hash,omitempty"`
	Status         string `json:"status"`
	User           string `json:"user,omitempty"`
	Admin          bool   `json:"admin,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	ExpiresAt      int64  `json:"expires_at"`
	ApprovedAt     int64  `json:"approved_at,omitempty"`
	EutherIDAt     int64  `json:"eutherid_at,omitempty"`
	ApprovalDevice string `json:"approval_device,omitempty"`
}

type accessRecord struct {
	FamilyID  string   `json:"family_id"`
	User      string   `json:"user"`
	Scopes    []string `json:"scopes"`
	IssuedAt  int64    `json:"issued_at"`
	ExpiresAt int64    `json:"expires_at"`
}

type refreshRecord struct {
	FamilyID  string   `json:"family_id"`
	User      string   `json:"user"`
	Scopes    []string `json:"scopes"`
	IssuedAt  int64    `json:"issued_at"`
	ExpiresAt int64    `json:"expires_at"`
}

type authDiskState struct {
	Version int                      `json:"version"`
	Grants  map[string]deviceGrant   `json:"grants"`
	Access  map[string]accessRecord  `json:"access"`
	Refresh map[string]refreshRecord `json:"refresh"`
}

type authStore struct {
	mu    sync.Mutex
	path  string
	state authDiskState
	now   func() time.Time
}

type issuedTokens struct {
	AccessToken  string
	RefreshToken string
	User         string
	Scopes       []string
	ExpiresAt    int64
}

func loadAuthStore(path string) (*authStore, error) {
	store := &authStore{
		path: path,
		now:  time.Now,
		state: authDiskState{
			Version: 1,
			Grants:  map[string]deviceGrant{},
			Access:  map[string]accessRecord{},
			Refresh: map[string]refreshRecord{},
		},
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &store.state); err != nil {
		return nil, fmt.Errorf("parse CLI auth state: %w", err)
	}
	if store.state.Version != 1 {
		return nil, fmt.Errorf("unsupported CLI auth state version %d", store.state.Version)
	}
	if store.state.Grants == nil {
		store.state.Grants = map[string]deviceGrant{}
	}
	if store.state.Access == nil {
		store.state.Access = map[string]accessRecord{}
	}
	if store.state.Refresh == nil {
		store.state.Refresh = map[string]refreshRecord{}
	}
	store.mu.Lock()
	changed := store.pruneLocked(store.now().Unix())
	if changed {
		err = store.saveLocked()
	}
	store.mu.Unlock()
	return store, err
}

func (store *authStore) startDeviceGrant(clientName, codeChallenge string) (deviceGrant, string, error) {
	clientName = strings.TrimSpace(clientName)
	if clientName == "" || len(clientName) > 100 {
		return deviceGrant{}, "", errors.New("client_name must be 1-100 characters")
	}
	if !validPKCEChallenge(codeChallenge) {
		return deviceGrant{}, "", errors.New("invalid PKCE code_challenge")
	}
	deviceCode, err := randomAuthToken(32)
	if err != nil {
		return deviceGrant{}, "", err
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().Unix()
	store.pruneLocked(now)
	if len(store.state.Grants) >= maxAuthGrants {
		return deviceGrant{}, "", errors.New("too many pending CLI authorizations")
	}
	userCode, err := store.uniqueUserCodeLocked()
	if err != nil {
		return deviceGrant{}, "", err
	}
	grant := deviceGrant{
		UserCode:      userCode,
		DeviceHash:    hashAuthSecret(deviceCode),
		CodeChallenge: codeChallenge,
		ClientName:    clientName,
		Status:        "pending",
		CreatedAt:     now,
		ExpiresAt:     now + int64(authDeviceTTL/time.Second),
	}
	store.state.Grants[userCode] = grant
	if err := store.saveLocked(); err != nil {
		delete(store.state.Grants, userCode)
		return deviceGrant{}, "", err
	}
	return grant, deviceCode, nil
}

func (store *authStore) authorizationPage(userCode string) (deviceGrant, string, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().Unix()
	store.pruneLocked(now)
	grant, ok := store.state.Grants[normalizeUserCode(userCode)]
	if !ok || grant.Status != "pending" || grant.ExpiresAt <= now {
		return deviceGrant{}, "", errors.New("authorization request is invalid or expired")
	}
	csrf, err := randomAuthToken(24)
	if err != nil {
		return deviceGrant{}, "", err
	}
	grant.CSRFHash = hashAuthSecret(csrf)
	store.state.Grants[grant.UserCode] = grant
	if err := store.saveLocked(); err != nil {
		return deviceGrant{}, "", err
	}
	return grant, csrf, nil
}

func (store *authStore) approve(userCode, csrf string, principal authPrincipal, eutherIDAt int64) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().Unix()
	store.pruneLocked(now)
	code := normalizeUserCode(userCode)
	grant, ok := store.state.Grants[code]
	if !ok || grant.Status != "pending" || grant.ExpiresAt <= now {
		return errors.New("authorization request is invalid or expired")
	}
	if grant.CSRFHash == "" || !equalAuthHash(grant.CSRFHash, hashAuthSecret(csrf)) {
		return errors.New("invalid authorization confirmation")
	}
	if principal.User == "" || principal.AuthMode != "eutherid_browser" || eutherIDAt <= 0 {
		return errors.New("an EutherID-verified browser session is required")
	}
	grant.Status = "approved"
	grant.User = principal.User
	grant.Admin = false
	grant.ApprovedAt = now
	grant.EutherIDAt = eutherIDAt
	grant.CSRFHash = ""
	store.state.Grants[code] = grant
	return store.saveLocked()
}

func (store *authStore) deny(userCode, csrf string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().Unix()
	store.pruneLocked(now)
	code := normalizeUserCode(userCode)
	grant, ok := store.state.Grants[code]
	if !ok || grant.Status != "pending" || grant.ExpiresAt <= now {
		return errors.New("authorization request is invalid or expired")
	}
	if grant.CSRFHash == "" || !equalAuthHash(grant.CSRFHash, hashAuthSecret(csrf)) {
		return errors.New("invalid authorization confirmation")
	}
	grant.Status = "denied"
	grant.CSRFHash = ""
	store.state.Grants[code] = grant
	return store.saveLocked()
}

func (store *authStore) exchange(deviceCode, verifier string) (issuedTokens, string, error) {
	if !validPKCEVerifier(verifier) {
		return issuedTokens{}, "invalid_request", errors.New("invalid PKCE code_verifier")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().Unix()
	store.pruneLocked(now)
	deviceHash := hashAuthSecret(deviceCode)
	var code string
	var grant deviceGrant
	for candidateCode, candidate := range store.state.Grants {
		if equalAuthHash(candidate.DeviceHash, deviceHash) {
			code, grant = candidateCode, candidate
			break
		}
	}
	if code == "" || grant.ExpiresAt <= now {
		return issuedTokens{}, "expired_token", errors.New("device authorization expired")
	}
	if !equalAuthHash(grant.CodeChallenge, pkceChallenge(verifier)) {
		return issuedTokens{}, "invalid_grant", errors.New("PKCE verification failed")
	}
	switch grant.Status {
	case "pending":
		return issuedTokens{}, "authorization_pending", errors.New("authorization is pending")
	case "denied":
		delete(store.state.Grants, code)
		_ = store.saveLocked()
		return issuedTokens{}, "access_denied", errors.New("authorization was denied")
	case "approved":
	default:
		return issuedTokens{}, "invalid_grant", errors.New("authorization request was already consumed")
	}
	tokens, err := store.issueLocked(grant.User, []string{"eutherpunk:chat"}, "")
	if err != nil {
		return issuedTokens{}, "server_error", err
	}
	delete(store.state.Grants, code)
	if err := store.saveLocked(); err != nil {
		return issuedTokens{}, "server_error", err
	}
	return tokens, "", nil
}

func (store *authStore) refresh(rawToken string) (issuedTokens, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().Unix()
	store.pruneLocked(now)
	hash := hashAuthSecret(rawToken)
	record, ok := store.state.Refresh[hash]
	if !ok || record.ExpiresAt <= now {
		return issuedTokens{}, errors.New("refresh token is invalid or expired")
	}
	delete(store.state.Refresh, hash)
	tokens, err := store.issueLocked(record.User, record.Scopes, record.FamilyID)
	if err != nil {
		return issuedTokens{}, err
	}
	if err := store.saveLocked(); err != nil {
		return issuedTokens{}, err
	}
	return tokens, nil
}

func (store *authStore) authenticate(rawToken string) (authPrincipal, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	now := store.now().Unix()
	changed := store.pruneLocked(now)
	record, ok := store.state.Access[hashAuthSecret(rawToken)]
	if changed {
		_ = store.saveLocked()
	}
	if !ok || record.ExpiresAt <= now {
		return authPrincipal{}, false
	}
	return authPrincipal{
		User:      record.User,
		Scopes:    append([]string(nil), record.Scopes...),
		AuthMode:  "cli_token",
		ExpiresAt: record.ExpiresAt,
	}, true
}

func (store *authStore) revoke(rawAccessToken string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	hash := hashAuthSecret(rawAccessToken)
	record, ok := store.state.Access[hash]
	if !ok {
		return nil
	}
	for tokenHash, access := range store.state.Access {
		if access.FamilyID == record.FamilyID {
			delete(store.state.Access, tokenHash)
		}
	}
	for tokenHash, refresh := range store.state.Refresh {
		if refresh.FamilyID == record.FamilyID {
			delete(store.state.Refresh, tokenHash)
		}
	}
	return store.saveLocked()
}

func (store *authStore) issueLocked(user string, scopes []string, familyID string) (issuedTokens, error) {
	if familyID == "" {
		var err error
		familyID, err = randomAuthToken(18)
		if err != nil {
			return issuedTokens{}, err
		}
	}
	access, err := randomAuthToken(32)
	if err != nil {
		return issuedTokens{}, err
	}
	refresh, err := randomAuthToken(40)
	if err != nil {
		return issuedTokens{}, err
	}
	now := store.now().Unix()
	accessExpiry := now + int64(authAccessTTL/time.Second)
	refreshExpiry := now + int64(authRefreshTTL/time.Second)
	store.state.Access[hashAuthSecret(access)] = accessRecord{
		FamilyID:  familyID,
		User:      user,
		Scopes:    append([]string(nil), scopes...),
		IssuedAt:  now,
		ExpiresAt: accessExpiry,
	}
	store.state.Refresh[hashAuthSecret(refresh)] = refreshRecord{
		FamilyID:  familyID,
		User:      user,
		Scopes:    append([]string(nil), scopes...),
		IssuedAt:  now,
		ExpiresAt: refreshExpiry,
	}
	return issuedTokens{
		AccessToken:  access,
		RefreshToken: refresh,
		User:         user,
		Scopes:       append([]string(nil), scopes...),
		ExpiresAt:    accessExpiry,
	}, nil
}

func (store *authStore) uniqueUserCodeLocked() (string, error) {
	for range 20 {
		raw := make([]byte, 8)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
		for i := range raw {
			raw[i] = alphabet[int(raw[i])%len(alphabet)]
		}
		code := string(raw[:4]) + "-" + string(raw[4:])
		if _, exists := store.state.Grants[code]; !exists {
			return code, nil
		}
	}
	return "", errors.New("could not allocate a device authorization code")
}

func (store *authStore) pruneLocked(now int64) bool {
	changed := false
	for code, grant := range store.state.Grants {
		if grant.ExpiresAt <= now {
			delete(store.state.Grants, code)
			changed = true
		}
	}
	for hash, token := range store.state.Access {
		if token.ExpiresAt <= now {
			delete(store.state.Access, hash)
			changed = true
		}
	}
	for hash, token := range store.state.Refresh {
		if token.ExpiresAt <= now {
			delete(store.state.Refresh, hash)
			changed = true
		}
	}
	return changed
}

func (store *authStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(store.state, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(store.path), ".cli-auth-*.new")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, store.path)
}

func randomAuthToken(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashAuthSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func equalAuthHash(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func validPKCEChallenge(value string) bool {
	if len(value) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil
}

func validPKCEVerifier(value string) bool {
	if len(value) < 43 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("-._~", r) {
			continue
		}
		return false
	}
	return true
}

func normalizeUserCode(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func hasScope(principal authPrincipal, scope string) bool {
	for _, candidate := range principal.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}
