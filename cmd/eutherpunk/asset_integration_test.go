package main

import (
	"bufio"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchHTMLBackgroundAssetPreservesApplication(t *testing.T) {
	original := `<!doctype html>
<html><head><style>
body { background-color: #111; }
</style></head><body>
<script>const gameplayMustSurvive = true;</script>
</body></html>`
	path := "assets/ocean-background.png"
	patched, ok, err := patchHTMLBackgroundAsset(original, path)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	for _, expected := range []string{
		backgroundStyleStart,
		`background-image: url("assets/ocean-background.png") !important`,
		"const gameplayMustSurvive = true;",
		"body { background-color: #111; }",
	} {
		if !strings.Contains(patched, expected) {
			t.Fatalf("patched HTML misses %q:\n%s", expected, patched)
		}
	}

	replacement, ok, err := patchHTMLBackgroundAsset(
		patched,
		"assets/ocean-background-darker.png",
	)
	if err != nil || !ok {
		t.Fatalf("replacement ok=%v err=%v", ok, err)
	}
	if strings.Count(replacement, backgroundStyleStart) != 1 ||
		strings.Contains(replacement, `url("assets/ocean-background.png")`) ||
		!strings.Contains(replacement, `url("assets/ocean-background-darker.png")`) {
		t.Fatalf("background block was not replaced cleanly:\n%s", replacement)
	}
}

func TestExistingBackgroundAssetIntegratesWithoutModelRequest(t *testing.T) {
	root := t.TempDir()
	workspace := workspaceState{Root: root}
	if err := ensureProjectMemory(workspace, "test"); err != nil {
		t.Fatal(err)
	}
	const originalHTML = `<!doctype html>
<html><head><style>body { background: #111; }</style></head>
<body><script>const gameplayMustSurvive = true;</script></body></html>`
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(originalHTML), 0o600); err != nil {
		t.Fatal(err)
	}
	png, err := base64.StdEncoding.DecodeString(
		"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
	)
	if err != nil {
		t.Fatal(err)
	}
	assetPath := filepath.Join("assets", "ocean-background.png")
	if err := os.Mkdir(filepath.Join(root, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, assetPath), png, 0o600); err != nil {
		t.Fatal(err)
	}
	intent := workspaceAssetIntent{
		Role:        "background",
		LogicalName: "ocean-background",
		ImagePrompt: "ocean",
		Original:    "bild av havet som bakgrund",
	}
	if _, err := registerWorkspaceAsset(
		workspace,
		workspaceAssetRegistry{Version: assetRegistryVersion},
		intent,
		filepath.ToSlash(assetPath),
	); err != nil {
		t.Fatal(err)
	}

	originalClient := cliHTTPClient
	var networkCalls int
	cliHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		networkCalls++
		t.Fatal("existing asset integration must not call the image or model server")
		return nil, nil
	})}
	t.Cleanup(func() { cliHTTPClient = originalClient })

	cfg := cliConfig{workspace: workspace}
	request := "kan du prova att koppla in ocean bilden som bakgrund till index.html"
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
	if !handled || !prepared.Completed || prepared.CodePrompt != "" || networkCalls != 0 {
		t.Fatalf(
			"handled=%v prepared=%#v networkCalls=%d",
			handled,
			prepared,
			networkCalls,
		)
	}
	updated, err := os.ReadFile(filepath.Join(root, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), `url("assets/ocean-background.png")`) ||
		!strings.Contains(string(updated), "gameplayMustSurvive = true") {
		t.Fatalf("updated index.html = %s", updated)
	}
	backup, err := os.ReadFile(filepath.Join(root, "index.html.eutherpunk.previous"))
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != originalHTML {
		t.Fatalf("backup changed: %s", backup)
	}
	state, err := loadProjectMemoryState(filepath.Join(root, projectMemoryDirName))
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "asset_integrated" ||
		state.AssetPath != "assets/ocean-background.png" ||
		state.LastTask != request ||
		state.JobID != "" ||
		len(state.Files) != 2 {
		t.Fatalf("state = %#v", state)
	}
}
