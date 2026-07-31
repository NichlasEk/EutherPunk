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

func TestDetectNaturalOceanBackgroundAsset(t *testing.T) {
	request := "Fint nu skulle jag vilja ha havet som bakgrund med riktig bildgenererad asset?"
	intent, ok := detectWorkspaceAssetIntent(
		request,
		workspaceAssetRegistry{Version: assetRegistryVersion},
	)
	if !ok {
		t.Fatal("natural image asset request was not detected")
	}
	if intent.Role != "background" || intent.LogicalName != "ocean-background" {
		t.Fatalf("intent = %#v", intent)
	}
	if !strings.Contains(intent.ImagePrompt, request) {
		t.Fatalf("image prompt does not preserve original request: %q", intent.ImagePrompt)
	}
}

func TestDetectObservedNaturalImagePhrases(t *testing.T) {
	for _, test := range []struct {
		request string
		role    string
	}{
		{
			request: "du utan att bygga helt nytt kan du prova att göra en bild av havet och lägga in som bakgrund på tetris html",
			role:    "background",
		},
		{
			request: "gör en bild av havet och lägg in som assett",
			role:    "asset",
		},
	} {
		intent, ok := detectWorkspaceAssetIntent(
			test.request,
			workspaceAssetRegistry{Version: assetRegistryVersion},
		)
		if !ok || intent.Role != test.role || intent.LogicalName != "ocean-"+test.role {
			t.Fatalf("request %q produced intent %#v, ok=%v", test.request, intent, ok)
		}
	}
}

func TestDetectStandaloneImageRequestInsideCodingWorkspace(t *testing.T) {
	request := "du kan du prova att göra en bild på en glad katt i hatt och visa här?"
	intent, ok := detectWorkspaceAssetIntent(
		request,
		workspaceAssetRegistry{Version: assetRegistryVersion},
	)
	if !ok {
		t.Fatal("observed standalone image request was routed to the coding worker")
	}
	if intent.Role != "asset" || intent.LogicalName != "generated-asset" {
		t.Fatalf("intent = %#v", intent)
	}
	if !strings.Contains(intent.ImagePrompt, request) {
		t.Fatalf("image prompt does not preserve original request: %q", intent.ImagePrompt)
	}
}

func TestImageResizeCodingRequestDoesNotGenerateImage(t *testing.T) {
	if intent, ok := detectWorkspaceAssetIntent(
		"Gör en bild större med CSS och visa den i dialogen",
		workspaceAssetRegistry{Version: assetRegistryVersion},
	); ok {
		t.Fatalf("image-related coding request became generation intent: %#v", intent)
	}
}

func TestAssetFollowupReusesLogicalAsset(t *testing.T) {
	registry := workspaceAssetRegistry{
		Version: assetRegistryVersion,
		Assets: []workspaceAssetRecord{{
			ID:          "asset-old",
			LogicalName: "ocean-background",
			Role:        "background",
			Path:        "assets/ocean-background-old.png",
			Prompt:      "A blue ocean",
			Request:     "Använd havet som bakgrund",
			Status:      "active",
		}},
	}
	intent, ok := detectWorkspaceAssetIntent("Bra, men gör havet lite mörkare", registry)
	if !ok {
		t.Fatal("natural follow-up was not detected")
	}
	if intent.Role != "background" || intent.LogicalName != "ocean-background" {
		t.Fatalf("intent = %#v", intent)
	}
	if intent.PreviousAsset == nil || intent.PreviousAsset.ID != "asset-old" {
		t.Fatalf("previous asset = %#v", intent.PreviousAsset)
	}
}

func TestPlainCSSBackgroundChangeDoesNotGenerateImage(t *testing.T) {
	if intent, ok := detectWorkspaceAssetIntent(
		"Gör bakgrunden blå med CSS",
		workspaceAssetRegistry{Version: assetRegistryVersion},
	); ok {
		t.Fatalf("plain code styling request became image intent: %#v", intent)
	}
}

func TestPrepareNaturalWorkspaceAssetGeneratesRegistersAndHandsOff(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("ocean-image")...)
	originalClient := cliHTTPClient
	originalInterval := cliImagePollInterval
	cliImagePollInterval = time.Millisecond
	cliHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/api/eutherpunk/images/generate":
			var request cliImageRequest
			if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(request.Prompt, "havet som bakgrund") {
				t.Fatalf("image prompt = %q", request.Prompt)
			}
			return testHTTPResponse(http.StatusAccepted, `{"job_id":"ocean-1","status":"queued"}`), nil
		case req.Method == http.MethodGet && req.URL.Path == "/api/eutherpunk/images/jobs/ocean-1":
			return testHTTPResponse(http.StatusOK, `{"job_id":"ocean-1","status":"done","image":{"filename":"server-name.png","url":"/api/eutherpunk/images/nichlas/ocean.png"}}`), nil
		case req.Method == http.MethodGet && req.URL.Path == "/api/eutherpunk/images/nichlas/ocean.png":
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
	request := "Fint nu skulle jag vilja ha havet som bakgrund med riktig bildgenererad asset"
	prepared, handled, err := prepareNaturalWorkspaceAsset(
		cfg,
		bufio.NewReader(strings.NewReader("")),
		&sessionPermissions{files: permissionAuto},
		request,
		[]chatMessage{{Role: "user", Content: request}},
		io.Discard,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !handled || prepared.Path == "" {
		t.Fatalf("handled=%v prepared=%#v", handled, prepared)
	}
	if !strings.HasPrefix(prepared.Path, "assets/ocean-background-") ||
		!strings.HasSuffix(prepared.Path, ".png") {
		t.Fatalf("asset path = %q", prepared.Path)
	}
	registry, err := loadWorkspaceAssetRegistry(cfg.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Assets) != 1 ||
		registry.Assets[0].Status != "active" ||
		registry.Assets[0].Path != prepared.Path ||
		registry.Assets[0].SHA256 == "" {
		t.Fatalf("registry = %#v", registry)
	}
	if !strings.Contains(prepared.CodePrompt, "`"+prepared.Path+"`") ||
		!strings.Contains(prepared.CodePrompt, "HARNESS-VERIFIED IMMUTABLE ASSET") ||
		!strings.Contains(prepared.CodePrompt, registry.Assets[0].SHA256) ||
		!strings.Contains(prepared.CodePrompt, "Preserve all unrelated working behavior") {
		t.Fatalf("code handoff = %q", prepared.CodePrompt)
	}
	got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(prepared.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, png) {
		t.Fatalf("saved asset = %q", got)
	}
	context, err := projectMemoryContext(cfg.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(context, ".eutherpunk/assets.json") ||
		!strings.Contains(context, prepared.Path) ||
		!strings.Contains(context, `"asset_status": "asset_ready"`) {
		t.Fatalf("project memory context = %q", context)
	}
}
