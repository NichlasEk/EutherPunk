package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

const (
	maxWorkspaceFiles        = 32
	maxWorkspaceFileBytes    = 16 * 1024
	maxWorkspaceContextBytes = 48 * 1024
	maxProposalFiles         = 16
	maxProposalFileBytes     = 64 * 1024
	maxProposalBytes         = 256 * 1024
)

type workspaceState struct {
	Root string
}

type workspaceFile struct {
	Path    string
	Content string
}

type fileProposal struct {
	Files []workspaceFile `json:"files"`
}

type workspaceJobResponse struct {
	ID         string                 `json:"id"`
	Status     string                 `json:"status"`
	Model      string                 `json:"model,omitempty"`
	Message    string                 `json:"message,omitempty"`
	Activities []workspaceJobActivity `json:"activities,omitempty"`
	Files      []workspaceFile        `json:"files,omitempty"`
	Error      string                 `json:"error,omitempty"`
}

type workspaceJobActivity struct {
	Sequence int    `json:"sequence"`
	Message  string `json:"message"`
}

type pendingWorkspaceJob struct {
	Job          workspaceJobResponse
	LastActivity int
}

func offerCurrentDirectoryWorkspace(reader *bufio.Reader, state *workspaceState) error {
	if state.Root != "" || !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil
	}
	current, err := os.Getwd()
	if err != nil {
		return err
	}
	current, err = filepath.EvalSymlinks(current)
	if err != nil {
		return err
	}
	info, err := os.Stat(current)
	if err != nil {
		return err
	}
	if !info.IsDir() || unsafeWorkspaceRoot(current) {
		return nil
	}

	fmt.Println("Current directory:", current)
	fmt.Print("Would you like to initialize this directory as an EutherPunk workspace? [y/N]: ")
	answer, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		state.Root = filepath.Clean(current)
		fmt.Println("Workspace initialized for this session.")
		fmt.Println("EutherPunk cannot access files outside this directory.")
		fmt.Println()
	}
	return nil
}

func workspaceChat(
	cfg cliConfig,
	messages []chatMessage,
	reader *bufio.Reader,
	permissions *sessionPermissions,
	output io.Writer,
) (string, error) {
	toolContext, allowed, err := approvedWorkspaceContext(reader, cfg.workspace, permissions)
	if err != nil {
		return "", err
	}
	if !allowed {
		return runInterruptibleAgentCall(output, func(ctx context.Context) (string, error) {
			return streamChatContext(ctx, cfg, messages, output)
		})
	}
	var structuredProposal fileProposal
	answer, err := runInterruptibleAgentCall(output, func(ctx context.Context) (string, error) {
		var requestErr error
		var response string
		response, structuredProposal, requestErr = requestWorkspaceAnswer(ctx, cfg, messages, toolContext, output)
		return response, requestErr
	})
	if err != nil {
		return "", err
	}
	return reviewWorkspaceResult(reader, cfg.workspace, permissions, output, answer, structuredProposal)
}

func reviewWorkspaceResult(
	reader *bufio.Reader,
	workspace workspaceState,
	permissions *sessionPermissions,
	output io.Writer,
	answer string,
	structuredProposal fileProposal,
) (string, error) {
	proposal := structuredProposal
	visible := answer
	found := len(proposal.Files) > 0
	if !found {
		var parseErr error
		proposal, visible, found, parseErr = parseFileProposal(answer)
		if parseErr != nil {
			_, _ = io.WriteString(output, answer)
			return answer, parseErr
		}
	}
	if visible != "" {
		if _, err := io.WriteString(output, visible); err != nil {
			return "", err
		}
	}
	if !found {
		return answer, nil
	}
	if visible != "" {
		_, _ = io.WriteString(output, "\n")
	}
	applied, err := approveAndApplyProposal(reader, workspace, permissions, proposal)
	if err != nil {
		return visible, err
	}
	summary := fmt.Sprintf("[Lokalt filförslag: %d fil(er), nekat]", len(proposal.Files))
	if applied {
		summary = fmt.Sprintf("[Lokalt filverktyg: %d fil(er) skrivna i arbetsytan]", len(proposal.Files))
	}
	if visible != "" {
		return visible + "\n" + summary, nil
	}
	return summary, nil
}

func beginBackgroundWorkspaceJob(
	ctx context.Context,
	cfg cliConfig,
	messages []chatMessage,
	reader *bufio.Reader,
	permissions *sessionPermissions,
	output io.Writer,
) (pendingWorkspaceJob, bool, error) {
	toolContext, allowed, err := approvedWorkspaceContext(reader, cfg.workspace, permissions)
	if err != nil || !allowed {
		return pendingWorkspaceJob{}, false, err
	}
	job, status, err := startWorkspaceJob(ctx, cfg, messages, toolContext)
	if err != nil {
		return pendingWorkspaceJob{}, false, err
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		return pendingWorkspaceJob{}, false, fmt.Errorf("starta kodjobb: HTTP %d: %s", status, job.Error)
	}
	if job.ID == "" {
		return pendingWorkspaceJob{}, false, errors.New("servern returnerade inget jobb-ID")
	}
	printWorkspaceJobStarted(output, job)
	_, _ = io.WriteString(
		output,
		"Jobbet arbetar i bakgrunden. Du kan fortsätta prata; /job visar läget.\r\n",
	)
	return pendingWorkspaceJob{Job: job, LastActivity: 1}, true, nil
}

func handleWorkspaceJobCommand(
	cfg cliConfig,
	command string,
	pending *pendingWorkspaceJob,
	reader *bufio.Reader,
	permissions *sessionPermissions,
	output io.Writer,
) error {
	if pending.Job.ID == "" {
		_, _ = io.WriteString(output, "Inget kodjobb finns i den här CLI-sessionen.\n")
		return nil
	}
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(command)))
	action := "status"
	if len(fields) > 1 {
		action = fields[1]
	}
	switch action {
	case "cancel", "avbryt":
		cancelWorkspaceJob(cfg, pending.Job.ID)
		_, _ = io.WriteString(output, "Kodjobbet har avbrutits.\n")
		*pending = pendingWorkspaceJob{}
		return nil
	case "wait", "vänta", "vanta":
		var message string
		var proposal fileProposal
		err := func() error {
			answer, waitErr := runInterruptibleAgentCall(output, func(ctx context.Context) (string, error) {
				var innerErr error
				message, proposal, innerErr = waitWorkspaceJob(
					ctx,
					cfg,
					pending.Job,
					output,
					&pending.LastActivity,
				)
				return message, innerErr
			})
			message = answer
			return waitErr
		}()
		if err != nil {
			if errors.Is(err, errAgentInterrupted) {
				_, _ = io.WriteString(output, "\nBevakningen avbröts. Serverjobbet har också avbrutits.\n")
				*pending = pendingWorkspaceJob{}
				return nil
			}
			return err
		}
		_, err = reviewWorkspaceResult(reader, cfg.workspace, permissions, output, message, proposal)
		*pending = pendingWorkspaceJob{}
		return err
	case "open", "öppna", "oppna":
		if err := refreshPendingWorkspaceJob(context.Background(), cfg, pending, output); err != nil {
			return err
		}
		if pending.Job.Status != "completed" {
			_, _ = fmt.Fprintf(output, "Kodjobbet är %s. Använd /job wait för att följa det.\n", pending.Job.Status)
			return nil
		}
		message, proposal, err := validateWorkspaceJobResult(pending.Job)
		if err != nil {
			return err
		}
		if validationErr := validateProposalSyntax(proposal); validationErr != nil {
			repaired, repairErr := requestWorkspaceJobRepair(
				context.Background(),
				cfg,
				pending.Job.ID,
				validationErr.Error(),
			)
			if repairErr != nil {
				return fmt.Errorf("lokal kontroll: %v; reparation: %w", validationErr, repairErr)
			}
			pending.Job = repaired
			printWorkspaceJobActivities(output, repaired, &pending.LastActivity)
			_, _ = fmt.Fprintf(
				output,
				"Den lokala kontrollen hittade ett körfel och startade ett reparationsvarv:\n%s\n",
				validationErr,
			)
			return nil
		}
		_, err = reviewWorkspaceResult(reader, cfg.workspace, permissions, output, message, proposal)
		*pending = pendingWorkspaceJob{}
		return err
	case "status":
		if err := refreshPendingWorkspaceJob(context.Background(), cfg, pending, output); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(output, "Kodjobb %s: %s.\n", shortWorkspaceJobID(pending.Job.ID), pending.Job.Status)
		if pending.Job.Status == "completed" {
			_, _ = io.WriteString(output, "Förslaget är klart. Använd /job open för att granska det.\n")
		}
		return nil
	default:
		return errors.New("använd /job, /job wait, /job open eller /job cancel")
	}
}

func shortWorkspaceJobID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func requestWorkspaceAnswer(
	ctx context.Context,
	cfg cliConfig,
	messages []chatMessage,
	toolContext string,
	output io.Writer,
) (string, fileProposal, error) {
	job, status, err := startWorkspaceJob(ctx, cfg, messages, toolContext)
	if err != nil {
		return "", fileProposal{}, err
	}
	if status == http.StatusNotFound {
		raw, marshalErr := workspaceJobRequestBody(cfg, messages, toolContext)
		if marshalErr != nil {
			return "", fileProposal{}, marshalErr
		}
		return requestWorkspaceAnswerLegacy(ctx, cfg, raw)
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		return "", fileProposal{}, fmt.Errorf("starta kodjobb: HTTP %d: %s", status, job.Error)
	}
	if job.ID == "" {
		return "", fileProposal{}, errors.New("servern returnerade inget jobb-ID")
	}
	printWorkspaceJobStarted(output, job)
	lastActivity := 1
	return waitWorkspaceJob(ctx, cfg, job, output, &lastActivity)
}

func workspaceJobRequestBody(
	cfg cliConfig,
	messages []chatMessage,
	toolContext string,
) ([]byte, error) {
	clientContext := strings.TrimSpace(cfg.memory.ClientContext())
	if clientContext != "" {
		clientContext += "\n\n"
	}
	clientContext += toolContext
	return json.Marshal(chatRequest{
		Messages:       chatOnlyMessages(messages),
		Model:          cfg.model,
		ClientContext:  clientContext,
		LocalWorkspace: true,
	})
}

func startWorkspaceJob(
	ctx context.Context,
	cfg cliConfig,
	messages []chatMessage,
	toolContext string,
) (workspaceJobResponse, int, error) {
	raw, err := workspaceJobRequestBody(cfg, messages, toolContext)
	if err != nil {
		return workspaceJobResponse{}, 0, err
	}
	return requestWorkspaceJob(
		ctx,
		cfg,
		http.MethodPost,
		cfg.apiURL+"/api/eutherpunk/workspace/jobs",
		raw,
	)
}

func printWorkspaceJobStarted(output io.Writer, job workspaceJobResponse) {
	shortID := job.ID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	if job.Model != "" {
		_, _ = fmt.Fprintf(output, "Kodjobb %s startat med %s.\r\n", shortID, job.Model)
	} else {
		_, _ = fmt.Fprintf(output, "Kodjobb %s startat.\r\n", shortID)
	}
}

func waitWorkspaceJob(
	ctx context.Context,
	cfg cliConfig,
	job workspaceJobResponse,
	output io.Writer,
	lastActivity *int,
) (string, fileProposal, error) {
	started := time.Now()
	nextProgress := 10 * time.Second
	pollImmediately := true
	for {
		printWorkspaceJobActivities(output, job, lastActivity)
		switch job.Status {
		case "completed":
			totalBytes := 0
			for _, file := range job.Files {
				totalBytes += len(file.Content)
			}
			_, _ = fmt.Fprintf(
				output,
				"Lokal kontroll: granskar %d fil(er), %d byte…\r\n",
				len(job.Files),
				totalBytes,
			)
			message, proposal, err := validateWorkspaceJobResult(job)
			if err != nil {
				_, _ = fmt.Fprintf(output, "Lokal kontroll stoppade förslaget: %v\r\n", err)
				return "", fileProposal{}, err
			}
			if len(proposal.Files) == 0 {
				_, _ = io.WriteString(output, "Kodjobbet avslutades utan filförslag.\r\n")
				return message, proposal, nil
			}
			if validationErr := validateProposalSyntax(proposal); validationErr != nil {
				_, _ = fmt.Fprintf(
					output,
					"Lokal kontroll hittade ett körfel och skickar tillbaka det för reparation:\r\n%v\r\n",
					validationErr,
				)
				repaired, repairErr := requestWorkspaceJobRepair(
					ctx,
					cfg,
					job.ID,
					validationErr.Error(),
				)
				if repairErr != nil {
					return "", fileProposal{}, fmt.Errorf(
						"lokal kontroll: %v; reparation: %w",
						validationErr,
						repairErr,
					)
				}
				job = repaired
				started = time.Now()
				nextProgress = 10 * time.Second
				pollImmediately = true
				continue
			}
			_, _ = io.WriteString(output, "Lokal kontroll godkänd; visar förhandsvisning.\r\n")
			return message, proposal, nil
		case "failed":
			if job.Error == "" {
				job.Error = "okänt serverfel"
			}
			return "", fileProposal{}, errors.New(job.Error)
		case "cancelled":
			return "", fileProposal{}, errAgentInterrupted
		case "queued", "running":
		default:
			return "", fileProposal{}, fmt.Errorf("okänd kodjobbsstatus %q", job.Status)
		}

		elapsed := time.Since(started)
		if elapsed >= nextProgress {
			_, _ = fmt.Fprintf(output, "Kodjobbet arbetar fortfarande… %ds\r\n", int(elapsed.Round(time.Second).Seconds()))
			nextProgress += 10 * time.Second
		}
		if pollImmediately {
			pollImmediately = false
		} else {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				cancelWorkspaceJob(cfg, job.ID)
				return "", fileProposal{}, ctx.Err()
			case <-timer.C:
			}
		}
		nextJob, status, err := requestWorkspaceJob(
			ctx,
			cfg,
			http.MethodGet,
			cfg.apiURL+"/api/eutherpunk/workspace/jobs/"+url.PathEscape(job.ID),
			nil,
		)
		if err != nil {
			cancelWorkspaceJob(cfg, job.ID)
			return "", fileProposal{}, err
		}
		if status != http.StatusOK {
			cancelWorkspaceJob(cfg, job.ID)
			return "", fileProposal{}, fmt.Errorf("hämta kodjobb: HTTP %d: %s", status, nextJob.Error)
		}
		job = nextJob
	}
}

func printWorkspaceJobActivities(output io.Writer, job workspaceJobResponse, lastActivity *int) {
	for _, activity := range job.Activities {
		if activity.Sequence <= *lastActivity || strings.TrimSpace(activity.Message) == "" {
			continue
		}
		_, _ = fmt.Fprintf(output, "  ↳ %s\r\n", activity.Message)
		*lastActivity = activity.Sequence
	}
}

func refreshPendingWorkspaceJob(
	ctx context.Context,
	cfg cliConfig,
	pending *pendingWorkspaceJob,
	output io.Writer,
) error {
	if pending == nil || pending.Job.ID == "" {
		return nil
	}
	job, status, err := requestWorkspaceJob(
		ctx,
		cfg,
		http.MethodGet,
		cfg.apiURL+"/api/eutherpunk/workspace/jobs/"+url.PathEscape(pending.Job.ID),
		nil,
	)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("hämta kodjobb: HTTP %d: %s", status, job.Error)
	}
	pending.Job = job
	printWorkspaceJobActivities(output, job, &pending.LastActivity)
	return nil
}

func requestWorkspaceJob(
	ctx context.Context,
	cfg cliConfig,
	method, endpoint string,
	body []byte,
) (workspaceJobResponse, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return workspaceJobResponse{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "eutherpunk-cli/"+version)
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	if err := cfg.authorize(req); err != nil {
		return workspaceJobResponse{}, 0, err
	}
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return workspaceJobResponse{}, 0, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxProposalBytes+128*1024))
	if err != nil {
		return workspaceJobResponse{}, resp.StatusCode, err
	}
	var job workspaceJobResponse
	if len(strings.TrimSpace(string(responseBody))) > 0 {
		if err := json.Unmarshal(responseBody, &job); err != nil {
			return workspaceJobResponse{}, resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, string(responseBody))
		}
	}
	return job, resp.StatusCode, nil
}

func validateWorkspaceJobResult(job workspaceJobResponse) (string, fileProposal, error) {
	proposal := fileProposal{Files: job.Files}
	if len(proposal.Files) > maxProposalFiles {
		return "", fileProposal{}, errors.New("servern returnerade för många filförslag")
	}
	for _, file := range proposal.Files {
		if err := validateProposedFile(file); err != nil {
			return "", fileProposal{}, err
		}
	}
	return job.Message, proposal, nil
}

func cancelWorkspaceJob(cfg cliConfig, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, _ = requestWorkspaceJob(
		ctx,
		cfg,
		http.MethodDelete,
		cfg.apiURL+"/api/eutherpunk/workspace/jobs/"+url.PathEscape(id),
		nil,
	)
}

func requestWorkspaceJobRepair(
	ctx context.Context,
	cfg cliConfig,
	id, diagnostics string,
) (workspaceJobResponse, error) {
	raw, err := json.Marshal(struct {
		Diagnostics string `json:"diagnostics"`
	}{Diagnostics: diagnostics})
	if err != nil {
		return workspaceJobResponse{}, err
	}
	job, status, err := requestWorkspaceJob(
		ctx,
		cfg,
		http.MethodPost,
		cfg.apiURL+"/api/eutherpunk/workspace/jobs/"+url.PathEscape(id)+"/repair",
		raw,
	)
	if err != nil {
		return workspaceJobResponse{}, err
	}
	if status != http.StatusAccepted && status != http.StatusOK {
		return workspaceJobResponse{}, fmt.Errorf("HTTP %d: %s", status, job.Error)
	}
	return job, nil
}

func requestWorkspaceAnswerLegacy(
	ctx context.Context,
	cfg cliConfig,
	raw []byte,
) (string, fileProposal, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.apiURL+"/api/eutherpunk/chat", bytes.NewReader(raw))
	if err != nil {
		return "", fileProposal{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "eutherpunk-cli/"+version)
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	if err := cfg.authorize(req); err != nil {
		return "", fileProposal{}, err
	}
	resp, err := cliHTTPClient.Do(req)
	if err != nil {
		return "", fileProposal{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProposalBytes+128*1024))
	if err != nil {
		return "", fileProposal{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fileProposal{}, fmt.Errorf("%s: %s", resp.Status, string(body))
	}
	var response chatResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fileProposal{}, err
	}
	if response.Error != "" {
		return "", fileProposal{}, errors.New(response.Error)
	}
	return validateWorkspaceJobResult(workspaceJobResponse{
		Status:  "completed",
		Message: response.Message,
		Files:   response.Files,
	})
}

func handleWorkspaceCommand(state *workspaceState, command string) error {
	fields := strings.Fields(strings.TrimSpace(command))
	if len(fields) == 1 {
		if state.Root == "" {
			fmt.Println("Arbetsyta: INTE VALD")
			fmt.Println("Använd /workspace init <katalog> eller /workspace use <katalog>.")
			return nil
		}
		fmt.Println("Arbetsyta:", state.Root)
		return nil
	}
	if len(fields) < 3 || (strings.ToLower(fields[1]) != "use" && strings.ToLower(fields[1]) != "init") {
		return errors.New("använd /workspace init <katalog> eller /workspace use <katalog>")
	}
	rawPath := strings.TrimSpace(strings.Join(fields[2:], " "))
	path, err := filepath.Abs(rawPath)
	if err != nil {
		return err
	}
	if strings.ToLower(fields[1]) == "init" {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("katalogen finns redan; använd /workspace use")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(path))
		if err != nil {
			return fmt.Errorf("kontrollera föräldrakatalog: %w", err)
		}
		path = filepath.Join(parent, filepath.Base(path))
		if unsafeWorkspaceRoot(path) {
			return errors.New("arbetsytan är för bred; välj en särskild projektkatalog")
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return err
		}
	} else {
		path, err = filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("arbetsytan måste vara en katalog")
		}
		if unsafeWorkspaceRoot(path) {
			return errors.New("arbetsytan är för bred; välj en särskild projektkatalog")
		}
	}
	state.Root = filepath.Clean(path)
	fmt.Println("Arbetsyta:", state.Root)
	fmt.Println("EutherPunk kommer inte åt filer utanför denna katalog.")
	return nil
}

func unsafeWorkspaceRoot(path string) bool {
	path = filepath.Clean(path)
	home, _ := os.UserHomeDir()
	return path == filepath.Clean(string(filepath.Separator)) ||
		(home != "" && path == filepath.Clean(home))
}

func approvedWorkspaceContext(
	reader *bufio.Reader,
	workspace workspaceState,
	permissions *sessionPermissions,
) (string, bool, error) {
	if workspace.Root == "" {
		return "", false, nil
	}
	switch permissions.files {
	case permissionOff:
		return "", false, nil
	case permissionAsk:
		fmt.Println("EutherPunk vill läsa textfiler i arbetsytan:")
		fmt.Println(" ", workspace.Root)
		fmt.Printf("  Högst %d filer och %d KiB; symlänkar, .git och hemlighetsfiler hoppas över.\n",
			maxWorkspaceFiles, maxWorkspaceContextBytes/1024)
		fmt.Print("Tillåt? [y] en gång  [s] sessionen  [N] neka: ")
		answer, err := reader.ReadString('\n')
		if err != nil {
			return "", false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "s", "session":
			permissions.files = permissionSession
		case "y", "yes", "j", "ja":
		default:
			fmt.Println("Nekad.")
			return "", false, nil
		}
	case permissionSession:
	default:
		return "", false, fmt.Errorf("okänd filbehörighet: %s", permissions.files)
	}
	context, err := workspaceContext(workspace)
	return context, err == nil, err
}

func workspaceContext(workspace workspaceState) (string, error) {
	files, err := readWorkspaceFiles(workspace)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString("LOKAL KODARBETSYTA\n")
	out.WriteString("Du får läsa följande snapshot, men du har ingen direkt fil- eller shellåtkomst.\n")
	out.WriteString("Servern har en separat strukturerad filkanal. Lägg aldrig filinnehåll eller kodblock i chattsvaret.\n")
	out.WriteString("Använd endast relativa sökvägar. Ta aldrig med hemligheter. Ett lokalt godkännande krävs innan något skrivs.\n\n")
	out.WriteString("Arbetsyta: ")
	out.WriteString(filepath.Base(workspace.Root))
	out.WriteString("\n")
	if len(files) == 0 {
		out.WriteString("(tom arbetsyta)\n")
	}
	for _, file := range files {
		fmt.Fprintf(&out, "\n--- %s ---\n%s", filepath.ToSlash(file.Path), file.Content)
		if !strings.HasSuffix(file.Content, "\n") {
			out.WriteByte('\n')
		}
	}
	return out.String(), nil
}

func readWorkspaceFiles(workspace workspaceState) ([]workspaceFile, error) {
	root, err := canonicalWorkspaceRoot(workspace)
	if err != nil {
		return nil, err
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if shouldSkipWorkspaceDir(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() && !shouldSkipWorkspaceFile(relative) {
			paths = append(paths, relative)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	if len(paths) > maxWorkspaceFiles {
		paths = paths[:maxWorkspaceFiles]
	}
	files := make([]workspaceFile, 0, len(paths))
	total := 0
	for _, relative := range paths {
		path := filepath.Join(root, relative)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxWorkspaceFileBytes {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(raw, 0) >= 0 || !utf8.Valid(raw) || total+len(raw) > maxWorkspaceContextBytes {
			continue
		}
		total += len(raw)
		files = append(files, workspaceFile{Path: relative, Content: string(raw)})
	}
	return files, nil
}

func shouldSkipWorkspaceDir(name string) bool {
	switch strings.ToLower(name) {
	case ".git", ".hg", ".svn", "node_modules", "target", "dist", "build", ".cache":
		return true
	}
	return false
}

func shouldSkipWorkspaceFile(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if strings.HasPrefix(name, ".env") || strings.HasSuffix(name, ".key") ||
		strings.HasSuffix(name, ".pem") || strings.HasSuffix(name, ".p12") ||
		strings.HasSuffix(name, ".eutherpunk.previous") ||
		strings.Contains(name, "credential") || strings.Contains(name, "secret") {
		return true
	}
	return false
}

func parseFileProposal(answer string) (fileProposal, string, bool, error) {
	const startMarker = "```eutherpunk_files"
	start := strings.Index(answer, startMarker)
	if start < 0 {
		return fileProposal{}, answer, false, nil
	}
	jsonStart := start + len(startMarker)
	if jsonStart < len(answer) && answer[jsonStart] == '\r' {
		jsonStart++
	}
	if jsonStart < len(answer) && answer[jsonStart] == '\n' {
		jsonStart++
	}
	endOffset := strings.Index(answer[jsonStart:], "```")
	if endOffset < 0 {
		return fileProposal{}, answer, false, errors.New("filförslaget saknar avslutande kodstaket")
	}
	raw := strings.TrimSpace(answer[jsonStart : jsonStart+endOffset])
	if len(raw) > maxProposalBytes {
		return fileProposal{}, answer, false, errors.New("filförslaget är för stort")
	}
	var proposal fileProposal
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proposal); err != nil {
		return fileProposal{}, answer, false, fmt.Errorf("tolka filförslag: %w", err)
	}
	if len(proposal.Files) == 0 || len(proposal.Files) > maxProposalFiles {
		return fileProposal{}, answer, false, fmt.Errorf("filförslaget måste innehålla 1-%d filer", maxProposalFiles)
	}
	for _, file := range proposal.Files {
		if err := validateProposedFile(file); err != nil {
			return fileProposal{}, answer, false, err
		}
	}
	visible := strings.TrimSpace(answer[:start] + answer[jsonStart+endOffset+3:])
	return proposal, visible, true, nil
}

func validateProposedFile(file workspaceFile) error {
	path := filepath.Clean(filepath.FromSlash(strings.TrimSpace(file.Path)))
	if path == "." || filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return fmt.Errorf("otillåten filsökväg %q", file.Path)
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".git" || shouldSkipWorkspaceFile(part) {
			return fmt.Errorf("skyddad filsökväg %q", file.Path)
		}
	}
	if len(file.Content) > maxProposalFileBytes || !utf8.ValidString(file.Content) {
		return fmt.Errorf("filen %q är för stor eller inte giltig UTF-8", file.Path)
	}
	return nil
}

func approveAndApplyProposal(
	reader *bufio.Reader,
	workspace workspaceState,
	permissions *sessionPermissions,
	proposal fileProposal,
) (bool, error) {
	if permissions.files == permissionOff {
		fmt.Println("Filåtkomst är avstängd. Aktivera med /permissions files ask.")
		return false, nil
	}
	if err := validateProposalSyntax(proposal); err != nil {
		fmt.Println()
		fmt.Println("FILFÖRSLAGET NEKADES AUTOMATISKT")
		fmt.Println(err)
		fmt.Println("Inga filer har ändrats.")
		return false, nil
	}
	root, err := canonicalWorkspaceRoot(workspace)
	if err != nil {
		return false, err
	}
	fmt.Println()
	fmt.Println("FÖRESLAGNA FILÄNDRINGAR")
	for _, file := range proposal.Files {
		target, exists, err := safeWorkspaceTarget(root, file.Path)
		if err != nil {
			return false, err
		}
		action := "SKAPA"
		if exists {
			action = "ERSÄTT"
		}
		fmt.Printf("  %s  %s  (%d byte)\n", action, filepath.ToSlash(file.Path), len(file.Content))
		if target == "" {
			return false, errors.New("ogiltigt filmål")
		}
	}
	fmt.Println()
	fmt.Println("FÖRHANDSVISNING AV NYTT INNEHÅLL")
	for _, file := range proposal.Files {
		printFilePreview(file)
	}
	fmt.Println("Inga filer har ändrats ännu.")
	fmt.Print("Skriv filerna i arbetsytan? [y/N]: ")
	answer, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes", "j", "ja":
	default:
		fmt.Println("Filändringarna nekades.")
		return false, nil
	}
	for _, file := range proposal.Files {
		if err := backupWorkspaceFile(root, file.Path); err != nil {
			return false, err
		}
		if err := writeWorkspaceFile(root, file); err != nil {
			return false, err
		}
	}
	fmt.Printf("Skrev %d fil(er) i %s.\n", len(proposal.Files), root)
	return true, nil
}

func printFilePreview(file workspaceFile) {
	const maxPreviewLines = 120
	const maxPreviewBytes = 12 * 1024
	content := file.Content
	truncatedBytes := false
	if len(content) > maxPreviewBytes {
		content = content[:maxPreviewBytes]
		for !utf8.ValidString(content) {
			content = content[:len(content)-1]
		}
		truncatedBytes = true
	}
	lines := strings.Split(content, "\n")
	truncatedLines := len(lines) > maxPreviewLines
	if truncatedLines {
		lines = lines[:maxPreviewLines]
	}
	fmt.Printf("--- %s ---\n", filepath.ToSlash(file.Path))
	for _, line := range lines {
		fmt.Println("+", line)
	}
	if truncatedBytes || truncatedLines {
		fmt.Println("+ ... (förhandsvisningen är avkortad)")
	}
}

func backupWorkspaceFile(root, relative string) error {
	target, exists, err := safeWorkspaceTarget(root, relative)
	if err != nil || !exists {
		return err
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	backup := target + ".eutherpunk.previous"
	if info, err := os.Lstat(backup); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("osäker backupfil %q", backup)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(backup, raw, 0o600)
}

func canonicalWorkspaceRoot(workspace workspaceState) (string, error) {
	if workspace.Root == "" {
		return "", errors.New("ingen arbetsyta är vald")
	}
	root, err := filepath.EvalSymlinks(workspace.Root)
	if err != nil {
		return "", err
	}
	if unsafeWorkspaceRoot(root) {
		return "", errors.New("arbetsytan är för bred")
	}
	return filepath.Clean(root), nil
}

func safeWorkspaceTarget(root, relative string) (string, bool, error) {
	file := workspaceFile{Path: relative}
	if err := validateProposedFile(file); err != nil {
		return "", false, err
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	target := filepath.Join(root, clean)
	inside, err := filepath.Rel(root, target)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", false, errors.New("filsökvägen lämnar arbetsytan")
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return target, false, nil
		}
		if err != nil {
			return "", false, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("symlänkar tillåts inte i filsökvägen %q", relative)
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", false, fmt.Errorf("%q är inte en katalog", filepath.Join(parts[:i+1]...))
		}
		if i == len(parts)-1 {
			if !info.Mode().IsRegular() {
				return "", false, fmt.Errorf("%q är inte en vanlig fil", relative)
			}
			return target, true, nil
		}
	}
	return target, false, nil
}

func writeWorkspaceFile(root string, file workspaceFile) error {
	target, _, err := safeWorkspaceTarget(root, file.Path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(target)
	if err := mkdirWorkspaceParents(root, parent); err != nil {
		return err
	}
	temp, err := os.CreateTemp(parent, ".eutherpunk-*.new")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.WriteString(file.Content); err != nil {
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
	return replaceFile(tempPath, target)
}

func mkdirWorkspaceParents(root, parent string) error {
	relative, err := filepath.Rel(root, parent)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("föräldrakatalogen lämnar arbetsytan")
	}
	current := root
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("osäker föräldrakatalog %q", current)
		}
	}
	return nil
}
