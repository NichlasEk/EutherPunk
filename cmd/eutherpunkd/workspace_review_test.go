package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
