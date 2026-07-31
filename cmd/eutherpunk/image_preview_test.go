package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImageCellSizePreservesTerminalAspectRatio(t *testing.T) {
	if columns, rows := imageCellSize(800, 400, 80); columns != 80 || rows != 20 {
		t.Fatalf("imageCellSize = %dx%d, want 80x20", columns, rows)
	}
	if columns, rows := imageCellSize(400, 1200, 80); columns != 27 || rows != 40 {
		t.Fatalf("capped imageCellSize = %dx%d, want 27x40", columns, rows)
	}
}

func TestWriteBlockImage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview.png")
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	img.Set(1, 0, color.NRGBA{G: 255, A: 255})
	img.Set(0, 1, color.NRGBA{B: 255, A: 255})
	img.Set(1, 1, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, img); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := writeBlockImage(&output, path, 2); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	if !strings.Contains(got, "\x1b[38;2;255;0;0m") || !strings.Contains(got, "\x1b[48;2;0;0;255m") {
		t.Fatalf("preview did not contain expected colors: %q", got)
	}
	if strings.Count(got, "▀") != 2 {
		t.Fatalf("preview block count = %d, want 2", strings.Count(got, "▀"))
	}
}

func TestAbsoluteCLIImageAssetPath(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "assets")
	if err := os.Mkdir(assetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(assetDir, "result.png")
	if err := os.WriteFile(path, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := absoluteCLIImageAssetPath(cliConfig{workspace: workspaceState{Root: root}}, "assets/result.png")
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("absolute path = %q, want %q", got, path)
	}
}
