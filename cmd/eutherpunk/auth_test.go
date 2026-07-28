//go:build !windows

package main

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialFileRoundTripAndDelete(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	want := authCredentials{
		AccessToken:  "access-secret",
		RefreshToken: "refresh-secret",
		ExpiresAt:    123,
		User:         "nichlas",
		Scopes:       []string{"eutherpunk:chat"},
	}
	if err := saveCredentials(configPath, "https://example.invalid", want); err != nil {
		t.Fatal(err)
	}
	got, err := loadCredentials(configPath, "https://example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken || got.User != want.User {
		t.Fatalf("credentials = %#v", got)
	}
	if err := deleteCredentials(configPath, "https://example.invalid"); err != nil {
		t.Fatal(err)
	}
	got, err = loadCredentials(configPath, "https://example.invalid")
	if err != nil || got.AccessToken != "" {
		t.Fatalf("credentials after delete = %#v, %v", got, err)
	}
}

func TestEnsureAuthenticatedRotatesRefreshToken(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := saveCredentials(configPath, "https://example.invalid", authCredentials{
		RefreshToken: "old-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	originalClient := cliHTTPClient
	cliHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/eutherpunk/auth/refresh" {
			t.Fatalf("path = %q", req.URL.Path)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(`{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"expires_at":9999999999,
				"user":"nichlas",
				"scopes":["eutherpunk:chat"]
			}`)),
			Request: req,
		}, nil
	})}
	t.Cleanup(func() { cliHTTPClient = originalClient })

	cfg := cliConfig{apiURL: "https://example.invalid", configPath: configPath}
	if err := cfg.ensureAuthenticated(false); err != nil {
		t.Fatal(err)
	}
	if cfg.credentials.AccessToken != "new-access" || cfg.credentials.RefreshToken != "new-refresh" {
		t.Fatalf("credentials = %#v", cfg.credentials)
	}
	stored, err := loadCredentials(configPath, cfg.apiURL)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "new-refresh" || stored.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("stored credentials = %#v", stored)
	}
}
