package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type cliConfig struct {
	apiURL string
	model  string
}

type chatRequest struct {
	Message string `json:"message"`
	Model   string `json:"model,omitempty"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

func main() {
	cfg := cliConfig{
		apiURL: strings.TrimRight(envOr("EUTHERPUNK_URL", "http://127.0.0.1:8787"), "/"),
		model:  envOr("EUTHERPUNK_MODEL", "qwen3-coder:30b"),
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "doctor":
		err = doctor(cfg)
	case "status":
		err = printGet(cfg.apiURL + "/api/eutherpunk/status")
	case "models":
		err = printGet(cfg.apiURL + "/api/eutherpunk/models")
	case "ask":
		err = ask(cfg, strings.Join(os.Args[2:], " "))
	default:
		usage()
		err = fmt.Errorf("unknown command: %s", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func doctor(cfg cliConfig) error {
	fmt.Println("EutherPunk CLI")
	fmt.Println("api_url:", cfg.apiURL)
	fmt.Println("model:", cfg.model)
	fmt.Println()
	fmt.Println("status:")
	return printGet(cfg.apiURL + "/api/eutherpunk/status")
}

func ask(cfg cliConfig, prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return errors.New("ask requires a prompt")
	}

	raw, err := json.Marshal(chatRequest{Message: prompt, Model: cfg.model})
	if err != nil {
		return err
	}

	client := http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Post(cfg.apiURL+"/api/eutherpunk/chat", "application/json", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, string(body))
	}

	var out chatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	if out.Error != "" {
		return errors.New(out.Error)
	}
	fmt.Println(out.Message)
	return nil
}

func printGet(url string) error {
	client := http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	fmt.Println(string(body))
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  eutherpunk doctor")
	fmt.Fprintln(os.Stderr, "  eutherpunk status")
	fmt.Fprintln(os.Stderr, "  eutherpunk models")
	fmt.Fprintln(os.Stderr, "  eutherpunk ask <prompt>")
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
