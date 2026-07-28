package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strconv"
	"strings"
)

type systemReport struct {
	OperatingSystem  string
	OSVersion        string
	Architecture     string
	Hostname         string
	Username         string
	WorkingDirectory string
	LogicalCPUs      int
	CLIVersion       string
}

func collectSystemReport() (systemReport, error) {
	report := systemReport{
		OperatingSystem: runtime.GOOS,
		OSVersion:       platformVersion(),
		Architecture:    runtime.GOARCH,
		LogicalCPUs:     runtime.NumCPU(),
		CLIVersion:      version,
	}

	if hostname, err := os.Hostname(); err == nil {
		report.Hostname = hostname
	}
	if currentUser, err := user.Current(); err == nil {
		report.Username = currentUser.Username
	}
	if workingDirectory, err := os.Getwd(); err == nil {
		report.WorkingDirectory = workingDirectory
	}
	return report, nil
}

func platformVersion() string {
	switch runtime.GOOS {
	case "windows":
		if product := windowsProductName(); product != "" {
			return product
		}
		return fixedCommandOutput("cmd.exe", "/d", "/c", "ver")
	case "darwin":
		return fixedCommandOutput("sw_vers", "-productVersion")
	case "linux":
		raw, err := os.ReadFile("/etc/os-release")
		if err != nil {
			return ""
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if value, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				return strings.Trim(strings.TrimSpace(value), `"`)
			}
		}
	}
	return ""
}

func fixedCommandOutput(name string, args ...string) string {
	output, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (report systemReport) String() string {
	lines := []string{
		"SYSTEMINFORMATION",
		"OS                  " + valueOrUnknown(report.OperatingSystem),
		"OS-version          " + valueOrUnknown(report.OSVersion),
		"Arkitektur          " + valueOrUnknown(report.Architecture),
		"Datornamn           " + valueOrUnknown(report.Hostname),
		"Användare           " + valueOrUnknown(report.Username),
		"Arbetskatalog       " + valueOrUnknown(report.WorkingDirectory),
		"Logiska CPU:er      " + strconv.Itoa(report.LogicalCPUs),
		"EutherPunk CLI      " + valueOrUnknown(report.CLIVersion),
	}
	return strings.Join(lines, "\n")
}

func (report systemReport) StringForShare(privacy privacySettings, full bool) string {
	shared := report
	if !full {
		if !privacy.ShareHostname {
			shared.Hostname = "(maskerat)"
		}
		if !privacy.ShareUsername {
			shared.Username = "(maskerat)"
		}
		if !privacy.ShareWorkingDirectory {
			shared.WorkingDirectory = "(maskerat)"
		}
	}
	return shared.String()
}

func (report systemReport) SharedPrompt(privacy privacySettings, full bool) string {
	return fmt.Sprintf(
		"Jag har uttryckligen valt att dela följande lokalt insamlade systeminformation med dig. "+
			"Använd den som kontext men föreslå inga ändringar utan att fråga mig först.\n\n%s",
		report.StringForShare(privacy, full),
	)
}

func valueOrUnknown(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "(okänt)"
}
