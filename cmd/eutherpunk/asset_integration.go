package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	backgroundStyleStart = "/* eutherpunk:generated-background:start */"
	backgroundStyleEnd   = "/* eutherpunk:generated-background:end */"
)

func verifyRegisteredWorkspaceAsset(
	workspace workspaceState,
	record workspaceAssetRecord,
) error {
	if !strings.EqualFold(filepath.Ext(record.Path), ".png") {
		return fmt.Errorf("den registrerade asseten är inte en PNG: %s", record.Path)
	}
	root, err := canonicalWorkspaceRoot(workspace)
	if err != nil {
		return err
	}
	target, exists, err := safeWorkspaceTarget(root, record.Path)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("den registrerade asseten saknas: %s", record.Path)
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if len(raw) < 8 || !bytes.Equal(raw[:8], []byte("\x89PNG\r\n\x1a\n")) {
		return fmt.Errorf("den registrerade asseten är inte en giltig PNG: %s", record.Path)
	}
	sum := sha256.Sum256(raw)
	if record.SHA256 != "" && !strings.EqualFold(record.SHA256, hex.EncodeToString(sum[:])) {
		return fmt.Errorf("den registrerade assetens checksumma stämmer inte: %s", record.Path)
	}
	return nil
}

func integrateWorkspaceAssetDeterministically(
	workspace workspaceState,
	reader *bufio.Reader,
	permissions *sessionPermissions,
	record workspaceAssetRecord,
) (supported, applied bool, err error) {
	if record.Role != "background" {
		return false, false, nil
	}
	if err := verifyRegisteredWorkspaceAsset(workspace, record); err != nil {
		return true, false, err
	}
	root, err := canonicalWorkspaceRoot(workspace)
	if err != nil {
		return true, false, err
	}
	const htmlPath = "index.html"
	target, exists, err := safeWorkspaceTarget(root, htmlPath)
	if err != nil {
		return true, false, err
	}
	if !exists {
		return false, false, nil
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		return true, false, err
	}
	if len(raw) > maxProposalFileBytes || !utf8.Valid(raw) {
		return false, false, nil
	}
	updated, ok, err := patchHTMLBackgroundAsset(string(raw), record.Path)
	if err != nil {
		return true, false, err
	}
	if !ok {
		return false, false, nil
	}
	if updated == string(raw) {
		return true, true, nil
	}
	applied, err = approveAndApplyProposal(
		reader,
		workspace,
		permissions,
		fileProposal{Files: []workspaceFile{{
			Path:    htmlPath,
			Content: updated,
		}}},
	)
	return true, applied, err
}

func patchHTMLBackgroundAsset(html, assetPath string) (string, bool, error) {
	assetPath = filepath.ToSlash(strings.TrimSpace(assetPath))
	if assetPath == "" ||
		strings.ContainsAny(assetPath, "\"'\r\n()\\") ||
		!strings.EqualFold(filepath.Ext(assetPath), ".png") {
		return "", false, errors.New("osäker sökväg för bakgrundsasset")
	}
	block := fmt.Sprintf(`%s
        body {
            background-image: url("%s") !important;
            background-size: cover !important;
            background-position: center center !important;
            background-repeat: no-repeat !important;
            background-attachment: fixed !important;
        }
        %s`, backgroundStyleStart, assetPath, backgroundStyleEnd)

	if start := strings.Index(html, backgroundStyleStart); start >= 0 {
		endOffset := strings.Index(html[start:], backgroundStyleEnd)
		if endOffset < 0 {
			return "", false, errors.New("befintligt EutherPunk-bakgrundsblock är ofullständigt")
		}
		end := start + endOffset + len(backgroundStyleEnd)
		return html[:start] + block + html[end:], true, nil
	}

	lower := strings.ToLower(html)
	if closeStyle := strings.LastIndex(lower, "</style>"); closeStyle >= 0 {
		return html[:closeStyle] + "        " + block + "\n    " + html[closeStyle:], true, nil
	}
	if closeHead := strings.LastIndex(lower, "</head>"); closeHead >= 0 {
		style := "    <style>\n        " + block + "\n    </style>\n"
		return html[:closeHead] + style + html[closeHead:], true, nil
	}
	return "", false, nil
}
