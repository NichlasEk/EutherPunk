package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceJobReturnsImmediatelyAndCompletes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "coder-model" {
			t.Fatalf("workspace model = %q", request.Model)
		}
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role: "assistant",
				Content: `{"message":"klart","files":[` +
					`{"path":"main.lua","content":"print('hej')\n"}]}`,
			},
			Done: true,
		})
	}))
	defer ollama.Close()

	workspaceJobsMu.Lock()
	workspaceJobs = map[string]*workspaceJob{}
	workspaceJobsMu.Unlock()
	cfg := serverConfig{
		ollamaURL:      ollama.URL,
		model:          "chat-model",
		workspaceModel: "coder-model",
		visionModel:    "vision-model",
		settingsDir:    t.TempDir(),
		promptsPath:    t.TempDir() + "/prompts.toml",
	}
	body := `{"messages":[{"role":"user","content":"skapa"}],"local_workspace":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/eutherpunk/workspace/jobs", strings.NewReader(body))
	req.Header.Set("X-EutherPunk-Client-Mode", "chat-only")
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, authPrincipal{
		User:     "nichlas",
		Scopes:   []string{"eutherpunk:chat"},
		AuthMode: "cli_token",
	}))
	rec := httptest.NewRecorder()

	handleWorkspaceJobStart(cfg)(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}
	var startedJob workspaceJob
	if err := json.NewDecoder(rec.Body).Decode(&startedJob); err != nil {
		t.Fatal(err)
	}
	if startedJob.ID == "" || (startedJob.Status != "queued" && startedJob.Status != "running") {
		t.Fatalf("started job = %#v", startedJob)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("workspace model did not start")
	}

	getJob := func() workspaceJob {
		getReq := httptest.NewRequest(http.MethodGet, "/api/eutherpunk/workspace/jobs/"+startedJob.ID, nil)
		getReq.SetPathValue("id", startedJob.ID)
		getReq = getReq.WithContext(context.WithValue(getReq.Context(), authContextKey{}, authPrincipal{
			User:     "nichlas",
			Scopes:   []string{"eutherpunk:chat"},
			AuthMode: "cli_token",
		}))
		getRec := httptest.NewRecorder()
		handleWorkspaceJobGet()(getRec, getReq)
		if getRec.Code != http.StatusOK {
			t.Fatalf("get status = %d: %s", getRec.Code, getRec.Body.String())
		}
		var job workspaceJob
		if err := json.NewDecoder(getRec.Body).Decode(&job); err != nil {
			t.Fatal(err)
		}
		return job
	}
	if job := getJob(); job.Status != "running" {
		t.Fatalf("running job = %#v", job)
	}

	close(release)
	deadline := time.Now().Add(time.Second)
	for {
		job := getJob()
		if job.Status == "completed" {
			if job.Message != "klart" || len(job.Files) != 1 || job.Files[0].Path != "main.lua" {
				t.Fatalf("completed job = %#v", job)
			}
			if len(job.Activities) < 5 ||
				!strings.Contains(job.Activities[len(job.Activities)-1].Message, "1 fil") {
				t.Fatalf("completed activities = %#v", job.Activities)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete: %#v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
