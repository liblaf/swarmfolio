//go:build windows

package config

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// checkConfigPermissions accepts only a config owned by the current account
// whose discretionary ACL grants access only to that account, SYSTEM, and the
// local Administrators group. Windows file modes do not represent ACLs.
func checkConfigPermissions(file *os.File, _ os.FileInfo) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current Windows account: %w", err)
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("inspect access control for config %q: %w", file.Name(), err)
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return fmt.Errorf("inspect owner for config %q: %w", file.Name(), err)
	}
	if !owner.Equals(user.User.Sid) {
		return fmt.Errorf("config %q must be owned by the current Windows account", file.Name())
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("config %q must have a restrictive Windows access control list", file.Name())
	}
	allowed, err := windowsConfigPrincipals(user.User.Sid)
	if err != nil {
		return err
	}
	for index := uint16(0); index < dacl.AceCount; index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(index), &ace); err != nil {
			return fmt.Errorf("inspect access control entry for config %q: %w", file.Name(), err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			return fmt.Errorf("config %q has an unsupported Windows access control entry", file.Name())
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !containsSID(allowed, sid) {
			return fmt.Errorf("config %q grants access to another Windows account", file.Name())
		}
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

func containsSID(sids []*windows.SID, want *windows.SID) bool {
	for _, sid := range sids {
		if sid.Equals(want) {
			return true
		}
	}
	return false
}
