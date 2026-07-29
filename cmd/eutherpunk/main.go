package main

import (
	"bufio"
	"bytes"
	"context"
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
	apiURL             string
	model              string
	configPath         string
	memory             memoryState
	settings           cliSettings
	credentials        authCredentials
	workspace          workspaceState
	nonInteractiveAuth bool
	verifierDriven     bool
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Message        string        `json:"message,omitempty"`
	Model          string        `json:"model,omitempty"`
	Messages       []chatMessage `json:"messages,omitempty"`
	ClientContext  string        `json:"client_context,omitempty"`
	LocalWorkspace bool          `json:"local_workspace,omitempty"`
	VerifierDriven bool          `json:"verifier_driven,omitempty"`
}

type chatResponse struct {
	Model   string          `json:"model"`
	Message string          `json:"message"`
	Files   []workspaceFile `json:"files,omitempty"`
	Error   string          `json:"error,omitempty"`
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
	baseAPIURL := strings.TrimRight(cliValue("EUTHERPUNK_URL", appConfig.Agent.APIURL, defaultAPIURL, configExists), "/")
	baseModel := cliValue("EUTHERPUNK_MODEL", appConfig.Agent.Model, defaultModel, configExists)
	memory, memoryErr := loadMemoryState(appConfig.Path)
	settingsDefaults := defaultCLISettings(appConfig.Path, baseAPIURL, baseModel, memory.Enabled)
	settings, settingsErr := loadCLISettings(settingsDefaults)
	if settingsErr == nil && !settings.Exists {
		if err := settings.Save(); err != nil {
			settingsErr = fmt.Errorf("skapa settings.toml: %w", err)
		}
	}
	if settingsErr != nil {
		settings = settingsDefaults
	}
	if settingsErr == nil && settings.Exists {
		memory, memoryErr = loadMemoryStateFromSettings(settings)
	}

	cfg := cliConfig{
		apiURL:     baseAPIURL,
		model:      baseModel,
		configPath: appConfig.Path,
		memory:     memory,
		settings:   settings,
	}
	if settingsErr == nil && settings.Exists {
		if strings.TrimSpace(os.Getenv("EUTHERPUNK_URL")) == "" {
			cfg.apiURL = strings.TrimRight(settings.ConnectionURL, "/")
		}
		if strings.TrimSpace(os.Getenv("EUTHERPUNK_MODEL")) == "" {
			cfg.model = settings.Model
		}
	}
	if settingsErr != nil {
		fmt.Fprintln(os.Stderr, "inställningsvarning:", settingsErr)
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
		err = authenticatedGet(&cfg, cfg.apiURL+"/api/eutherpunk/models")
	case "users":
		err = authenticatedGet(&cfg, cfg.apiURL+"/api/eutherpunk/users")
	case "auth":
		if len(os.Args) > 2 && os.Args[2] == "login" {
			err = cfg.login()
		} else {
			err = cfg.authStatus()
		}
	case "logout":
		err = cfg.logout()
	case "ask":
		err = ask(cfg, strings.Join(os.Args[2:], " "))
	case "chat":
		err = chat(cfg, strings.Join(os.Args[2:], " "))
	case "worker":
		err = runWorker(&cfg, os.Args[2:], os.Stdout, os.Stderr)
	case "trace":
		err = runTrace(os.Args[2:], os.Stdout, os.Stderr)
	case "eval":
		err = runEval(&cfg, os.Args[2:], os.Stdout, os.Stderr)
	case "dataset":
		err = runDataset(os.Args[2:], os.Stdout, os.Stderr)
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
	fmt.Println("local_access: system-info and explicit workspace files (ask); shell disabled")
	fmt.Println("config:", cfg.configPath)
	fmt.Println("api_url:", cfg.apiURL)
	fmt.Println("model:", cfg.model)
	fmt.Println("memory:", cfg.memory.StatusLine())
	fmt.Println("settings:", cfg.settings.Path)
	fmt.Println()
	fmt.Println("status:")
	return printGet(cfg.apiURL + "/api/eutherpunk/status")
}

func assist(cfg cliConfig, initialPrompt string) error {
	reader := bufio.NewReader(os.Stdin)
	if err := offerCurrentDirectoryWorkspace(reader, &cfg.workspace); err != nil {
		fmt.Fprintln(os.Stderr, "workspace warning:", err)
	}
	if err := cfg.ensureAuthenticated(true); err != nil {
		return err
	}

	fmt.Printf("EutherPunk %s\n", version)
	fmt.Println("Försiktig förhandsversion: chatt, systeminformation och avgränsade kodarbetsytor.")
	if cfg.settings.Files == permissionAuto {
		fmt.Println("Arbetsytefiler: AUTO — godkänd kod skrivs automatiskt, endast i vald arbetsyta.")
	} else {
		fmt.Println("CLI:t kan bara läsa vald arbetsyta och frågar alltid innan filer ändras.")
	}
	fmt.Println("Valfria kommandon och administratörsåtkomst är avstängda.")
	fmt.Println("Skriv /help för hjälp eller /exit för att avsluta.")
	fmt.Printf("Modell: %s\n\n", cfg.model)
	fmt.Println("Minne:", cfg.memory.StatusLine())
	fmt.Println()

	editor := newLineEditor(reader, cfg.settings.Terminal)
	history := make([]chatMessage, 0, 12)
	permissions := defaultSessionPermissions()
	pendingJob := pendingWorkspaceJob{}
	if cfg.settings.Exists {
		permissions.systemInfo = cfg.settings.SystemInfo
		permissions.files = cfg.settings.Files
	}
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
		if strings.HasPrefix(lowerPrompt, "/workspace") {
			if err := handleWorkspaceCommand(&cfg.workspace, prompt); err != nil {
				fmt.Fprintln(os.Stderr, "arbetsytefel:", err)
			}
			prompt = ""
			continue
		}
		if strings.HasPrefix(lowerPrompt, "/settings") {
			if err := handleSettingsCommand(&cfg, &permissions, editor, prompt); err != nil {
				fmt.Fprintln(os.Stderr, "inställningsfel:", err)
			}
			prompt = ""
			continue
		}
		if strings.HasPrefix(lowerPrompt, "/memory") {
			wasEnabled := cfg.memory.Enabled
			if err := handleMemoryCommand(&cfg.memory, prompt); err != nil {
				fmt.Fprintln(os.Stderr, "minnesfel:", err)
			} else if cfg.settings.Exists && wasEnabled != cfg.memory.Enabled {
				cfg.settings.MemoryEnabled = cfg.memory.Enabled
				if err := cfg.settings.Save(); err != nil {
					fmt.Fprintln(os.Stderr, "inställningsfel:", err)
				} else {
					_ = os.Remove(cfg.memory.EnabledPath)
					fmt.Println("Inställningen är sparad i settings.toml.")
				}
			}
			prompt = ""
			continue
		}
		if lowerPrompt == "/job" || strings.HasPrefix(lowerPrompt, "/job ") {
			if err := handleWorkspaceJobCommand(
				cfg,
				prompt,
				&pendingJob,
				reader,
				&permissions,
				os.Stdout,
			); err != nil {
				fmt.Fprintln(os.Stderr, "kodjobbsfel:", err)
			}
			prompt = ""
			continue
		}
		if lowerPrompt == "/system" || lowerPrompt == "/system share" || lowerPrompt == "/system share full" {
			report, allowed, err := approvedSystemReport(reader, &permissions)
			if err != nil {
				return err
			}
			if !allowed {
				prompt = ""
				continue
			}
			if lowerPrompt == "/system" {
				fmt.Println()
				fmt.Println(report.String())
				fmt.Println()
				fmt.Println("Informationen stannar lokalt. Använd /system share för att dela den med EutherPunk.")
				prompt = ""
				continue
			}
			full := lowerPrompt == "/system share full"
			if full {
				fmt.Println()
				fmt.Println("FULL SYSTEMRAPPORT")
				fmt.Println(report.StringForShare(cfg.settings.Privacy, true))
				fmt.Println()
				fmt.Print("Detta delar datornamn, användarnamn och arbetskatalog. Fortsätt? [y/N]: ")
				answer, err := reader.ReadString('\n')
				if err != nil {
					return err
				}
				switch strings.ToLower(strings.TrimSpace(answer)) {
				case "y", "yes", "j", "ja":
				default:
					fmt.Println("Delningen avbröts.")
					prompt = ""
					continue
				}
			}
			fmt.Println()
			fmt.Println("DETTA DELAS MED MODELLEN")
			fmt.Println(report.StringForShare(cfg.settings.Privacy, full))
			prompt = report.SharedPrompt(cfg.settings.Privacy, full)
		}

		switch lowerPrompt {
		case "/exit", "/quit", "exit", "quit":
			if pendingJob.Job.Status == "queued" || pendingJob.Job.Status == "running" {
				cancelWorkspaceJob(cfg, pendingJob.Job.ID)
			}
			return nil
		case "/help":
			fmt.Println("Skriv ett meddelande och tryck Enter.")
			fmt.Println("Tryck Esc två gånger inom 1,5 sekunder för att avbryta agenten.")
			fmt.Println("Uppåtpil eller Tab accepterar ett giftgrönt kommandoförslag.")
			fmt.Println("Uppåtpil visar historik när inget förslag syns.")
			fmt.Println("/memory visar eller ändrar det lokala långtidsminnet.")
			fmt.Println("/settings visar eller sparar permanenta inställningar.")
			fmt.Println("/permissions visar eller ändrar lokala behörigheter.")
			fmt.Println("/permissions files auto följer, reparerar och skriver kodjobb automatiskt i vald arbetsyta.")
			fmt.Println("/workspace init <katalog> skapar och väljer en avgränsad kodarbetsyta.")
			fmt.Println("/workspace use <katalog> väljer en befintlig kodarbetsyta.")
			fmt.Println("/job visar det aktiva kodjobbets bygglogg och status.")
			fmt.Println("/job wait följer jobbet tills filförslaget kan granskas.")
			fmt.Println("/job open öppnar ett färdigt förslag; /job cancel avbryter.")
			fmt.Println("/system visar grundläggande systeminformation lokalt.")
			fmt.Println("/system share delar en maskerad rapport med modellen.")
			fmt.Println("/system share full delar även identifierande fält efter extra godkännande.")
			fmt.Println("/clear glömmer den lokala samtalstråden.")
			fmt.Println("/status kontrollerar anslutningen.")
			fmt.Println("/auth visar EutherID-inloggningen.")
			fmt.Println("/auth login öppnar en ny säker webbläsarinloggning.")
			fmt.Println("/logout återkallar och tar bort CLI-inloggningen.")
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
		case "/auth":
			if err := cfg.authStatus(); err != nil {
				fmt.Fprintln(os.Stderr, "inloggningsfel:", err)
			}
			prompt = ""
			continue
		case "/auth login":
			if err := cfg.login(); err != nil {
				fmt.Fprintln(os.Stderr, "inloggningsfel:", err)
			}
			prompt = ""
			continue
		case "/logout":
			if err := cfg.logout(); err != nil {
				fmt.Fprintln(os.Stderr, "utloggningsfel:", err)
			}
			prompt = ""
			continue
		}

		history = append(history, chatMessage{Role: "user", Content: prompt})
		if pendingJob.Job.ID != "" {
			if refreshErr := refreshPendingWorkspaceJob(
				context.Background(),
				cfg,
				&pendingJob,
				os.Stdout,
			); refreshErr != nil {
				fmt.Fprintln(os.Stderr, "kodjobbsvarning:", refreshErr)
			} else {
				switch strings.ToLower(strings.TrimSpace(pendingJob.Job.Status)) {
				case "failed", "cancelled", "expired":
					fmt.Printf(
						"eutherpunk> föregående kodjobb är %s; nästa arbetsyteförfrågan kan starta ett nytt jobb.\n",
						workspaceJobStatusLabel(pendingJob.Job.Status),
					)
					pendingJob = pendingWorkspaceJob{}
				}
			}
		}
		if cfg.workspace.Root != "" && pendingJob.Job.ID == "" {
			fmt.Println("eutherpunk> startar kodjobb…")
			startedJob, started, startErr := beginBackgroundWorkspaceJob(
				context.Background(),
				cfg,
				trimHistory(history),
				reader,
				&permissions,
				os.Stdout,
			)
			if startErr != nil {
				fmt.Fprintln(os.Stderr, "kodjobbsfel:", startErr)
				history = history[:len(history)-1]
				prompt = ""
				continue
			}
			if started {
				pendingJob = startedJob
				if permissions.files == permissionAuto {
					fmt.Println("eutherpunk> AUTO följer jobbet, reparerar vid behov och skriver godkända filer.")
					if err := waitAndReviewPendingWorkspaceJob(
						cfg,
						&pendingJob,
						reader,
						&permissions,
						os.Stdout,
					); err != nil {
						fmt.Fprintln(os.Stderr, "kodjobbsfel:", err)
						history = history[:len(history)-1]
					} else {
						history = append(history, chatMessage{
							Role:    "assistant",
							Content: "[Kodjobbet slutfördes automatiskt i arbetsytan.]",
						})
						history = trimHistory(history)
					}
					prompt = ""
					continue
				}
				history = append(history, chatMessage{
					Role:    "assistant",
					Content: "[Kodjobbet arbetar i bakgrunden. Använd /job för status.]",
				})
				history = trimHistory(history)
				prompt = ""
				continue
			}
		}

		fmt.Println("eutherpunk> arbetar… (Esc Esc avbryter)")
		var answer string
		var err error
		if pendingJob.Job.ID != "" {
			fmt.Printf(
				"eutherpunk> kodjobbet är %s; chatten svarar separat via %s och kan köas medan GPU:n är upptagen.\n",
				workspaceJobStatusLabel(pendingJob.Job.Status),
				cfg.model,
			)
		}
		imageOutput := newImageDirectiveWriter(os.Stdout)
		answer, err = runInterruptibleAgentCall(os.Stdout, func(ctx context.Context) (string, error) {
			return streamChatContext(ctx, cfg, trimHistory(history), imageOutput)
		})
		outputErr := imageOutput.Finish()
		if err != nil {
			fmt.Println()
			if errors.Is(err, errAgentInterrupted) {
				fmt.Println("Agenten avbröts. Du kan fortsätta skriva.")
				history = history[:len(history)-1]
				prompt = ""
				continue
			}
			fmt.Fprintln(os.Stderr, "anslutningsfel:", err)
			history = history[:len(history)-1]
			prompt = ""
			continue
		}
		if outputErr != nil {
			fmt.Println()
			fmt.Fprintln(os.Stderr, "utskriftsfel:", outputErr)
			history = history[:len(history)-1]
			prompt = ""
			continue
		}
		fmt.Println()
		visibleAnswer, imagePrompt, hasImagePrompt := extractImageToolDirective(answer)
		if visibleAnswer == "" && hasImagePrompt {
			visibleAnswer = "Jag skickar bildprompten till EutherPunks bildgenerator."
			fmt.Println("eutherpunk>", visibleAnswer)
		}
		history = append(history, chatMessage{Role: "assistant", Content: visibleAnswer})
		history = trimHistory(history)
		if hasImagePrompt {
			fmt.Println("eutherpunk> genererar bildasset… (Esc Esc avbryter)")
			var image cliImageResponse
			_, imageErr := runInterruptibleAgentCall(os.Stdout, func(ctx context.Context) (string, error) {
				var generateErr error
				image, generateErr = generateCLIImage(ctx, cfg, imagePrompt, trimHistory(history), os.Stdout)
				return "", generateErr
			})
			if imageErr != nil {
				if errors.Is(imageErr, errAgentInterrupted) {
					fmt.Println("Bildjobbet avbröts. Du kan fortsätta skriva.")
				} else if strings.Contains(imageErr.Error(), "insufficient_scope") {
					fmt.Fprintln(os.Stderr, "bildfel: din befintliga CLI-inloggning saknar mediaåtkomst; kör /logout och starta eutherpunk igen för att godkänna det nya scopet")
				} else {
					fmt.Fprintln(os.Stderr, "bildfel:", imageErr)
				}
			} else {
				assetPath, saved, saveErr := saveCLIImageAsset(
					context.Background(),
					cfg,
					reader,
					&permissions,
					image,
				)
				if saveErr != nil {
					fmt.Fprintln(os.Stderr, "bildfel:", saveErr)
				} else if saved {
					fmt.Println("eutherpunk> bildasset sparad:", assetPath)
					history = append(history, chatMessage{
						Role:    "assistant",
						Content: "Bildasset sparad i arbetsytan: " + assetPath,
					})
					history = trimHistory(history)
				} else {
					fmt.Println("eutherpunk> bild klar på servern:", absoluteCLIURL(cfg.apiURL, image.URL))
				}
			}
		}
		prompt = ""
	}
}

func workspaceJobStatusLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "queued":
		return "köat"
	case "running":
		return "igång"
	case "completed":
		return "klart och väntar på /job open"
	case "needs_review":
		return "klart men behöver granskas med /job open"
	case "failed":
		return "misslyckat; använd /job"
	case "cancelled":
		return "avbrutet"
	case "expired":
		return "utgånget"
	default:
		return "aktivt"
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
	return streamChatContext(context.Background(), cfg, messages, output)
}

func streamChatContext(ctx context.Context, cfg cliConfig, messages []chatMessage, output io.Writer) (string, error) {
	raw, err := json.Marshal(chatRequest{
		Messages:      chatOnlyMessages(messages),
		Model:         cfg.model,
		ClientContext: cfg.memory.ClientContext(),
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.apiURL+"/api/eutherpunk/chat/stream", bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "eutherpunk-cli/"+version)
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	req.Header.Set("X-EutherPunk-Client-Capabilities", "image-tool")
	if err := cfg.authorize(req); err != nil {
		return "", err
	}
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
	req.Header.Set("X-EutherPunk-Client-Capabilities", "image-tool")
	if err := cfg.authorize(req); err != nil {
		return err
	}
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

func authenticatedGet(cfg *cliConfig, url string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if err := cfg.authorize(req); err != nil {
		return err
	}
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
	fmt.Fprintln(os.Stderr, "  eutherpunk auth [login]")
	fmt.Fprintln(os.Stderr, "  eutherpunk logout")
	fmt.Fprintln(os.Stderr, "  eutherpunk ask <prompt>")
	fmt.Fprintln(os.Stderr, "  eutherpunk chat <prompt>")
	fmt.Fprintln(os.Stderr, "  eutherpunk worker --workspace <directory> --task <task> [--apply]")
	fmt.Fprintln(os.Stderr, "  eutherpunk trace finalize --result <worker.json> --workspace <directory> --diagnostics <file> --verdict accepted|rejected --output <trace.json>")
	fmt.Fprintln(os.Stderr, "  eutherpunk eval run --suite <suite.json> --output <directory> [--case <id>]")
	fmt.Fprintln(os.Stderr, "  eutherpunk dataset build --input <trace-or-directory> [--input ...] --output <directory>")
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
