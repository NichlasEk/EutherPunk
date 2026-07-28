//go:build windows

package main

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/windows/registry"
)

func windowsProductName() string {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows NT\CurrentVersion`,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return ""
	}
	defer key.Close()

	product, _, err := key.GetStringValue("ProductName")
	if err != nil {
		return ""
	}
	displayVersion, _, _ := key.GetStringValue("DisplayVersion")
	build, _, _ := key.GetStringValue("CurrentBuildNumber")

	if buildNumber, err := strconv.Atoi(strings.TrimSpace(build)); err == nil &&
		buildNumber >= 22000 &&
		strings.Contains(product, "Windows 10") {
		product = strings.Replace(product, "Windows 10", "Windows 11", 1)
	}

	parts := []string{strings.TrimSpace(product)}
	if displayVersion = strings.TrimSpace(displayVersion); displayVersion != "" {
		parts = append(parts, displayVersion)
	}
	result := strings.Join(parts, " ")
	if build = strings.TrimSpace(build); build != "" {
		result += fmt.Sprintf(" (build %s)", build)
	}
	return result
}
