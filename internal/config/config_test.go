package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUserEutherOxideMapping(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	input := `
profile = "dev"

[agent]
api_url = "http://127.0.0.1:9999"
listen = ":9999"
ollama_url = "http://127.0.0.1:11434"
model = "qwen3-coder:30b"
safe_mode = true

[eutheroxide]
base_url = "http://192.168.32.186:8080"
users_url = "http://192.168.32.186:8080/api/app/users"
auth_required = true

[users.nichlas]
eutheroxide_id = "42"
eutheroxide_username = "nichlas"
model = "qwen3-coder:30b"
safe_mode = false
`
	if err := os.WriteFile(path, []byte(input), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	user := cfg.User("nichlas")
	if user.EutherOxideID != "42" {
		t.Fatalf("EutherOxideID = %q", user.EutherOxideID)
	}
	if user.EutherOxideUsername != "nichlas" {
		t.Fatalf("EutherOxideUsername = %q", user.EutherOxideUsername)
	}
	if user.SafeMode == nil {
		t.Fatal("SafeMode was nil")
	}
	if *user.SafeMode {
		t.Fatal("SafeMode override should be false")
	}
}

func TestLoadMissingConfigUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.Model != "qwen3-coder:30b" {
		t.Fatalf("model = %q", cfg.Agent.Model)
	}
	if cfg.EutherOxide.UsersURL == "" {
		t.Fatal("expected default EutherOxide users URL")
	}
}
