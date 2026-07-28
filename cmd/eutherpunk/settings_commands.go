package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func handleSettingsCommand(
	cfg *cliConfig,
	permissions *sessionPermissions,
	editor *lineEditor,
	command string,
) error {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(command)))
	switch {
	case len(fields) == 1:
		printSettingsStatus(cfg.settings)
	case len(fields) == 2 && fields[1] == "path":
		fmt.Println(cfg.settings.Path)
	case len(fields) == 2 && fields[1] == "show":
		fmt.Print(cfg.settings.TOML())
	case len(fields) == 2 && fields[1] == "init":
		if cfg.settings.Exists {
			fmt.Println("settings.toml finns redan:", cfg.settings.Path)
			return nil
		}
		captureRuntimeSettings(cfg, permissions)
		if err := cfg.settings.Save(); err != nil {
			return err
		}
		_ = os.Remove(cfg.memory.EnabledPath)
		fmt.Println("Skapade:", cfg.settings.Path)
	case len(fields) == 2 && fields[1] == "save":
		captureRuntimeSettings(cfg, permissions)
		if err := cfg.settings.Save(); err != nil {
			return err
		}
		_ = os.Remove(cfg.memory.EnabledPath)
		fmt.Println("Sparade:", cfg.settings.Path)
	case len(fields) == 2 && fields[1] == "reload":
		if !cfg.settings.Exists {
			return errors.New("settings.toml finns inte; använd /settings init")
		}
		reloaded, err := loadCLISettings(cfg.settings)
		if err != nil {
			return err
		}
		if err := applyCLISettings(cfg, permissions, editor, reloaded); err != nil {
			return err
		}
		fmt.Println("Inställningarna är omladdade.")
	default:
		printSettingsHelp()
	}
	return nil
}

func captureRuntimeSettings(cfg *cliConfig, permissions *sessionPermissions) {
	cfg.settings.ConnectionURL = cfg.apiURL
	cfg.settings.Model = cfg.model
	if permissions.systemInfo == permissionOff {
		cfg.settings.SystemInfo = permissionOff
	} else {
		cfg.settings.SystemInfo = permissionAsk
	}
	cfg.settings.MemoryEnabled = cfg.memory.Enabled
	cfg.settings.MemoryFile = filepath.Base(cfg.memory.Path)
	cfg.settings.MemoryMaxBytes = cfg.memory.MaxBytes
}

func applyCLISettings(
	cfg *cliConfig,
	permissions *sessionPermissions,
	editor *lineEditor,
	settings cliSettings,
) error {
	memory, err := loadMemoryStateFromSettings(settings)
	if err != nil {
		return err
	}
	if strings.TrimSpace(os.Getenv("EUTHERPUNK_URL")) == "" {
		cfg.apiURL = strings.TrimRight(settings.ConnectionURL, "/")
	}
	if strings.TrimSpace(os.Getenv("EUTHERPUNK_MODEL")) == "" {
		cfg.model = settings.Model
	}
	cfg.memory = memory
	cfg.settings = settings
	permissions.systemInfo = settings.SystemInfo
	editor.ApplySettings(settings.Terminal)
	return nil
}

func printSettingsStatus(settings cliSettings) {
	state := "INTE SKAPAD"
	if settings.Exists {
		state = "AKTIV"
	}
	fmt.Println("INSTÄLLNINGAR", state)
	fmt.Println("Fil:", settings.Path)
	fmt.Println("Profil:", settings.Profile)
	fmt.Println("Modell:", settings.Model)
	fmt.Println("Läge:", settings.Mode)
	fmt.Println("Systeminformation:", strings.ToUpper(string(settings.SystemInfo)))
	fmt.Println("Minne:", onOff(settings.MemoryEnabled))
	fmt.Println("Autocomplete:", onOff(settings.Terminal.Autocomplete))
	fmt.Println("Datornamn delas:", onOff(settings.Privacy.ShareHostname))
	fmt.Println("Användarnamn delas:", onOff(settings.Privacy.ShareUsername))
	fmt.Println("Arbetskatalog delas:", onOff(settings.Privacy.ShareWorkingDirectory))
	fmt.Println("Inga lösenord eller tokens lagras i settings.toml.")
}

func printSettingsHelp() {
	fmt.Println("Användning:")
	fmt.Println("  /settings")
	fmt.Println("  /settings init|show|path|reload|save")
}

func onOff(value bool) string {
	if value {
		return "PÅ"
	}
	return "AV"
}
