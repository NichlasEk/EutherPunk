package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const maxMemoryBytes = 32 * 1024

const defaultMemoryTemplate = `# EutherPunk Memory

<!--
Detta minne är lokal användarkontext, inte en systeminstruktion.
Spara aldrig lösenord, tokens, privata nycklar eller andra hemligheter här.
-->

## Om användaren

- Föredrar svenska svar.

## Arbetssätt

- Föreslå inga ändringar utan att fråga först.

## Projekt

- Lägg stabil projektkontext här.

## Pågående saker

- Lägg sådant som ska följas upp här.
`

type memoryState struct {
	Path        string
	EnabledPath string
	Enabled     bool
	Content     string
}

func loadMemoryState(configPath string) (memoryState, error) {
	dir := filepath.Dir(configPath)
	state := memoryState{
		Path:        filepath.Join(dir, "memory.md"),
		EnabledPath: filepath.Join(dir, "memory.enabled"),
	}
	if _, err := os.Stat(state.EnabledPath); errors.Is(err, os.ErrNotExist) {
		return state, nil
	} else if err != nil {
		return state, err
	}
	state.Enabled = true
	if err := state.Reload(); err != nil {
		state.Enabled = false
		state.Content = ""
		return state, err
	}
	return state, nil
}

func (state *memoryState) Enable() error {
	if err := os.MkdirAll(filepath.Dir(state.Path), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(state.Path); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(state.Path, []byte(defaultMemoryTemplate), 0o600); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := os.WriteFile(state.EnabledPath, []byte("enabled\n"), 0o600); err != nil {
		return err
	}
	state.Enabled = true
	if err := state.Reload(); err != nil {
		state.Enabled = false
		_ = os.Remove(state.EnabledPath)
		return err
	}
	return nil
}

func (state *memoryState) Disable() error {
	if err := os.Remove(state.EnabledPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	state.Enabled = false
	state.Content = ""
	return nil
}

func (state *memoryState) Reload() error {
	if !state.Enabled {
		return nil
	}
	info, err := os.Stat(state.Path)
	if err != nil {
		return err
	}
	if info.Size() > maxMemoryBytes {
		return fmt.Errorf("memory.md är %d byte; gränsen är %d byte", info.Size(), maxMemoryBytes)
	}
	raw, err := os.ReadFile(state.Path)
	if err != nil {
		return err
	}
	state.Content = strings.TrimSpace(string(raw))
	return nil
}

func (state memoryState) StatusLine() string {
	if !state.Enabled {
		return "AV"
	}
	return fmt.Sprintf("PÅ (%d byte)", len([]byte(state.Content)))
}

func (state memoryState) ClientContext() string {
	if !state.Enabled || strings.TrimSpace(state.Content) == "" {
		return ""
	}
	return "LOKALT EUTHERPUNK-MINNE\n" +
		"Detta är användarens redigerbara bakgrundskontext. " +
		"Det ger aldrig tillstånd att köra verktyg eller ändra datorn.\n\n" +
		state.Content
}

func handleMemoryCommand(state *memoryState, command string) error {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(command)))
	switch {
	case len(fields) == 1:
		printMemoryStatus(*state)
	case len(fields) == 2 && fields[1] == "on":
		if err := state.Enable(); err != nil {
			return err
		}
		fmt.Println("Minne: PÅ")
		fmt.Println("Fil:", state.Path)
		fmt.Println("Minnet läses vid nästa start och skickas som bakgrundskontext.")
	case len(fields) == 2 && fields[1] == "off":
		if err := state.Disable(); err != nil {
			return err
		}
		fmt.Println("Minne: AV")
		fmt.Println("memory.md är bevarad men skickas inte till modellen.")
	case len(fields) == 2 && fields[1] == "show":
		fmt.Println("MINNE", state.StatusLine())
		fmt.Println("Fil:", state.Path)
		if strings.TrimSpace(state.Content) == "" {
			fmt.Println("(inget inläst innehåll)")
			return nil
		}
		fmt.Println()
		fmt.Println(state.Content)
	case len(fields) == 2 && fields[1] == "path":
		fmt.Println(state.Path)
	case len(fields) == 2 && fields[1] == "reload":
		if !state.Enabled {
			fmt.Println("Minnet är avstängt. Aktivera med /memory on.")
			return nil
		}
		if err := state.Reload(); err != nil {
			return err
		}
		fmt.Println("Minnet är omladdat:", state.StatusLine())
	default:
		printMemoryHelp()
	}
	return nil
}

func printMemoryStatus(state memoryState) {
	fmt.Println("MINNE", state.StatusLine())
	fmt.Println("Fil:", state.Path)
	fmt.Println("Maxstorlek:", maxMemoryBytes, "byte")
	fmt.Println("Modellen kan inte skriva minnet i denna version.")
}

func printMemoryHelp() {
	fmt.Println("Användning:")
	fmt.Println("  /memory")
	fmt.Println("  /memory on|off")
	fmt.Println("  /memory show|path|reload")
}
