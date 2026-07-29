package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImageDirectiveWriterHidesToolLine(t *testing.T) {
	var output bytes.Buffer
	writer := newImageDirectiveWriter(&output)
	for _, part := range []string{
		"Jag ordnar det.\nEUTHERPUNK_IM",
		"AGE_PROMPT: shadowy blocks in space",
	} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "Jag ordnar det.\n" {
		t.Fatalf("visible output = %q", got)
	}
}

func TestExtractImageToolDirective(t *testing.T) {
	visible, prompt, ok := extractImageToolDirective(
		"Jag gör bilden nu.\nEutherPunk_IMAGE_PROMPT: floating green blocks\n",
	)
	if !ok || visible != "Jag gör bilden nu." || prompt != "floating green blocks" {
		t.Fatalf("visible=%q prompt=%q ok=%v", visible, prompt, ok)
	}
}

func TestWorkspaceJobStatusLabel(t *testing.T) {
	if got := workspaceJobStatusLabel("completed"); !strings.Contains(got, "/job open") {
		t.Fatalf("completed label = %q", got)
	}
}

func TestGenerateAndSaveCLIImageAsset(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("test-image")...)
	var calls int
	originalClient := cliHTTPClient
	originalInterval := cliImagePollInterval
	cliImagePollInterval = time.Millisecond
	cliHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q", got)
		}
		calls++
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/api/eutherpunk/images/generate":
			var request cliImageRequest
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.Prompt != "floating blocks" || len(request.Context) != 1 {
				t.Fatalf("request = %#v", request)
			}
			return testHTTPResponse(http.StatusAccepted, `{"job_id":"image-1","status":"queued"}`), nil
		case req.Method == http.MethodGet && req.URL.Path == "/api/eutherpunk/images/jobs/image-1":
			return testHTTPResponse(http.StatusOK, `{"job_id":"image-1","status":"done","image":{"filename":"result.png","url":"/api/eutherpunk/images/nichlas/result.png"}}`), nil
		case req.Method == http.MethodGet && req.URL.Path == "/api/eutherpunk/images/nichlas/result.png":
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Body:       io.NopCloser(bytes.NewReader(png)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		default:
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			return nil, nil
		}
	})}
	t.Cleanup(func() {
		cliHTTPClient = originalClient
		cliImagePollInterval = originalInterval
	})

	root := t.TempDir()
	cfg := cliConfig{
		apiURL:    "https://example.invalid",
		workspace: workspaceState{Root: root},
		credentials: authCredentials{
			AccessToken: "test-token",
			ExpiresAt:   time.Now().Add(time.Hour).Unix(),
		},
	}
	result, err := runCLIImageAsset(
		cfg,
		bufio.NewReader(strings.NewReader("")),
		&sessionPermissions{files: permissionAuto},
		"floating blocks",
		[]chatMessage{{Role: "user", Content: "gör en bild"}},
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result != "Bildasset sparad i arbetsytan: assets/result.png" || calls != 3 {
		t.Fatalf("result=%q calls=%d", result, calls)
	}
	got, err := os.ReadFile(filepath.Join(root, "assets", "result.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, png) {
		t.Fatalf("saved bytes = %q", got)
	}
}

func TestAuthorizedCLIURLRejectsAnotherHost(t *testing.T) {
	if _, err := authorizedCLIURL("https://example.invalid", "https://evil.invalid/image.png"); err == nil {
		t.Fatal("cross-origin image URL was accepted")
	}
}

func testHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}
