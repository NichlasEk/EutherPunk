package main

import (
	"bufio"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const settingsVersion = 1

type privacySettings struct {
	ShareHostname         bool
	ShareUsername         bool
	ShareWorkingDirectory bool
}

type terminalSettings struct {
	Autocomplete  bool
	AcceptUpArrow bool
	AcceptTab     bool
	History       bool
	GhostColor    string
	ImagePreview  string
	ImageMaxWidth int
}

type cliSettings struct {
	Path           string
	Exists         bool
	Version        int
	Profile        string
	ConnectionURL  string
	Model          string
	Language       string
	Mode           string
	SystemInfo     permissionLevel
	Files          permissionLevel
	MemoryEnabled  bool
	MemoryFile     string
	MemoryMaxBytes int
	Privacy        privacySettings
	Terminal       terminalSettings
}

func defaultCLISettings(configPath, apiURL, model string, memoryEnabled bool) cliSettings {
	return cliSettings{
		Path:           filepath.Join(filepath.Dir(configPath), "settings.toml"),
		Version:        settingsVersion,
		Profile:        "portable",
		ConnectionURL:  apiURL,
		Model:          model,
		Language:       "sv",
		Mode:           "chat",
		SystemInfo:     permissionAsk,
		Files:          permissionAuto,
		MemoryEnabled:  memoryEnabled,
		MemoryFile:     "memory.md",
		MemoryMaxBytes: maxMemoryBytes,
		Privacy: privacySettings{
			ShareHostname:         false,
			ShareUsername:         false,
			ShareWorkingDirectory: false,
		},
		Terminal: terminalSettings{
			Autocomplete:  true,
			AcceptUpArrow: true,
			AcceptTab:     true,
			History:       true,
			GhostColor:    "#5cff5c",
			ImagePreview:  "auto",
			ImageMaxWidth: 80,
		},
	}
}

func loadCLISettings(defaults cliSettings) (cliSettings, error) {
	settings := defaults
	settings.Exists = false
	file, err := os.Open(settings.Path)
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return settings, err
	}
	defer file.Close()

	settings.Exists = true
	section := ""
	scanner := bufio.NewScanner(file)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok {
			return settings, fmt.Errorf("%s:%d: förväntade key = value", settings.Path, lineNo)
		}
		if err := settings.set(section, strings.TrimSpace(key), strings.TrimSpace(raw)); err != nil {
			return settings, fmt.Errorf("%s:%d: %w", settings.Path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return settings, err
	}
	if err := settings.Validate(); err != nil {
		return settings, err
	}
	return settings, nil
}

func (settings *cliSettings) set(section, key, raw string) error {
	switch section {
	case "":
		switch key {
		case "version":
			return setSettingsInt(raw, &settings.Version)
		case "profile":
			return setSettingsString(raw, &settings.Profile)
		default:
			return fmt.Errorf("okänd inställning %q", key)
		}
	case "connection":
		if key != "url" {
			return fmt.Errorf("okänd inställning [connection].%s", key)
		}
		return setSettingsString(raw, &settings.ConnectionURL)
	case "agent":
		switch key {
		case "model":
			return setSettingsString(raw, &settings.Model)
		case "language":
			return setSettingsString(raw, &settings.Language)
		case "mode":
			return setSettingsString(raw, &settings.Mode)
		default:
			return fmt.Errorf("okänd inställning [agent].%s", key)
		}
	case "permissions":
		var destination *permissionLevel
		switch key {
		case "system_info":
			destination = &settings.SystemInfo
		case "files":
			destination = &settings.Files
		default:
			return fmt.Errorf("okänd inställning [permissions].%s", key)
		}
		var value string
		if err := setSettingsString(raw, &value); err != nil {
			return err
		}
		*destination = permissionLevel(value)
	case "memory":
		switch key {
		case "enabled":
			return setSettingsBool(raw, &settings.MemoryEnabled)
		case "file":
			return setSettingsString(raw, &settings.MemoryFile)
		case "max_bytes":
			return setSettingsInt(raw, &settings.MemoryMaxBytes)
		default:
			return fmt.Errorf("okänd inställning [memory].%s", key)
		}
	case "privacy":
		switch key {
		case "share_hostname":
			return setSettingsBool(raw, &settings.Privacy.ShareHostname)
		case "share_username":
			return setSettingsBool(raw, &settings.Privacy.ShareUsername)
		case "share_working_directory":
			return setSettingsBool(raw, &settings.Privacy.ShareWorkingDirectory)
		default:
			return fmt.Errorf("okänd inställning [privacy].%s", key)
		}
	case "terminal":
		switch key {
		case "autocomplete":
			return setSettingsBool(raw, &settings.Terminal.Autocomplete)
		case "accept_up_arrow":
			return setSettingsBool(raw, &settings.Terminal.AcceptUpArrow)
		case "accept_tab":
			return setSettingsBool(raw, &settings.Terminal.AcceptTab)
		case "history":
			return setSettingsBool(raw, &settings.Terminal.History)
		case "ghost_color":
			return setSettingsString(raw, &settings.Terminal.GhostColor)
		case "image_preview":
			return setSettingsString(raw, &settings.Terminal.ImagePreview)
		case "image_max_width":
			return setSettingsInt(raw, &settings.Terminal.ImageMaxWidth)
		default:
			return fmt.Errorf("okänd inställning [terminal].%s", key)
		}
	default:
		return fmt.Errorf("okänd sektion [%s]", section)
	}
	return nil
}

func (settings cliSettings) Validate() error {
	if settings.Version != settingsVersion {
		return fmt.Errorf("settings-version %d stöds inte", settings.Version)
	}
	parsedURL, err := url.Parse(settings.ConnectionURL)
	if err != nil || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") || parsedURL.Host == "" {
		return errors.New("[connection].url måste vara en giltig http- eller https-adress")
	}
	if strings.TrimSpace(settings.Model) == "" {
		return errors.New("[agent].model får inte vara tom")
	}
	if settings.Mode != "chat" {
		return errors.New("[agent].mode stöder endast \"chat\" i denna version")
	}
	if settings.SystemInfo != permissionOff && settings.SystemInfo != permissionAsk {
		return errors.New("[permissions].system_info måste vara \"off\" eller \"ask\"")
	}
	if settings.Files != permissionOff && settings.Files != permissionAsk && settings.Files != permissionAuto {
		return errors.New("[permissions].files måste vara \"off\", \"ask\" eller \"auto\"")
	}
	if settings.MemoryMaxBytes < 1 || settings.MemoryMaxBytes > maxMemoryBytes {
		return fmt.Errorf("[memory].max_bytes måste vara 1-%d", maxMemoryBytes)
	}
	if err := validateRelativeSettingsFile(settings.MemoryFile); err != nil {
		return err
	}
	if !validHexColor(settings.Terminal.GhostColor) {
		return errors.New("[terminal].ghost_color måste ha formatet #rrggbb")
	}
	switch settings.Terminal.ImagePreview {
	case "auto", "kitty", "blocks", "path", "off":
	default:
		return errors.New("[terminal].image_preview måste vara \"auto\", \"kitty\", \"blocks\", \"path\" eller \"off\"")
	}
	if settings.Terminal.ImageMaxWidth < 20 || settings.Terminal.ImageMaxWidth > 160 {
		return errors.New("[terminal].image_max_width måste vara 20-160")
	}
	return nil
}

func validateRelativeSettingsFile(name string) error {
	if name == "" || filepath.IsAbs(name) || filepath.Base(name) != name || name == "." || name == ".." {
		return errors.New("[memory].file måste vara ett enkelt filnamn bredvid settings.toml")
	}
	return nil
}

func validHexColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	_, err := strconv.ParseUint(value[1:], 16, 24)
	return err == nil
}

func (settings *cliSettings) Save() error {
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settings.Path), 0o700); err != nil {
		return err
	}
	raw := []byte(settings.TOML())
	if current, err := os.ReadFile(settings.Path); err == nil {
		if err := os.WriteFile(settings.Path+".previous", current, 0o600); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(settings.Path), "settings.toml.*.new")
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
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, settings.Path); err != nil {
		return err
	}
	settings.Exists = true
	return nil
}

func (settings cliSettings) TOML() string {
	return fmt.Sprintf(`version = %d
profile = %s

[connection]
url = %s

[agent]
model = %s
language = %s
mode = %s

[permissions]
system_info = %s
files = %s

[memory]
enabled = %t
file = %s
max_bytes = %d

[privacy]
share_hostname = %t
share_username = %t
share_working_directory = %t

[terminal]
autocomplete = %t
accept_up_arrow = %t
accept_tab = %t
history = %t
ghost_color = %s
image_preview = %s
image_max_width = %d
`,
		settings.Version,
		strconv.Quote(settings.Profile),
		strconv.Quote(settings.ConnectionURL),
		strconv.Quote(settings.Model),
		strconv.Quote(settings.Language),
		strconv.Quote(settings.Mode),
		strconv.Quote(string(settings.SystemInfo)),
		strconv.Quote(string(settings.Files)),
		settings.MemoryEnabled,
		strconv.Quote(settings.MemoryFile),
		settings.MemoryMaxBytes,
		settings.Privacy.ShareHostname,
		settings.Privacy.ShareUsername,
		settings.Privacy.ShareWorkingDirectory,
		settings.Terminal.Autocomplete,
		settings.Terminal.AcceptUpArrow,
		settings.Terminal.AcceptTab,
		settings.Terminal.History,
		strconv.Quote(strings.ToLower(settings.Terminal.GhostColor)),
		strconv.Quote(strings.ToLower(settings.Terminal.ImagePreview)),
		settings.Terminal.ImageMaxWidth,
	)
}

func stripTOMLComment(line string) string {
	inString := false
	escaped := false
	for i, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inString:
			escaped = true
		case r == '"':
			inString = !inString
		case r == '#' && !inString:
			return line[:i]
		}
	}
	return line
}

func setSettingsString(raw string, destination *string) error {
	value, err := strconv.Unquote(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("ogiltig sträng: %w", err)
	}
	*destination = value
	return nil
}

func setSettingsBool(raw string, destination *bool) error {
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("ogiltigt booleskt värde: %w", err)
	}
	*destination = value
	return nil
}

func setSettingsInt(raw string, destination *int) error {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("ogiltigt heltal: %w", err)
	}
	*destination = value
	return nil
}
