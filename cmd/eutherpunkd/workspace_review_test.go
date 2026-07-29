package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkspaceReviewKeepsAcceptedBooleanAuthoritative(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role: "assistant",
				Content: `{
					"accepted": true,
					"issues": [
						"The requested replacement has been done correctly."
					]
				}`,
			},
			Done: true,
		})
	}))
	defer ollama.Close()

	review, err := reviewWorkspaceProposalOllama(
		context.Background(),
		ollama.URL,
		"test-model",
		"replace one with two",
		"done",
		[]workspaceResponseFile{{Path: "continuity.txt", Content: "checkpoint two"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !review.Accepted {
		t.Fatalf("accepted review was inverted: %#v", review)
	}
	if len(review.Issues) != 0 {
		t.Fatalf("positive issue text was retained: %#v", review.Issues)
	}
}

func TestWorkspaceRepairTargetsDiagnosedFileWithoutReplanning(t *testing.T) {
	var calls int
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if !workspaceRequestHasProperty(request, "content") {
			t.Fatalf("repair used a planner request: %#v", request.Format)
		}
		prompt := request.Messages[len(request.Messages)-1].Content
		if !strings.Contains(prompt, `REPARERA NU ENDAST FILEN "main.go"`) ||
			!strings.Contains(prompt, "undefined: run") {
			t.Fatalf("repair prompt = %q", prompt)
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role:    "assistant",
				Content: `{"content":"package main\nfunc run() {}\n"}`,
			},
			Done: true,
		})
	}))
	defer ollama.Close()

	message, files, err := repairWorkspaceProposalOllama(
		context.Background(),
		ollama.URL,
		"test-model",
		"fix the program",
		"candidate",
		[]workspaceResponseFile{
			{Path: "main.go", Content: "package main\nfunc main() { run() }\n"},
			{Path: "config.toml", Content: "enabled = true\n"},
		},
		[]string{"main.go: undefined: run"},
		func(string) {},
	)
	if err != nil {
		t.Fatal(err)
	}
	if message != "candidate" {
		t.Fatalf("message = %q", message)
	}
	if calls != 1 {
		t.Fatalf("repair calls = %d", calls)
	}
	if files[0].Content != "package main\nfunc run() {}\n" {
		t.Fatalf("main.go = %q", files[0].Content)
	}
	if files[1].Content != "enabled = true\n" {
		t.Fatalf("unaffected file changed: %q", files[1].Content)
	}
}
