package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
konkret funktionsfel. Undvik kosmetiskt tyckande. Svara endast med JSON enligt
schemat. "issues" ska vara korta, konkreta och möjliga att reparera.`
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
	if review.Accepted && len(review.Issues) > 0 {
		review.Accepted = false
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
	repairPrompt := fmt.Sprintf(
		`Uppgiften är:
%s

Här är ditt senaste kompletta filförslag:
%s

En oberoende körbarhetsgranskning hittade följande konkreta fel:
- %s

Reparera samtliga fel. Returnera återigen kompletta slutliga filer, inte en
diff, inte resonemang och inte tester som ersätter den begärda funktionen.`,
		strings.TrimSpace(task),
		proposalJSON,
		strings.Join(issues, "\n- "),
	)
	return askWorkspaceOllama(
		ctx,
		ollamaURL,
		model,
		"Du reparerar ett redan granskat kodförslag. Bevara fungerande delar men prioritera korrekt körbarhet.",
		[]ollamaMessage{{Role: "user", Content: repairPrompt}},
		progress,
	)
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
