package main

import (
	"bufio"
	"fmt"
	"strings"
)

type permissionLevel string

const (
	permissionOff     permissionLevel = "off"
	permissionAsk     permissionLevel = "ask"
	permissionSession permissionLevel = "session"
)

type sessionPermissions struct {
	systemInfo permissionLevel
}

func defaultSessionPermissions() sessionPermissions {
	return sessionPermissions{systemInfo: permissionAsk}
}

func handlePermissionsCommand(permissions *sessionPermissions, command string) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(command)))
	switch {
	case len(fields) == 1:
		printPermissions(*permissions)
	case len(fields) == 2 && fields[1] == "reset":
		*permissions = defaultSessionPermissions()
		fmt.Println("Behörigheterna är återställda för den här sessionen.")
		printPermissions(*permissions)
	case len(fields) == 3 && (fields[1] == "system" || fields[1] == "system-info"):
		level, ok := parsePermissionLevel(fields[2])
		if !ok {
			printPermissionsHelp()
			return
		}
		permissions.systemInfo = level
		fmt.Printf("Systeminformation: %s\n", strings.ToUpper(string(level)))
	default:
		printPermissionsHelp()
	}
}

func parsePermissionLevel(value string) (permissionLevel, bool) {
	switch permissionLevel(strings.ToLower(strings.TrimSpace(value))) {
	case permissionOff:
		return permissionOff, true
	case permissionAsk:
		return permissionAsk, true
	case permissionSession:
		return permissionSession, true
	default:
		return "", false
	}
}

func printPermissions(permissions sessionPermissions) {
	fmt.Println("BEHÖRIGHETER")
	fmt.Printf("Systeminformation    %s\n", strings.ToUpper(string(permissions.systemInfo)))
	fmt.Println("Läsa filer           OFF (inte tillgängligt)")
	fmt.Println("Ändra filer          OFF (inte tillgängligt)")
	fmt.Println("Köra kommandon        OFF (inte tillgängligt)")
	fmt.Println("Administratör         NEVER")
	fmt.Println()
	fmt.Println("Behörigheterna gäller bara tills EutherPunk avslutas.")
}

func printPermissionsHelp() {
	fmt.Println("Användning:")
	fmt.Println("  /permissions")
	fmt.Println("  /permissions system off|ask|session")
	fmt.Println("  /permissions reset")
}

func approvedSystemReport(reader *bufio.Reader, permissions *sessionPermissions) (systemReport, bool, error) {
	switch permissions.systemInfo {
	case permissionOff:
		fmt.Println("Systeminformation är avstängd. Aktivera med /permissions system ask.")
		return systemReport{}, false, nil
	case permissionSession:
		report, err := collectSystemReport()
		return report, err == nil, err
	case permissionAsk:
		fmt.Println("EutherPunk vill läsa grundläggande systeminformation:")
		fmt.Println("  OS, version, arkitektur, datornamn, användarnamn, arbetskatalog och CPU-antal")
		fmt.Println("  Inga filer, IP-adresser, serienummer eller maskin-ID:n läses.")
		fmt.Print("Tillåt? [y] en gång  [s] sessionen  [N] neka: ")
		answer, err := reader.ReadString('\n')
		if err != nil {
			return systemReport{}, false, err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes", "j", "ja":
			report, err := collectSystemReport()
			return report, err == nil, err
		case "s", "session":
			permissions.systemInfo = permissionSession
			report, err := collectSystemReport()
			return report, err == nil, err
		default:
			fmt.Println("Nekad.")
			return systemReport{}, false, nil
		}
	default:
		return systemReport{}, false, fmt.Errorf("okänd behörighetsnivå: %s", permissions.systemInfo)
	}
}
