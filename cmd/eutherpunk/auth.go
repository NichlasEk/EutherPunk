package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type authCredentials struct {
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token"`
	ExpiresAt    int64    `json:"expires_at"`
	User         string   `json:"user"`
	Scopes       []string `json:"scopes"`
}

type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

type authError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func (cfg *cliConfig) ensureAuthenticated(interactive bool) error {
	if cfg.credentials.AccessToken != "" && cfg.credentials.ExpiresAt > time.Now().Add(30*time.Second).Unix() {
		return nil
	}
	credentials, err := loadCredentials(cfg.configPath, cfg.apiURL)
	if err != nil {
		return fmt.Errorf("läs inloggning: %w", err)
	}
	if credentials.AccessToken != "" && credentials.ExpiresAt > time.Now().Add(30*time.Second).Unix() {
		cfg.credentials = credentials
		return nil
	}
	if credentials.RefreshToken != "" {
		refreshed, refreshErr := cfg.refreshCredentials(credentials.RefreshToken)
		if refreshErr == nil {
			cfg.credentials = refreshed
			return saveCredentials(cfg.configPath, cfg.apiURL, refreshed)
		}
		_ = deleteCredentials(cfg.configPath, cfg.apiURL)
	}
	if !interactive {
		return errors.New("EutherID-inloggning krävs; kör eutherpunk auth login")
	}
	return cfg.login()
}

func (cfg *cliConfig) login() error {
	verifierBytes := make([]byte, 48)
	if _, err := rand.Read(verifierBytes); err != nil {
		return err
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	var authorization deviceAuthorization
	if err := cfg.authJSON(http.MethodPost, "/api/eutherpunk/auth/device", map[string]string{
		"client_name":    "EutherPunk CLI " + version + " (" + runtime.GOOS + ")",
		"code_challenge": challenge,
	}, "", &authorization); err != nil {
		return err
	}
	fmt.Println("Öppna och godkänn EutherPunk med EutherID:")
	fmt.Println(authorization.VerificationURIComplete)
	fmt.Println("Kod:", authorization.UserCode)
	if err := openBrowser(authorization.VerificationURIComplete); err != nil {
		fmt.Println("Webbläsaren kunde inte öppnas automatiskt; öppna länken ovan.")
	}

	interval := time.Duration(authorization.Interval) * time.Second
	if interval < time.Second {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(time.Duration(authorization.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		var credentials authCredentials
		err := cfg.authJSON(http.MethodPost, "/api/eutherpunk/auth/token", map[string]string{
			"device_code":   authorization.DeviceCode,
			"code_verifier": verifier,
		}, "", &credentials)
		var remoteErr *authErrorResponse
		if errors.As(err, &remoteErr) && remoteErr.Code == "authorization_pending" {
			continue
		}
		if err != nil {
			return err
		}
		cfg.credentials = credentials
		if err := saveCredentials(cfg.configPath, cfg.apiURL, credentials); err != nil {
			return fmt.Errorf("spara inloggning: %w", err)
		}
		fmt.Println("Inloggad som", credentials.User, "via EutherID.")
		return nil
	}
	return errors.New("inloggningen gick ut; kör /auth login igen")
}

func (cfg *cliConfig) refreshCredentials(refreshToken string) (authCredentials, error) {
	var credentials authCredentials
	err := cfg.authJSON(http.MethodPost, "/api/eutherpunk/auth/refresh", map[string]string{
		"refresh_token": refreshToken,
	}, "", &credentials)
	return credentials, err
}

func (cfg *cliConfig) logout() error {
	if credentials, err := loadCredentials(cfg.configPath, cfg.apiURL); err == nil && credentials.AccessToken != "" {
		_ = cfg.authJSON(http.MethodPost, "/api/eutherpunk/auth/revoke", nil, credentials.AccessToken, nil)
	}
	cfg.credentials = authCredentials{}
	if err := deleteCredentials(cfg.configPath, cfg.apiURL); err != nil {
		return err
	}
	fmt.Println("Utloggad. Den lokala inloggningen och dess server-token är återkallade.")
	return nil
}

func (cfg *cliConfig) authStatus() error {
	if err := cfg.ensureAuthenticated(false); err != nil {
		fmt.Println("EutherID: inte inloggad")
		return nil
	}
	var principal struct {
		User     string   `json:"user"`
		Scopes   []string `json:"scopes"`
		AuthMode string   `json:"auth_mode"`
	}
	if err := cfg.authJSON(http.MethodGet, "/api/eutherpunk/auth/me", nil, cfg.credentials.AccessToken, &principal); err != nil {
		return err
	}
	fmt.Printf("EutherID: inloggad som %s\n", principal.User)
	fmt.Printf("Åtkomst: %s\n", strings.Join(principal.Scopes, ", "))
	return nil
}

type authErrorResponse struct {
	Status int
	Code   string
	Text   string
	Path   string
}

func (err *authErrorResponse) Error() string {
	if err.Text != "" {
		return fmt.Sprintf("auth %d %s: %s", err.Status, err.Code, err.Text)
	}
	if err.Code == "" {
		return fmt.Sprintf(
			"auth %d %s från %s; kontrollera connection.url med 'eutherpunk doctor'",
			err.Status,
			http.StatusText(err.Status),
			err.Path,
		)
	}
	return fmt.Sprintf("auth %d: %s", err.Status, err.Code)
}

func (cfg *cliConfig) authJSON(method, path string, input any, bearer string, output any) error {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, cfg.apiURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "eutherpunk-cli/"+version)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var remote authError
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&remote)
		return &authErrorResponse{
			Status: resp.StatusCode,
			Code:   remote.Error,
			Text:   remote.Message,
			Path:   cfg.apiURL + path,
		}
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 128*1024)).Decode(output)
}

func (cfg *cliConfig) authorize(req *http.Request) error {
	if err := cfg.ensureAuthenticated(true); err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.credentials.AccessToken)
	return nil
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		command = exec.Command("open", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Start()
}
