//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
)

var (
	advapi32       = windows.NewLazySystemDLL("advapi32.dll")
	procCredRead   = advapi32.NewProc("CredReadW")
	procCredWrite  = advapi32.NewProc("CredWriteW")
	procCredDelete = advapi32.NewProc("CredDeleteW")
	procCredFree   = advapi32.NewProc("CredFree")
)

type windowsCredential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

func credentialTarget(apiURL string) string {
	return "EutherPunk CLI|" + apiURL
}

func loadCredentials(_ string, apiURL string) (authCredentials, error) {
	var credentials authCredentials
	target, err := windows.UTF16PtrFromString(credentialTarget(apiURL))
	if err != nil {
		return credentials, err
	}
	var stored *windowsCredential
	ok, _, callErr := procCredRead.Call(
		uintptr(unsafe.Pointer(target)),
		credTypeGeneric,
		0,
		uintptr(unsafe.Pointer(&stored)),
	)
	if ok == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return credentials, nil
		}
		return credentials, fmt.Errorf("Windows Credential Manager: %w", callErr)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(stored)))
	if stored.CredentialBlobSize == 0 {
		return credentials, nil
	}
	raw := append([]byte(nil), unsafe.Slice(stored.CredentialBlob, int(stored.CredentialBlobSize))...)
	return credentials, json.Unmarshal(raw, &credentials)
}

func saveCredentials(_ string, apiURL string, credentials authCredentials) error {
	raw, err := json.Marshal(credentials)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(credentialTarget(apiURL))
	if err != nil {
		return err
	}
	username, err := windows.UTF16PtrFromString(credentials.User)
	if err != nil {
		return err
	}
	entry := windowsCredential{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: uint32(len(raw)),
		CredentialBlob:     &raw[0],
		Persist:            credPersistLocalMachine,
		UserName:           username,
	}
	ok, _, callErr := procCredWrite.Call(uintptr(unsafe.Pointer(&entry)), 0)
	if ok == 0 {
		return fmt.Errorf("Windows Credential Manager: %w", callErr)
	}
	return nil
}

func deleteCredentials(_ string, apiURL string) error {
	target, err := windows.UTF16PtrFromString(credentialTarget(apiURL))
	if err != nil {
		return err
	}
	ok, _, callErr := procCredDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	if ok == 0 && !errors.Is(callErr, windows.ERROR_NOT_FOUND) {
		return fmt.Errorf("Windows Credential Manager: %w", callErr)
	}
	return nil
}
