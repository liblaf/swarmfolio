//go:build windows

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestLoadAcceptsCurrentUserACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, privateConfig(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := protectWindowsConfig(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load(private config) = %v", err)
	}
}

func TestLoadRejectsEveryoneReadACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, privateConfig(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := protectWindowsConfig(path); err != nil {
		t.Fatal(err)
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	principals, err := windowsConfigPrincipals(user.User.Sid)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(principals)+1)
	for _, principal := range principals {
		entries = append(entries, windows.EXPLICIT_ACCESS{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(principal)}})
	}
	everyone, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_READ,
		AccessMode:        windows.SET_ACCESS,
		Trustee:           windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(everyone)},
	})
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "grants access") {
		t.Fatalf("Load(insecure config) error = %v", err)
	}
}

func privateConfig() []byte {
	data := strings.Replace(Example, `api-key = ""`, `api-key = "mteam-secret"`, 1)
	return []byte(data)
}

func protectWindowsConfig(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	principals, err := windowsConfigPrincipals(user.User.Sid)
	if err != nil {
		return err
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(principals))
	for _, principal := range principals {
		entries = append(entries, windows.EXPLICIT_ACCESS{AccessPermissions: windows.GENERIC_ALL, AccessMode: windows.SET_ACCESS, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_USER, TrusteeValue: windows.TrusteeValueFromSID(principal)}})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, user.User.Sid, nil, acl, nil)
}
