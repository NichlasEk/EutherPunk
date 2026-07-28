package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/NichlasEk/EutherPunk/internal/config"
)

var (
	version       = "dev"
	defaultAPIURL = ""
	defaultModel  = ""
	cliHTTPClient = &http.Client{Timeout: 5 * time.Minute}
)

type cliConfig struct {
	apiURL     string
	model      string
	configPath string
	memory     memoryState
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Message       string        `json:"message,omitempty"`
	Model         string        `json:"model,omitempty"`
	Messages      []chatMessage `json:"messages,omitempty"`
	ClientContext string        `json:"client_context,omitempty"`
}

type chatResponse struct {
	Model   string `json:"model"`
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

type streamChunk struct {
	Delta string `json:"delta,omitempty"`
	Done  bool   `json:"done,omitempty"`
	Error string `json:"error,omitempty"`
}

func main() {
	configPath := config.DefaultPath()
	_, statErr := os.Stat(configPath)
	configExists := statErr == nil

	appConfig, err := config.Load("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	memory, memoryErr := loadMemoryState(appConfig.Path)

	cfg := cliConfig{
		apiURL:     strings.TrimRight(cliValue("EUTHERPUNK_URL", appConfig.Agent.APIURL, defaultAPIURL, configExists), "/"),
		model:      cliValue("EUTHERPUNK_MODEL", appConfig.Agent.Model, defaultModel, configExists),
		configPath: appConfig.Path,
		memory:     memory,
	}
	if memoryErr != nil {
		fmt.Fprintln(os.Stderr, "minnesvarning:", memoryErr)
	}

	if len(os.Args) == 1 {
		err = assist(cfg, "")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	err = nil
	switch os.Args[1] {
	case "assist":
		err = assist(cfg, strings.Join(os.Args[2:], " "))
	case "doctor":
		err = doctor(cfg)
	case "status":
		err = printGet(cfg.apiURL + "/api/eutherpunk/status")
	case "models":
		err = printGet(cfg.apiURL + "/api/eutherpunk/models")
	case "users":
		err = printGet(cfg.apiURL + "/api/eutherpunk/users")
	case "ask":
		err = ask(cfg, strings.Join(os.Args[2:], " "))
	case "chat":
		err = chat(cfg, strings.Join(os.Args[2:], " "))
	case "version", "--version", "-version":
		fmt.Println("EutherPunk", version)
	case "help", "--help", "-h":
		usage()
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
	fmt.Println("EutherPunk CLI", version)
	fmt.Println("mode: portable safe preview")
	fmt.Println("local_access: system-info (ask); files and shell disabled")
	fmt.Println("config:", cfg.configPath)
	fmt.Println("api_url:", cfg.apiURL)
	fmt.Println("model:", cfg.model)
	fmt.Println("memory:", cfg.memory.StatusLine())
	fmt.Println()
	fmt.Println("status:")
	return printGet(cfg.apiURL + "/api/eutherpunk/status")
}

func assist(cfg cliConfig, initialPrompt string) error {
	fmt.Printf("EutherPunk %s\n", version)
	fmt.Println("Försiktig förhandsversion: chatt och godkänd systeminformation.")
	fmt.Println("CLI:t kan inte läsa dina filer, köra valfria kommandon eller ändra datorn.")
	fmt.Println("Skriv /help för hjälp eller /exit för att avsluta.")
	fmt.Printf("Modell: %s\n\n", cfg.model)
	fmt.Println("Minne:", cfg.memory.StatusLine())
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	editor := newLineEditor(reader)
	history := make([]chatMessage, 0, 12)
	permissions := defaultSessionPermissions()
	prompt := strings.TrimSpace(initialPrompt)

	for {
		if prompt == "" {
			line, err := editor.ReadLine("du> ")
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			prompt = strings.TrimSpace(line)
			if prompt == "" && errors.Is(err, io.EOF) {
				return nil
			}
			if prompt == "" {
				continue
			}
		}

		lowerPrompt := strings.ToLower(strings.TrimSpace(prompt))
		if strings.HasPrefix(lowerPrompt, "/permissions") {
			handlePermissionsCommand(&permissions, prompt)
			prompt = ""
			continue
		}
		if strings.HasPrefix(lowerPrompt, "/memory") {
			if err := handleMemoryCommand(&cfg.memory, prompt); err != nil {
				fmt.Fprintln(os.Stderr, "minnesfel:", err)
			}
			prompt = ""
			continue
		}
		if lowerPrompt == "/system" || lowerPrompt == "/system share" {
			report, allowed, err := approvedSystemReport(reader, &permissions)
			if err != nil {
				return err
			}
			if !allowed {
				prompt = ""
				continue
			}
			fmt.Println()
			fmt.Println(report.String())
			if lowerPrompt == "/system" {
				fmt.Println()
				fmt.Println("Informationen stannar lokalt. Använd /system share för att dela den med EutherPunk.")
				prompt = ""
				continue
			}
			prompt = report.SharedPrompt()
		}

		switch lowerPrompt {
		case "/exit", "/quit", "exit", "quit":
			return nil
		case "/help":
			fmt.Println("Skriv ett meddelande och tryck Enter.")
			fmt.Println("Uppåtpil eller Tab accepterar ett giftgrönt kommandoförslag.")
			fmt.Println("Uppåtpil visar historik när inget förslag syns.")
			fmt.Println("/memory visar eller ändrar det lokala långtidsminnet.")
			fmt.Println("/permissions visar eller ändrar lokala behörigheter.")
			fmt.Println("/system visar grundläggande systeminformation lokalt.")
			fmt.Println("/system share delar rapporten med modellen.")
			fmt.Println("/clear glömmer den lokala samtalstråden.")
			fmt.Println("/status kontrollerar anslutningen.")
			fmt.Println("/exit avslutar.")
			prompt = ""
			continue
		case "/clear":
			history = history[:0]
			fmt.Println("Samtalstråden är tömd.")
			prompt = ""
			continue
		case "/status":
			if err := printGet(cfg.apiURL + "/api/eutherpunk/status"); err != nil {
				fmt.Fprintln(os.Stderr, "anslutningsfel:", err)
			}
			prompt = ""
			continue
		}

		history = append(history, chatMessage{Role: "user", Content: prompt})
		fmt.Print("eutherpunk> ")
		answer, err := streamChat(cfg, trimHistory(history), os.Stdout)
		if err != nil {
			fmt.Println()
			fmt.Fprintln(os.Stderr, "anslutningsfel:", err)
			history = history[:len(history)-1]
			prompt = ""
			continue
		}
		fmt.Println()
		history = append(history, chatMessage{Role: "assistant", Content: answer})
		history = trimHistory(history)
		prompt = ""
	}
}

func chat(cfg cliConfig, prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return errors.New("chat requires a prompt")
	}

	_, err := streamChat(cfg, []chatMessage{{Role: "user", Content: prompt}}, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Println()
	return nil
}

func streamChat(cfg cliConfig, messages []chatMessage, output io.Writer) (string, error) {
	raw, err := json.Marshal(chatRequest{
		Messages:      chatOnlyMessages(messages),
		Model:         cfg.model,
		ClientContext: cfg.memory.ClientContext(),
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(http.MethodPost, cfg.apiURL+"/api/eutherpunk/chat/stream", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "eutherpunk-cli/"+version)
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("%s: %s", resp.Status, string(body))
	}

	decoder := json.NewDecoder(resp.Body)
	var answer strings.Builder
	for {
		var chunk streamChunk
		if err := decoder.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return answer.String(), err
		}
		if chunk.Error != "" {
			return answer.String(), errors.New(chunk.Error)
		}
		if chunk.Delta != "" {
			answer.WriteString(chunk.Delta)
			if _, err := io.WriteString(output, chunk.Delta); err != nil {
				return answer.String(), err
			}
		}
		if chunk.Done {
			break
		}
	}
	return answer.String(), nil
}

func ask(cfg cliConfig, prompt string) error {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return errors.New("ask requires a prompt")
	}

	raw, err := json.Marshal(chatRequest{
		Message:       "/chat " + prompt,
		Model:         cfg.model,
		ClientContext: cfg.memory.ClientContext(),
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, cfg.apiURL+"/api/eutherpunk/chat", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "eutherpunk-cli/"+version)
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	resp, err := cliHTTPClient.Do(req)
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
	fmt.Fprintln(os.Stderr, "  eutherpunk")
	fmt.Fprintln(os.Stderr, "  eutherpunk assist [first prompt]")
	fmt.Fprintln(os.Stderr, "  eutherpunk doctor")
	fmt.Fprintln(os.Stderr, "  eutherpunk status")
	fmt.Fprintln(os.Stderr, "  eutherpunk models")
	fmt.Fprintln(os.Stderr, "  eutherpunk users")
	fmt.Fprintln(os.Stderr, "  eutherpunk ask <prompt>")
	fmt.Fprintln(os.Stderr, "  eutherpunk chat <prompt>")
	fmt.Fprintln(os.Stderr, "  eutherpunk version")
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func cliValue(envName, configValue, releaseValue string, configExists bool) string {
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return value
	}
	if configExists || strings.TrimSpace(releaseValue) == "" {
		return configValue
	}
	return releaseValue
}

func trimHistory(messages []chatMessage) []chatMessage {
	const maxMessages = 12
	if len(messages) <= maxMessages {
		return messages
	}
	out := make([]chatMessage, maxMessages)
	copy(out, messages[len(messages)-maxMessages:])
	return out
}

func chatOnlyMessages(messages []chatMessage) []chatMessage {
	out := make([]chatMessage, len(messages))
	copy(out, messages)
	for i := range out {
		if out[i].Role == "user" {
			out[i].Content = "/chat " + out[i].Content
		}
	}
	return out
}
