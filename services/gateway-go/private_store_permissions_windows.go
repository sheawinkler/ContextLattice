//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func enforceOwnerOnlyPermissions(path string, _ os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("owner-only Windows ACL target is a symlink")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return err
	}
	administrators, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return err
	}
	inheritance := uint32(windows.NO_INHERITANCE)
	if info.IsDir() {
		inheritance = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entries := []windows.EXPLICIT_ACCESS{
		ownerOnlyWindowsAccess(user.User.Sid, windows.TRUSTEE_IS_USER, inheritance),
		ownerOnlyWindowsAccess(system, windows.TRUSTEE_IS_USER, inheritance),
		ownerOnlyWindowsAccess(administrators, windows.TRUSTEE_IS_GROUP, inheritance),
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		acl,
		nil,
	); err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	verifiedACL, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 || verifiedACL == nil || verifiedACL.AceCount != uint16(len(entries)) {
		return errors.New("owner-only Windows ACL verification failed")
	}
	return nil
}

func ownerOnlyWindowsAccess(sid *windows.SID, trusteeType windows.TRUSTEE_TYPE, inheritance uint32) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.ACCESS_MASK(windows.GENERIC_ALL),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inheritance,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  trusteeType,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}
