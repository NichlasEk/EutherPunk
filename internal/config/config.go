package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Profile     string                `json:"profile"`
	Server      ServerConfig          `json:"server"`
	Agent       AgentConfig           `json:"agent"`
	Downloads   DownloadsConfig       `json:"downloads"`
	EutherOxide EutherOxideConfig     `json:"eutheroxide"`
	Tools       ToolsConfig           `json:"tools"`
	Users       map[string]UserConfig `json:"users"`
	Path        string                `json:"path"`
}

type ServerConfig struct {
	LANURL    string `json:"lan_url"`
	PublicURL string `json:"public_url"`
	PreferLAN bool   `json:"prefer_lan"`
}

type AgentConfig struct {
	APIURL    string `json:"api_url"`
	Listen    string `json:"listen"`
	OllamaURL string `json:"ollama_url"`
	Model     string `json:"model"`
	SafeMode  bool   `json:"safe_mode"`
}

type DownloadsConfig struct {
	Directory string `json:"directory"`
}

type EutherOxideConfig struct {
	BaseURL      string `json:"base_url"`
	UsersURL     string `json:"users_url"`
	AuthRequired bool   `json:"auth_required"`
}

type ToolsConfig struct {
	AllowRead               bool `json:"allow_read"`
	AllowWrite              bool `json:"allow_write"`
	AllowShell              bool `json:"allow_shell"`
	RequireApprovalForApply bool `json:"require_approval_for_apply"`
}

type UserConfig struct {
	EutherOxideID       string `json:"eutheroxide_id"`
	EutherOxideUsername string `json:"eutheroxide_username"`
	Model               string `json:"model"`
	APIURL              string `json:"api_url"`
	SafeMode            *bool  `json:"safe_mode"`
}

func Default() Config {
	return Config{
		Profile: "default",
		Server: ServerConfig{
			LANURL:    "http://192.168.32.186:8080",
			PublicURL: "https://apothictech.se",
			PreferLAN: true,
		},
		Agent: AgentConfig{
			APIURL:    "http://127.0.0.1:8787",
			Listen:    ":8787",
			OllamaURL: "http://127.0.0.1:11434",
			Model:     "qwen3-coder:30b",
			SafeMode:  true,
		},
		Downloads: DownloadsConfig{
			Directory: "dist/cli",
		},
		EutherOxide: EutherOxideConfig{
			BaseURL:      "http://192.168.32.186:8080",
			UsersURL:     "http://192.168.32.186:8080/api/app/users",
			AuthRequired: true,
		},
		Tools: ToolsConfig{
			AllowRead:               true,
			AllowWrite:              false,
			AllowShell:              false,
			RequireApprovalForApply: true,
		},
		Users: map[string]UserConfig{},
	}
}

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultPath()
	}
	cfg.Path = path

	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	defer file.Close()

	section := ""
	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := stripComment(scanner.Text())
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			if strings.HasPrefix(section, "users.") {
				name := strings.TrimPrefix(section, "users.")
				if name == "" {
					return cfg, fmt.Errorf("%s:%d: empty user section", path, lineNo)
				}
				if cfg.Users == nil {
					cfg.Users = map[string]UserConfig{}
				}
				if _, ok := cfg.Users[name]; !ok {
					cfg.Users[name] = UserConfig{}
				}
			}
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return cfg, fmt.Errorf("%s:%d: expected key = value", path, lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if err := cfg.set(section, key, value); err != nil {
			return cfg, fmt.Errorf("%s:%d: %w", path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func DefaultPath() string {
	if value := strings.TrimSpace(os.Getenv("EUTHERPUNK_CONFIG")); value != "" {
		return value
	}
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "eutherpunk", "config.toml")
	}
	return "config/eutherpunk.toml"
}

func (cfg Config) User(name string) UserConfig {
	user := cfg.Users[name]
	if user.Model == "" {
		user.Model = cfg.Agent.Model
	}
	if user.APIURL == "" {
		user.APIURL = cfg.Agent.APIURL
	}
	if user.SafeMode == nil {
		safeMode := cfg.Agent.SafeMode
		user.SafeMode = &safeMode
	}
	return user
}

func (cfg *Config) set(section, key, raw string) error {
	switch section {
	case "":
		switch key {
		case "profile":
			cfg.Profile = mustString(raw)
		default:
			return unknown(section, key)
		}
	case "server":
		switch key {
		case "lan_url":
			cfg.Server.LANURL = mustString(raw)
		case "public_url":
			cfg.Server.PublicURL = mustString(raw)
		case "prefer_lan":
			cfg.Server.PreferLAN = mustBool(raw)
		default:
			return unknown(section, key)
		}
	case "agent":
		switch key {
		case "api_url":
			cfg.Agent.APIURL = mustString(raw)
		case "listen":
			cfg.Agent.Listen = mustString(raw)
		case "ollama_url":
			cfg.Agent.OllamaURL = mustString(raw)
		case "model":
			cfg.Agent.Model = mustString(raw)
		case "safe_mode":
			cfg.Agent.SafeMode = mustBool(raw)
		default:
			return unknown(section, key)
		}
	case "eutheroxide":
		switch key {
		case "base_url":
			cfg.EutherOxide.BaseURL = mustString(raw)
		case "users_url":
			cfg.EutherOxide.UsersURL = mustString(raw)
		case "auth_required":
			cfg.EutherOxide.AuthRequired = mustBool(raw)
		default:
			return unknown(section, key)
		}
	case "downloads":
		switch key {
		case "directory":
			cfg.Downloads.Directory = mustString(raw)
		default:
			return unknown(section, key)
		}
	case "tools":
		switch key {
		case "allow_read":
			cfg.Tools.AllowRead = mustBool(raw)
		case "allow_write":
			cfg.Tools.AllowWrite = mustBool(raw)
		case "allow_shell":
			cfg.Tools.AllowShell = mustBool(raw)
		case "require_approval_for_apply":
			cfg.Tools.RequireApprovalForApply = mustBool(raw)
		default:
			return unknown(section, key)
		}
	default:
		if strings.HasPrefix(section, "users.") {
			name := strings.TrimPrefix(section, "users.")
			user := cfg.Users[name]
			switch key {
			case "eutheroxide_id":
				user.EutherOxideID = mustString(raw)
			case "eutheroxide_username":
				user.EutherOxideUsername = mustString(raw)
			case "model":
				user.Model = mustString(raw)
			case "api_url":
				user.APIURL = mustString(raw)
			case "safe_mode":
				safeMode := mustBool(raw)
				user.SafeMode = &safeMode
			default:
				return unknown(section, key)
			}
			cfg.Users[name] = user
			return nil
		}
		return fmt.Errorf("unknown section [%s]", section)
	}
	return nil
}

func stripComment(line string) string {
	inString := false
	escaped := false
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inString = !inString
		case r == '#' && !inString:
			return line[:i]
		}
	}
	return line
}

func mustString(raw string) string {
	value, err := strconv.Unquote(raw)
	if err == nil {
		return value
	}
	return strings.Trim(raw, `"`)
}

func mustBool(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), "true")
}

func unknown(section, key string) error {
	if section == "" {
		return fmt.Errorf("unknown key %q", key)
	}
	return fmt.Errorf("unknown key %q in [%s]", key, section)
}
