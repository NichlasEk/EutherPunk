package main

import (
	"context"
	"encoding/json"
	"fmt"
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
		expectedModel := "coder-model"
		if workspaceRequestHasProperty(request, "accepted") {
			expectedModel = "reviewer-model"
		}
		if request.Model != expectedModel {
			t.Fatalf("request model = %q, want %q", request.Model, expectedModel)
		}
		if workspaceRequestHasProperty(request, "accepted") {
			_ = json.NewEncoder(w).Encode(ollamaChatResponse{
				Message: ollamaMessage{
					Role:    "assistant",
					Content: `{"accepted":true,"issues":[]}`,
				},
				Done: true,
			})
			return
		}
		if workspaceRequestHasProperty(request, "content") {
			_ = json.NewEncoder(w).Encode(ollamaChatResponse{
				Message: ollamaMessage{
					Role:    "assistant",
					Content: `{"content":"print('hej')\n"}`,
				},
				Done: true,
			})
			return
		}
		close(started)
		<-release
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{
				Role: "assistant",
				Content: `{"message":"klart","files":[` +
					`{"path":"main.lua","instruction":"Skriv ett komplett program."}]}`,
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
		reviewModel:    "reviewer-model",
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
			if len(job.Activities) < 7 ||
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

func TestVerifierDrivenWorkspaceJobSkipsModelSelfReview(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if workspaceRequestHasProperty(request, "accepted") {
			t.Fatal("verifier-driven job invoked model self-review")
		}
		content := `{"message":"ready","files":[{"path":"engine.js","instruction":"repair engine"}]}`
		if workspaceRequestHasProperty(request, "content") {
			content = `{"content":"export const answer = 42;\n"}`
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{Role: "assistant", Content: content},
			Done:    true,
		})
	}))
	defer ollama.Close()

	workspaceJobsMu.Lock()
	workspaceJobs = map[string]*workspaceJob{
		"verified-job": {
			ID:     "verified-job",
			Status: "running",
			Model:  "coder-model",
			user:   "local",
			task:   "repair engine",
		},
	}
	workspaceJobsMu.Unlock()
	t.Cleanup(func() {
		workspaceJobsMu.Lock()
		workspaceJobs = map[string]*workspaceJob{}
		workspaceJobsMu.Unlock()
	})

	runWorkspaceJob(
		context.Background(),
		serverConfig{ollamaURL: ollama.URL, model: "chat-model"},
		"verified-job",
		"coder-model",
		"system",
		[]ollamaMessage{{Role: "user", Content: "repair engine"}},
		true,
	)

	workspaceJobsMu.Lock()
	job := workspaceJobViewLocked(workspaceJobs["verified-job"])
	workspaceJobsMu.Unlock()
	if job.Status != "completed" || len(job.Files) != 1 || job.DraftRev != 1 {
		t.Fatalf("job = %#v", job)
	}
	found := false
	for _, activity := range job.Activities {
		if strings.Contains(activity.Message, "extern körbar verifierare") {
			found = true
		}
	}
	if !found {
		t.Fatalf("activities = %#v", job.Activities)
	}
}

func workspaceRequestHasProperty(request ollamaChatRequest, property string) bool {
	format, ok := request.Format.(map[string]any)
	if !ok {
		return false
	}
	properties, ok := format["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = properties[property]
	return ok
}

func TestWorkspaceJobAllowsExplicitlyDisabledLocalAuth(t *testing.T) {
	workspaceJobsMu.Lock()
	workspaceJobs = map[string]*workspaceJob{}
	workspaceJobsMu.Unlock()

	body := `{"messages":[{"role":"user","content":"skapa"}],"local_workspace":true}`
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/eutherpunk/workspace/jobs",
		strings.NewReader(body),
	)
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, authPrincipal{
		User:     "local",
		Scopes:   []string{"eutherpunk:*"},
		AuthMode: "disabled",
	}))
	rec := httptest.NewRecorder()

	handleWorkspaceJobStart(serverConfig{
		ollamaURL:      "http://127.0.0.1:1",
		model:          "chat-model",
		workspaceModel: "coder-model",
		settingsDir:    t.TempDir(),
		promptsPath:    t.TempDir() + "/prompts.toml",
	})(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("start status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkspaceRepairAllowsDisabledAuthOnlyFromLoopback(t *testing.T) {
	cfg := serverConfig{authRequired: false}
	principal := authPrincipal{AuthMode: "disabled"}
	for _, test := range []struct {
		remote string
		want   bool
	}{
		{remote: "127.0.0.1:42000", want: true},
		{remote: "[::1]:42000", want: true},
		{remote: "192.168.32.10:42000", want: false},
	} {
		req := httptest.NewRequest(http.MethodPost, "/repair", nil)
		req.RemoteAddr = test.remote
		if got := workspaceRepairAllowed(cfg, req, principal); got != test.want {
			t.Fatalf("remote %q allowed=%t, want %t", test.remote, got, test.want)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "/repair", nil)
	req.RemoteAddr = "127.0.0.1:42000"
	if workspaceRepairAllowed(serverConfig{authRequired: true}, req, principal) {
		t.Fatal("disabled principal bypassed required authentication")
	}
	if !workspaceRepairAllowed(
		serverConfig{authRequired: true},
		req,
		authPrincipal{AuthMode: "cli_token"},
	) {
		t.Fatal("CLI token was rejected")
	}
}

func TestWorkspaceRepairUsesSavedDraft(t *testing.T) {
	workspaceJobsMu.Lock()
	workspaceJobs = map[string]*workspaceJob{
		"draft-job": {
			ID:      "draft-job",
			Status:  "completed",
			Model:   "coder-model",
			Message: "review rejected",
			DraftFiles: []workspaceResponseFile{{
				Path:    "engine.js",
				Content: "export const broken = true;\n",
			}},
			DraftRev: 1,
			user:     "local",
			task:     "repair engine",
		},
	}
	workspaceJobsMu.Unlock()
	t.Cleanup(func() {
		workspaceJobsMu.Lock()
		workspaceJobs = map[string]*workspaceJob{}
		workspaceJobsMu.Unlock()
	})

	req := httptest.NewRequest(
		http.MethodPost,
		"/api/eutherpunk/workspace/jobs/draft-job/repair",
		strings.NewReader(`{"diagnostics":"engine.js failed"}`),
	)
	req.RemoteAddr = "127.0.0.1:42000"
	req.SetPathValue("id", "draft-job")
	req = req.WithContext(context.WithValue(req.Context(), authContextKey{}, authPrincipal{
		User:     "local",
		AuthMode: "disabled",
	}))
	rec := httptest.NewRecorder()
	handleWorkspaceJobRepair(serverConfig{
		ollamaURL:    "http://127.0.0.1:1",
		authRequired: false,
	})(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("repair status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestVerifiedRepairUsesDiagnosticAnalystAndKeepsFiles(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		content := `{"content":"export const answer = 42;\n"}`
		if workspaceRequestHasProperty(request, "issues") &&
			!workspaceRequestHasProperty(request, "accepted") {
			content = `{"issues":["app.js returns 0 where the verifier requires 42"]}`
		}
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{Role: "assistant", Content: content},
			Done:    true,
		})
	}))
	defer ollama.Close()

	workspaceJobsMu.Lock()
	workspaceJobs = map[string]*workspaceJob{
		"verified-repair": {
			ID:      "verified-repair",
			Status:  "running",
			Model:   "coder-model",
			Message: "repair",
			user:    "local",
			task:    "make answer 42",
		},
	}
	workspaceJobsMu.Unlock()
	t.Cleanup(func() {
		workspaceJobsMu.Lock()
		workspaceJobs = map[string]*workspaceJob{}
		workspaceJobsMu.Unlock()
	})

	runWorkspaceJobLocalRepair(
		context.Background(),
		serverConfig{
			ollamaURL:   ollama.URL,
			model:       "chat-model",
			reviewModel: "reviewer-model",
		},
		"verified-repair",
		"coder-model",
		"make answer 42",
		"repair",
		[]workspaceResponseFile{{
			Path:    "app.js",
			Content: "export const answer = 0;\n",
		}},
		"expected 42, got 0",
	)

	workspaceJobsMu.Lock()
	job := workspaceJobViewLocked(workspaceJobs["verified-repair"])
	workspaceJobsMu.Unlock()
	if job.Status != "completed" ||
		len(job.Files) != 1 ||
		!strings.Contains(job.Files[0].Content, "42") ||
		job.DraftRev != 1 {
		t.Fatalf("job = %#v", job)
	}
	activityText := ""
	for _, activity := range job.Activities {
		activityText += activity.Message + "\n"
	}
	if !strings.Contains(activityText, "Verifieringsdiagnos") ||
		!strings.Contains(activityText, "utan ett nytt modellomdöme") {
		t.Fatalf("activities = %s", activityText)
	}
}

func TestWorkspaceJobRepairsRejectedProposal(t *testing.T) {
	var generationCalls, reviewCalls int
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request ollamaChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if workspaceRequestHasProperty(request, "accepted") {
			reviewCalls++
			accepted := reviewCalls > 1
			issues := `["rotation fungerar inte för icke-kvadratiska matriser"]`
			if accepted {
				issues = "[]"
			}
			_ = json.NewEncoder(w).Encode(ollamaChatResponse{
				Message: ollamaMessage{
					Role: "assistant",
					Content: fmt.Sprintf(
						`{"accepted":%t,"issues":%s}`,
						accepted,
						issues,
					),
				},
				Done: true,
			})
			return
		}
		if !workspaceRequestHasProperty(request, "content") {
			response := fmt.Sprintf(
				`{"message":"varv %d","files":[{"path":"game.js","instruction":"Reparera rotationslogiken fullständigt."}]}`,
				generationCalls+1,
			)
			_ = json.NewEncoder(w).Encode(ollamaChatResponse{
				Message: ollamaMessage{Role: "assistant", Content: response},
				Done:    true,
			})
			return
		}
		generationCalls++
		content := "const rotation = 'trasig';\n"
		if generationCalls > 1 {
			content = "const rotation = 'fungerar';\n"
		}
		response := fmt.Sprintf(`{"content":%q}`, content)
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{
			Message: ollamaMessage{Role: "assistant", Content: response},
			Done:    true,
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
		settingsDir:    t.TempDir(),
		promptsPath:    t.TempDir() + "/prompts.toml",
	}
	body := `{"messages":[{"role":"user","content":"skapa ett spel"}],"local_workspace":true}`
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

	deadline := time.Now().Add(time.Second)
	for {
		workspaceJobsMu.Lock()
		job := workspaceJobViewLocked(workspaceJobs[startedJob.ID])
		workspaceJobsMu.Unlock()
		if job.Status == "completed" {
			if generationCalls != 2 || reviewCalls != 2 {
				t.Fatalf("generation=%d review=%d", generationCalls, reviewCalls)
			}
			if len(job.Files) != 1 || !strings.Contains(job.Files[0].Content, "fungerar") {
				t.Fatalf("repaired files = %#v", job.Files)
			}
			if job.DraftRev != 2 || len(job.DraftFiles) != 1 ||
				!strings.Contains(job.DraftFiles[0].Content, "fungerar") {
				t.Fatalf("draft revision=%d files=%#v", job.DraftRev, job.DraftFiles)
			}
			if len(job.Drafts) != 2 ||
				!strings.Contains(job.Drafts[0].Files[0].Content, "trasig") ||
				!strings.Contains(job.Drafts[1].Files[0].Content, "fungerar") {
				t.Fatalf("draft history = %#v", job.Drafts)
			}
			break
		}
		if job.Status == "failed" || time.Now().After(deadline) {
			t.Fatalf("repair job did not complete: %#v", job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
