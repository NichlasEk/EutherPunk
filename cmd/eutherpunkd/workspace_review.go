package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxWorkspaceRepairRounds = 2
	maxWorkspaceReviewBytes  = 96 * 1024
)

type workspaceQualityReview struct {
	Accepted bool     `json:"accepted"`
	Issues   []string `json:"issues"`
}

func reviewWorkspaceProposalOllama(
	ctx context.Context,
	ollamaURL, model, task, message string,
	files []workspaceResponseFile,
) (workspaceQualityReview, error) {
	proposalJSON, err := json.Marshal(struct {
		Message string                  `json:"message"`
		Files   []workspaceResponseFile `json:"files"`
	}{
		Message: message,
		Files:   files,
	})
	if err != nil {
		return workspaceQualityReview{}, err
	}
	if len(proposalJSON) > maxWorkspaceReviewBytes {
		return workspaceQualityReview{}, errors.New("filförslaget är för stort för kvalitetsgranskning")
	}

	system := `Du är en oberoende och skeptisk kodgranskare. Kontrollera om
filförslaget faktiskt löser användarens uppgift, inte bara om syntaxen ser
rimlig ut. Leta efter körfel, odefinierade identifierare, trasig tillståndsdata,
felaktiga algoritmer, funktioner som inte fungerar för alla giltiga former,
förlorad metadata, saknade kontroller och ofullständiga placeholders.
För spel ska du särskilt kontrollera spelloop, input, kollisioner, rotation av
alla former, låsning, poäng och game-over. Acceptera inte ett förslag med ett
konkret funktionsfel.

Granska endast mot uttryckliga krav i uppgiften och fel som kan beläggas direkt
i kandidatens kod. Hitta inte på krav på Unicode-normalisering, extra
separatorer, tomma indata, nya tester eller andra edge cases om uppgiften inte
kräver dem. Underkänn inte för att kandidaten saknar tester när användaren inte
bad om tester. Om en invändning beror på ett outtalat krav eller bara är en
möjlig förbättring ska förslaget accepteras.

Undvik kosmetiskt tyckande. Svara endast med JSON enligt schemat. Sätt
"accepted" till true och "issues" till en tom lista när de uttryckliga kraven är
uppfyllda. När "accepted" är false ska "issues" endast innehålla verkliga,
belagda fel, aldrig beröm eller en beskrivning av sådant som fungerar. Felen ska
vara korta, konkreta och möjliga att reparera.`
	user := fmt.Sprintf(
		"ANVÄNDARENS UPPGIFT:\n%s\n\nKANDIDAT:\n%s",
		strings.TrimSpace(task),
		proposalJSON,
	)
	format := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"accepted": map[string]any{"type": "boolean"},
			"issues": map[string]any{
				"type":     "array",
				"maxItems": 8,
				"items":    map[string]any{"type": "string"},
			},
		},
		"required": []string{"accepted", "issues"},
	}
	content, err := askWorkspaceStructured(
		ctx,
		ollamaURL,
		model,
		system,
		[]ollamaMessage{{Role: "user", Content: user}},
		format,
		1024,
	)
	if err != nil {
		return workspaceQualityReview{}, err
	}
	var review workspaceQualityReview
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&review); err != nil {
		return workspaceQualityReview{}, fmt.Errorf("tolka kvalitetsgranskning: %w", err)
	}
	review.Issues = compactReviewIssues(review.Issues)
	if review.Accepted {
		// The required boolean is authoritative. Some local models put a positive
		// explanation in issues even after accepting a correct proposal.
		review.Issues = nil
	}
	if !review.Accepted && len(review.Issues) == 0 {
		review.Issues = []string{"Granskaren underkände förslaget utan en användbar diagnos."}
	}
	return review, nil
}

func repairWorkspaceProposalOllama(
	ctx context.Context,
	ollamaURL, model, task, message string,
	files []workspaceResponseFile,
	issues []string,
	progress func(string),
) (string, []workspaceResponseFile, error) {
	proposalJSON, err := json.Marshal(struct {
		Message string                  `json:"message"`
		Files   []workspaceResponseFile `json:"files"`
	}{
		Message: message,
		Files:   files,
	})
	if err != nil {
		return "", nil, err
	}
	if len(proposalJSON) > maxWorkspaceReviewBytes {
		return "", nil, errors.New("filförslaget är för stort för reparationsvarv")
	}
	issueText := strings.ToLower(strings.Join(issues, "\n"))
	selected := make([]bool, len(files))
	selectedCount := 0
	for index, file := range files {
		path := strings.ToLower(strings.TrimSpace(file.Path))
		base := strings.ToLower(filepath.Base(path))
		if path != "" && (strings.Contains(issueText, path) ||
			(base != "" && strings.Contains(issueText, base))) {
			selected[index] = true
			selectedCount++
		}
	}
	if selectedCount == 0 {
		for index := range selected {
			selected[index] = true
		}
	}

	repaired := append([]workspaceResponseFile(nil), files...)
	for index, file := range files {
		if !selected[index] {
			continue
		}
		reportWorkspaceProgress(
			progress,
			fmt.Sprintf("Reparerar %s direkt utan en ny filplan.", file.Path),
		)
		content, err := repairWorkspaceFileOllama(
			ctx,
			ollamaURL,
			model,
			task,
			proposalJSON,
			file,
			issues,
		)
		if err != nil {
			return "", nil, fmt.Errorf("reparera %s: %w", file.Path, err)
		}
		repaired[index].Content = content
	}
	return message, repaired, nil
}

func repairWorkspaceFileOllama(
	ctx context.Context,
	ollamaURL, model, task string,
	proposalJSON []byte,
	file workspaceResponseFile,
	issues []string,
) (string, error) {
	repairPrompt := fmt.Sprintf(
		`UPPGIFT:
%s

HELA SENASTE FILFÖRSLAGET:
%s

VERIFIERADE FEL:
- %s

REPARERA NU ENDAST FILEN %q.

Returnera filens kompletta korrigerade innehåll. Verkställ ändringarna i koden;
upprepa inte bara diagnosen. Bevara fungerande API:n och beteenden. Returnera
inte en diff, Markdown eller resonemang.`,
		strings.TrimSpace(task),
		proposalJSON,
		strings.Join(issues, "\n- "),
		file.Path,
	)
	format := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties": map[string]any{
			"content": map[string]any{"type": "string"},
		},
		"required": []string{"content"},
	}
	content, err := askWorkspaceStructuredTimeout(
		ctx,
		ollamaURL,
		model,
		`Du är en precis filreparatör. Ett tidigare utkast och verifierade fel
finns i användarmeddelandet. Ändra den angivna filen så felen faktiskt
försvinner. Svara endast med JSON enligt schemat.`,
		[]ollamaMessage{{Role: "user", Content: repairPrompt}},
		format,
		6144,
		5*time.Minute,
	)
	if err != nil {
		return "", err
	}
	var generated struct {
		Content string `json:"content"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&generated); err != nil {
		return "", fmt.Errorf("tolka reparerad fil: %w", err)
	}
	if strings.TrimSpace(generated.Content) == "" {
		return "", errors.New("modellen returnerade en tom reparerad fil")
	}
	return generated.Content, nil
}

func askWorkspaceStructured(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
	format any,
	numPredict int,
) (string, error) {
	return askWorkspaceStructuredTimeout(
		ctx, ollamaURL, model, system, messages, format, numPredict, 2*time.Minute,
	)
}

func askWorkspaceStructuredTimeout(
	ctx context.Context,
	ollamaURL, model, system string,
	messages []ollamaMessage,
	format any,
	numPredict int,
	timeout time.Duration,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	think := false
	payload := ollamaChatRequest{
		Model:    model,
		Stream:   false,
		Messages: append([]ollamaMessage{{Role: "system", Content: system}}, messages...),
		Think:    &think,
		Format:   format,
		Options: map[string]any{
			"num_ctx":     12288,
			"num_predict": numPredict,
			"temperature": 0,
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(ollamaURL, "/")+"/api/chat",
		bytes.NewReader(raw),
	)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 128*1024))
	if err != nil {
		return "", err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("ollama returned %s: %s", response.Status, string(body))
	}
	var out ollamaChatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.Error != "" {
		return "", errors.New(out.Error)
	}
	if strings.TrimSpace(out.Message.Content) == "" {
		return "", errors.New("modellen gav en tom kvalitetsgranskning")
	}
	return out.Message.Content, nil
}

func compactReviewIssues(issues []string) []string {
	out := make([]string, 0, len(issues))
	for _, issue := range issues {
		issue = strings.TrimSpace(issue)
		if issue == "" {
			continue
		}
		if len(issue) > 500 {
			issue = issue[:500]
		}
		out = append(out, issue)
		if len(out) == 8 {
			break
		}
	}
	return out
}
