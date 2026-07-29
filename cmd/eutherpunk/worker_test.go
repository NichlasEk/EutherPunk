package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkerProposalOnlyReturnsJSONWithoutWriting(t *testing.T) {
	root := t.TempDir()
	server := workerTestServer(t, func(request chatRequest) {
		if len(request.Messages) != 1 ||
			!strings.Contains(request.Messages[0].Content, "WORKER ROLE: implementer") ||
			!strings.Contains(request.Messages[0].Content, "Create worker.txt") {
			t.Fatalf("worker messages = %#v", request.Messages)
		}
	})
	defer server.Close()

	cfg := workerTestConfig(server.URL)
	var stdout, stderr bytes.Buffer
	err := runWorker(
		&cfg,
		[]string{
			"--workspace", root,
			"--task", "Create worker.txt",
		},
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatal(err)
	}
	var result workerResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("result JSON: %v\n%s", err, stdout.String())
	}
	if result.Status != "completed" || result.Applied {
		t.Fatalf("result = %#v", result)
	}
	if len(result.Files) != 1 ||
		result.Files[0].Path != "worker.txt" ||
		result.Files[0].Content != "worker output\n" ||
		result.Files[0].SHA256 == "" {
		t.Fatalf("files = %#v", result.Files)
	}
	if len(result.Drafts) != 1 ||
		result.Drafts[0].Revision != 1 ||
		len(result.Drafts[0].Files) != 1 {
		t.Fatalf("drafts = %#v", result.Drafts)
	}
	if _, err := os.Stat(filepath.Join(root, "worker.txt")); !os.IsNotExist(err) {
		t.Fatalf("proposal-only worker wrote a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".eutherpunk")); !os.IsNotExist(err) {
		t.Fatalf("proposal-only worker created project memory: %v", err)
	}
	if !strings.Contains(stderr.String(), "worker job-test started") {
		t.Fatalf("progress = %q", stderr.String())
	}
}

func TestWorkerVerifierDrivenSetsServerRequest(t *testing.T) {
	root := t.TempDir()
	server := workerTestServer(t, func(request chatRequest) {
		if !request.VerifierDriven {
			t.Fatal("worker did not request verifier-driven mode")
		}
	})
	defer server.Close()

	cfg := workerTestConfig(server.URL)
	var stdout bytes.Buffer
	if err := runWorker(
		&cfg,
		[]string{
			"--workspace", root,
			"--task", "Repair worker.txt",
			"--verifier-driven",
		},
		&stdout,
		&bytes.Buffer{},
	); err != nil {
		t.Fatal(err)
	}
	var result workerResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result = %#v", result)
	}
}

func TestWorkerApplyWritesCheckpointAndProjectMemory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "worker.txt")
	if err := os.WriteFile(target, []byte("original\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := workerTestServer(t, nil)
	defer server.Close()

	cfg := workerTestConfig(server.URL)
	var stdout, stderr bytes.Buffer
	if err := runWorker(
		&cfg,
		[]string{
			"--workspace", root,
			"--task", "Create worker.txt",
			"--apply",
		},
		&stdout,
		&stderr,
	); err != nil {
		t.Fatal(err)
	}
	var result workerResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || !result.Applied || result.CheckpointRevision != 1 {
		t.Fatalf("result = %#v", result)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "worker output\n" {
		t.Fatalf("content = %q", content)
	}
	backup, err := os.ReadFile(target + ".eutherpunk.previous")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != "original\n" {
		t.Fatalf("backup = %q", backup)
	}
	state, err := loadProjectMemoryState(filepath.Join(root, ".eutherpunk"))
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "accepted" ||
		state.LastTask != "Create worker.txt" ||
		state.CheckpointRevision != 1 {
		t.Fatalf("state = %#v", state)
	}
}

func TestWorkerCanWriteResultFile(t *testing.T) {
	root := t.TempDir()
	server := workerTestServer(t, nil)
	defer server.Close()
	resultPath := filepath.Join(t.TempDir(), "worker-result.json")

	cfg := workerTestConfig(server.URL)
	var stdout bytes.Buffer
	if err := runWorker(
		&cfg,
		[]string{
			"--workspace", root,
			"--task", "Create worker.txt",
			"--output", resultPath,
		},
		&stdout,
		&bytes.Buffer{},
	); err != nil {
		t.Fatal(err)
	}
	fromFile, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stdout.Bytes(), fromFile) {
		t.Fatal("stdout and --output result differ")
	}
}

func TestWorkerNeedsReviewReturnsLastDraft(t *testing.T) {
	root := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(workspaceJobResponse{
				ID:     "job-draft",
				Status: "running",
				Model:  "worker-model",
			})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(workspaceJobResponse{
				ID:       "job-draft",
				Status:   "completed",
				Model:    "worker-model",
				Message:  "The final review rejected the proposal.",
				DraftRev: 2,
				DraftFiles: []workspaceFile{{
					Path:    "draft.txt",
					Content: "inspect this draft\n",
				}},
				Activities: []workspaceJobActivity{{
					Sequence: 2,
					Message:  "Granskaren hittade: verify the edge case",
				}},
			})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	cfg := workerTestConfig(server.URL)
	var stdout bytes.Buffer
	if err := runWorker(
		&cfg,
		[]string{"--workspace", root, "--task", "Draft a change"},
		&stdout,
		&bytes.Buffer{},
	); err != nil {
		t.Fatal(err)
	}
	var result workerResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "needs_review" ||
		len(result.Files) != 1 ||
		result.Files[0].Content != "inspect this draft\n" ||
		len(result.Issues) != 1 {
		t.Fatalf("result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "draft.txt")); !os.IsNotExist(err) {
		t.Fatalf("proposal-only worker wrote rejected draft: %v", err)
	}
}

func TestWorkerRepairsBrokenRejectedDraftUsingLocalDiagnostics(t *testing.T) {
	root := t.TempDir()
	var repaired atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/repair"):
			var input struct {
				Diagnostics string `json:"diagnostics"`
			}
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(input.Diagnostics, "JavaScript") {
				t.Fatalf("repair diagnostics = %q", input.Diagnostics)
			}
			repaired.Store(true)
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(workspaceJobResponse{
				ID:     "job-repair-draft",
				Status: "running",
				Model:  "worker-model",
			})
		case request.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(workspaceJobResponse{
				ID:     "job-repair-draft",
				Status: "running",
				Model:  "worker-model",
			})
		case request.Method == http.MethodGet && repaired.Load():
			_ = json.NewEncoder(w).Encode(workspaceJobResponse{
				ID:      "job-repair-draft",
				Status:  "completed",
				Model:   "worker-model",
				Message: "repaired",
				Files: []workspaceFile{{
					Path:    "app.js",
					Content: "export const answer = 42;\n",
				}},
				DraftRev: 2,
				DraftFiles: []workspaceFile{{
					Path:    "app.js",
					Content: "export const answer = 42;\n",
				}},
			})
		case request.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(workspaceJobResponse{
				ID:       "job-repair-draft",
				Status:   "completed",
				Model:    "worker-model",
				Message:  "review rejected",
				DraftRev: 1,
				DraftFiles: []workspaceFile{{
					Path:    "app.js",
					Content: "const broken = ;\n",
				}},
			})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	cfg := workerTestConfig(server.URL)
	var stdout bytes.Buffer
	if err := runWorker(
		&cfg,
		[]string{"--workspace", root, "--task", "Repair app.js"},
		&stdout,
		&bytes.Buffer{},
	); err != nil {
		t.Fatal(err)
	}
	var result workerResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !repaired.Load() ||
		result.Status != "completed" ||
		len(result.Files) != 1 ||
		!strings.Contains(result.Files[0].Content, "42") {
		t.Fatalf("result = %#v, repaired=%t", result, repaired.Load())
	}
}

func TestWorkerRequiresExistingNonInteractiveLogin(t *testing.T) {
	root := t.TempDir()
	cfg := cliConfig{
		apiURL:     "http://127.0.0.1:1",
		configPath: filepath.Join(t.TempDir(), "config.toml"),
	}
	var stdout bytes.Buffer
	err := runWorker(
		&cfg,
		[]string{"--workspace", root, "--task", "Do a task"},
		&stdout,
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "auth login") {
		t.Fatalf("error = %v", err)
	}
	var result workerResult
	if jsonErr := json.Unmarshal(stdout.Bytes(), &result); jsonErr != nil {
		t.Fatalf("failure result JSON: %v\n%s", jsonErr, stdout.String())
	}
	if result.Status != "failed" || result.Error == "" {
		t.Fatalf("result = %#v", result)
	}
}

func TestParseWorkerOptionsRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	for _, args := range [][]string{
		{"--workspace", "/tmp"},
		{"--workspace", "/tmp", "--task", "one", "two"},
		{"--workspace", "/tmp", "--task", "one", "--role", "reviewer"},
		{"--workspace", "/tmp", "--task", "one", "--timeout", "31m"},
	} {
		if _, err := parseWorkerOptions(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("options unexpectedly accepted: %#v", args)
		}
	}
}

func workerTestConfig(apiURL string) cliConfig {
	return cliConfig{
		apiURL: apiURL,
		model:  "request-model",
		credentials: authCredentials{
			AccessToken: "test-token",
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		},
	}
}

func workerTestServer(t *testing.T, inspect func(chatRequest)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
		}
		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/api/eutherpunk/workspace/jobs":
			var input chatRequest
			if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
				t.Fatal(err)
			}
			if inspect != nil {
				inspect(input)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(workspaceJobResponse{
				ID:     "job-test-1",
				Status: "running",
				Model:  "worker-model",
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/api/eutherpunk/workspace/jobs/job-test-1":
			files := []workspaceFile{{
				Path:    "worker.txt",
				Content: "worker output\n",
			}}
			_ = json.NewEncoder(w).Encode(workspaceJobResponse{
				ID:         "job-test-1",
				Status:     "completed",
				Model:      "worker-model",
				Message:    "Worker task completed.",
				Files:      files,
				DraftFiles: files,
				DraftRev:   1,
				Drafts: []workspaceJobDraft{{
					Revision: 1,
					Files:    files,
				}},
				Activities: []workspaceJobActivity{{
					Sequence: 2,
					Message:  "Kvalitetsgranskning 1 godkände förslaget.",
				}},
			})
		default:
			http.NotFound(w, request)
		}
	}))
}
