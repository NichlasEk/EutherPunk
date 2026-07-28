//go:build !windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

func credentialPath(configPath, apiURL string) string {
	sum := sha256.Sum256([]byte(apiURL))
	return filepath.Join(filepath.Dir(configPath), "credentials-"+hex.EncodeToString(sum[:8])+".json")
}

func loadCredentials(configPath, apiURL string) (authCredentials, error) {
	var credentials authCredentials
	raw, err := os.ReadFile(credentialPath(configPath, apiURL))
	if errors.Is(err, os.ErrNotExist) {
		return credentials, nil
	}
	if err != nil {
		return credentials, err
	}
	err = json.Unmarshal(raw, &credentials)
	return credentials, err
}

func saveCredentials(configPath, apiURL string, credentials authCredentials) error {
	path := credentialPath(configPath, apiURL)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.new")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func deleteCredentials(configPath, apiURL string) error {
	err := os.Remove(credentialPath(configPath, apiURL))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
