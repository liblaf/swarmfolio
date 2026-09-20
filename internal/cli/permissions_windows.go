//go:build windows

package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func setFilePermissions(file *os.File, mode os.FileMode) error {
	if mode.Perm()&0o077 != 0 {
		return nil
	}
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current Windows account: %w", err)
	}
	principals, err := windowsConfigPrincipals(user.User.Sid)
	if err != nil {
		return err
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(principals))
	for _, principal := range principals {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.SET_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(principal),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build config access control list: %w", err)
	}
	if err := windows.SetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user.User.Sid, nil, acl, nil); err != nil {
		return fmt.Errorf("set access control for %q: %w", file.Name(), err)
	}
	return nil
}

func windowsConfigPrincipals(user *windows.SID) ([]*windows.SID, error) {
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return nil, fmt.Errorf("resolve Windows SYSTEM account: %w", err)
	}
	administrators, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return nil, fmt.Errorf("resolve Windows Administrators group: %w", err)
	}
	return []*windows.SID{user, system, administrators}, nil
}
