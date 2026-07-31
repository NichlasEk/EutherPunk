package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

const kittyImageChunkBytes = 3072

func absoluteCLIImageAssetPath(cfg cliConfig, relative string) (string, error) {
	root, err := canonicalWorkspaceRoot(cfg.workspace)
	if err != nil {
		return "", err
	}
	target, exists, err := safeWorkspaceTarget(root, filepath.FromSlash(relative))
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("bildasseten saknas: %s", relative)
	}
	return target, nil
}

func previewCLIImageAsset(cfg cliConfig, path string, output io.Writer) error {
	mode := strings.ToLower(strings.TrimSpace(cfg.settings.Terminal.ImagePreview))
	if mode == "" {
		mode = "auto"
	}
	if mode == "off" || mode == "path" {
		return nil
	}
	file, ok := output.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil
	}

	width := cfg.settings.Terminal.ImageMaxWidth
	if width == 0 {
		width = 80
	}
	if columns, _, err := term.GetSize(int(file.Fd())); err == nil && columns > 2 && width > columns-2 {
		width = columns - 2
	}

	if mode == "kitty" || (mode == "auto" && supportsKittyImages()) {
		return writeKittyImage(output, path, width)
	}
	return writeBlockImage(output, path, width)
}

func supportsKittyImages() bool {
	if strings.TrimSpace(os.Getenv("TMUX")) != "" || strings.TrimSpace(os.Getenv("STY")) != "" {
		return false
	}
	termName := strings.ToLower(os.Getenv("TERM"))
	termProgram := strings.ToLower(os.Getenv("TERM_PROGRAM"))
	return strings.Contains(termName, "kitty") || strings.Contains(termProgram, "wezterm")
}

func writeKittyImage(output io.Writer, path string, columns int) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	columns, rows := imageCellSize(config.Width, config.Height, columns)
	encoded := base64.StdEncoding.EncodeToString(raw)
	for offset := 0; offset < len(encoded); offset += kittyImageChunkBytes {
		end := offset + kittyImageChunkBytes
		if end > len(encoded) {
			end = len(encoded)
		}
		more := 1
		if end == len(encoded) {
			more = 0
		}
		if offset == 0 {
			if _, err := fmt.Fprintf(output, "\x1b_Ga=T,f=100,q=2,c=%d,r=%d,m=%d;%s\x1b\\", columns, rows, more, encoded[offset:end]); err != nil {
				return err
			}
		} else if _, err := fmt.Fprintf(output, "\x1b_Gm=%d;%s\x1b\\", more, encoded[offset:end]); err != nil {
			return err
		}
	}
	_, err = io.WriteString(output, strings.Repeat("\n", rows))
	return err
}

func writeBlockImage(output io.Writer, path string, columns int) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	img, _, err := image.Decode(bufio.NewReader(file))
	if err != nil {
		return err
	}
	bounds := img.Bounds()
	columns, rows := imageCellSize(bounds.Dx(), bounds.Dy(), columns)
	pixelRows := rows * 2
	for y := 0; y < pixelRows; y += 2 {
		for x := 0; x < columns; x++ {
			top := sampledImageColor(img, x, y, columns, pixelRows)
			bottom := sampledImageColor(img, x, y+1, columns, pixelRows)
			if _, err := fmt.Fprintf(output, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀", top.R, top.G, top.B, bottom.R, bottom.G, bottom.B); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(output, "\x1b[0m\n"); err != nil {
			return err
		}
	}
	return nil
}

func imageCellSize(width, height, maxColumns int) (columns, rows int) {
	if width < 1 || height < 1 || maxColumns < 1 {
		return 1, 1
	}
	columns = maxColumns
	rows = (height*columns + width*2 - 1) / (width * 2)
	if rows < 1 {
		rows = 1
	}
	if rows > 40 {
		rows = 40
		columns = (width*rows*2 + height - 1) / height
		if columns < 1 {
			columns = 1
		}
	}
	return columns, rows
}

func sampledImageColor(img image.Image, x, y, width, height int) color.RGBA {
	bounds := img.Bounds()
	sourceX := bounds.Min.X + x*bounds.Dx()/width
	sourceY := bounds.Min.Y + y*bounds.Dy()/height
	sample := color.NRGBAModel.Convert(img.At(sourceX, sourceY)).(color.NRGBA)
	return color.RGBA{
		R: uint8(uint16(sample.R) * uint16(sample.A) / 0xff),
		G: uint8(uint16(sample.G) * uint16(sample.A) / 0xff),
		B: uint8(uint16(sample.B) * uint16(sample.A) / 0xff),
		A: 0xff,
	}
}
