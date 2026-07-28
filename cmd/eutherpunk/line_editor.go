package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	ghostGreen = "\x1b[2;38;2;92;255;92m"
	ansiReset  = "\x1b[0m"
)

var availableCommands = []string{
	"/permissions",
	"/permissions reset",
	"/permissions system ask",
	"/permissions system off",
	"/permissions system session",
	"/system",
	"/system share",
	"/status",
	"/clear",
	"/help",
	"/exit",
}

type lineEditor struct {
	reader  *bufio.Reader
	history []string
}

func newLineEditor(reader *bufio.Reader) *lineEditor {
	return &lineEditor{reader: reader}
}

func (editor *lineEditor) ReadLine(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		fmt.Print(prompt)
		line, err := editor.reader.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), err
	}

	oldState, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Print(prompt)
		line, readErr := editor.reader.ReadString('\n')
		return strings.TrimRight(line, "\r\n"), readErr
	}
	defer term.Restore(fd, oldState)

	line := ""
	historyIndex := len(editor.history)
	if err := redrawInput(prompt, line); err != nil {
		return "", err
	}

	for {
		key, err := readTerminalKey(os.Stdin)
		if err != nil {
			return "", err
		}
		switch key {
		case "\r", "\n":
			if _, err := fmt.Print("\r\n"); err != nil {
				return "", err
			}
			if strings.TrimSpace(line) != "" {
				editor.history = append(editor.history, line)
			}
			return line, nil
		case "\x03":
			_, _ = fmt.Print("^C\r\n")
			return "", io.EOF
		case "\x04":
			if line == "" {
				_, _ = fmt.Print("\r\n")
				return "", io.EOF
			}
		case "\b", "\x7f":
			line = removeLastRune(line)
			historyIndex = len(editor.history)
		case "\t", "up":
			if suggestion := commandSuggestion(line); suggestion != "" {
				line = suggestion
				historyIndex = len(editor.history)
			} else if key == "up" && len(editor.history) > 0 {
				if historyIndex > 0 {
					historyIndex--
				}
				line = editor.history[historyIndex]
			}
		case "down":
			if historyIndex < len(editor.history)-1 {
				historyIndex++
				line = editor.history[historyIndex]
			} else {
				historyIndex = len(editor.history)
				line = ""
			}
		case "escape", "left", "right":
			// The preview editor intentionally edits at the end of the line.
		default:
			line += key
			historyIndex = len(editor.history)
		}
		if err := redrawInput(prompt, line); err != nil {
			return "", err
		}
	}
}

func readTerminalKey(input io.Reader) (string, error) {
	var first [1]byte
	if _, err := io.ReadFull(input, first[:]); err != nil {
		return "", err
	}
	if first[0] == 0x1b {
		var sequence [2]byte
		if _, err := io.ReadFull(input, sequence[:]); err != nil {
			return "escape", nil
		}
		switch string(sequence[:]) {
		case "[A", "OA":
			return "up", nil
		case "[B", "OB":
			return "down", nil
		case "[C", "OC":
			return "right", nil
		case "[D", "OD":
			return "left", nil
		default:
			return "escape", nil
		}
	}
	if first[0] == 0x00 || first[0] == 0xe0 {
		var legacy [1]byte
		if _, err := io.ReadFull(input, legacy[:]); err != nil {
			return "", err
		}
		switch legacy[0] {
		case 0x48:
			return "up", nil
		case 0x50:
			return "down", nil
		case 0x4d:
			return "right", nil
		case 0x4b:
			return "left", nil
		default:
			return "", nil
		}
	}
	if first[0] < utf8.RuneSelf {
		return string(first[:]), nil
	}

	size := utf8EncodedRuneSize(first[0])
	if size == 0 {
		return "", errors.New("ogiltig UTF-8-inmatning")
	}
	encoded := make([]byte, size)
	encoded[0] = first[0]
	if _, err := io.ReadFull(input, encoded[1:]); err != nil {
		return "", err
	}
	if !utf8.Valid(encoded) {
		return "", errors.New("ogiltig UTF-8-inmatning")
	}
	return string(encoded), nil
}

func utf8EncodedRuneSize(first byte) int {
	switch {
	case first&0xe0 == 0xc0:
		return 2
	case first&0xf0 == 0xe0:
		return 3
	case first&0xf8 == 0xf0:
		return 4
	default:
		return 0
	}
}

func redrawInput(prompt, line string) error {
	suggestion := commandSuggestion(line)
	suffix := ""
	if suggestion != "" && len(line) <= len(suggestion) {
		suffix = suggestion[len(line):]
	}
	if _, err := fmt.Printf("\r\x1b[2K%s%s", prompt, line); err != nil {
		return err
	}
	if suffix == "" {
		return nil
	}
	if _, err := fmt.Printf("%s%s%s", ghostGreen, suffix, ansiReset); err != nil {
		return err
	}
	_, err := fmt.Printf("\x1b[%dD", utf8.RuneCountInString(suffix))
	return err
}

func commandSuggestion(input string) string {
	if input == "" || !strings.HasPrefix(input, "/") {
		return ""
	}
	lower := strings.ToLower(input)
	for _, command := range availableCommands {
		if strings.EqualFold(command, input) {
			return ""
		}
		if strings.HasPrefix(strings.ToLower(command), lower) {
			return command
		}
	}
	return ""
}

func removeLastRune(value string) string {
	if value == "" {
		return ""
	}
	_, size := utf8.DecodeLastRuneInString(value)
	if size == 0 {
		return ""
	}
	return value[:len(value)-size]
}
