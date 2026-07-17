//go:build windows

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	privateArtifactFileRenameInformationEx   = 65
	privateArtifactWindowsFullControl        = windows.ACCESS_MASK(windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff)
	privateArtifactWindowsUnsafeParentAccess = windows.ACCESS_MASK(
		windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | 0x40 |
			windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
			windows.GENERIC_WRITE | windows.GENERIC_ALL,
	)
)

type privateArtifactRenameInformationEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

type privateArtifactDispositionInformationEx struct {
	Flags uint32
}

func createPrivateArtifact(path string) (*privateArtifactPublication, error) {
	if err := validatePrivateArtifactWindowsPath(path); err != nil {
		return nil, err
	}
	targetName := filepath.Base(path)
	parentPath := filepath.Dir(path)
	if err := preparePrivateArtifactWindowsParent(parentPath); err != nil {
		return nil, err
	}
	parentPathUTF16, err := windows.UTF16PtrFromString(filepath.Clean(parentPath))
	if err != nil {
		return nil, err
	}
	parent, err := windows.CreateFile(
		parentPathUTF16,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var parentInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(parent, &parentInfo); err != nil {
		_ = windows.CloseHandle(parent)
		return nil, err
	}
	if parentInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(parent)
		return nil, errors.New("private artifact parent is not a directory")
	}
	if parentInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(parent)
		return nil, errors.New("private artifact parent is a reparse point")
	}
	if err := privateArtifactWindowsParentSafe(parent); err != nil {
		_ = windows.CloseHandle(parent)
		return nil, err
	}

	principals, err := privateArtifactWindowsPrincipals()
	if err != nil {
		_ = windows.CloseHandle(parent)
		return nil, err
	}
	sddl := "D:P"
	for _, principal := range principals {
		principalSID := principal.String()
		if principalSID == "" {
			_ = windows.CloseHandle(parent)
			return nil, errors.New("private artifact Windows principal SID is unavailable")
		}
		sddl += "(A;;FA;;;" + principalSID + ")"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		_ = windows.CloseHandle(parent)
		return nil, err
	}

	var file *os.File
	for attempt := 0; attempt < privateArtifactTempAttempts; attempt++ {
		tempName, err := privateArtifactTempName(targetName)
		if err != nil {
			_ = windows.CloseHandle(parent)
			return nil, err
		}
		file, err = createPrivateArtifactWindowsFile(parent, parentPath, tempName, descriptor)
		if privateArtifactWindowsNameCollision(err) {
			continue
		}
		if err != nil {
			_ = windows.CloseHandle(parent)
			return nil, err
		}

		publication := &privateArtifactPublication{file: file}
		publication.commit = func() error {
			if err := renamePrivateArtifactWindowsFile(windows.Handle(file.Fd()), parent, targetName); err != nil {
				return err
			}
			publication.committed = true
			return nil
		}
		publication.cleanup = func() {
			if !publication.committed {
				deletePrivateArtifactWindowsFile(windows.Handle(file.Fd()))
			}
			_ = file.Close()
			_ = windows.CloseHandle(parent)
		}
		return publication, nil
	}
	_ = windows.CloseHandle(parent)
	return nil, errors.New("private artifact temporary name attempts exhausted")
}

func validatePrivateArtifactWindowsPath(path string) error {
	if err := validatePrivateArtifactWindowsNamespace(path); err != nil {
		return err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	relative := strings.TrimPrefix(filepath.Clean(absolute), volume)
	components := strings.FieldsFunc(relative, func(character rune) bool {
		return character == '\\' || character == '/'
	})
	return validatePrivateArtifactWindowsComponents(components)
}

func preparePrivateArtifactWindowsParent(parentPath string) error {
	absolute, err := filepath.Abs(parentPath)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(absolute, current)
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		encoded, encodeErr := windows.UTF16PtrFromString(current)
		if encodeErr != nil {
			return encodeErr
		}
		attributes, attributeErr := windows.GetFileAttributes(encoded)
		if os.IsNotExist(attributeErr) {
			if mkdirErr := os.Mkdir(current, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return mkdirErr
			}
			attributes, attributeErr = windows.GetFileAttributes(encoded)
		}
		if attributeErr != nil {
			return attributeErr
		}
		if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fmt.Errorf("private artifact parent component %q is a reparse point", component)
		}
	}
	return nil
}

func privateArtifactWindowsParentSafe(parent windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(
		parent,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	return privateArtifactWindowsParentDescriptorSafe(descriptor)
}

func privateArtifactWindowsParentDescriptorSafe(descriptor *windows.SECURITY_DESCRIPTOR) error {
	if descriptor == nil {
		return errors.New("private artifact Windows parent security descriptor is unavailable")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return errors.New("private artifact Windows parent owner is unavailable")
	}
	principals, err := privateArtifactWindowsPrincipals()
	if err != nil {
		return err
	}
	if !privateArtifactWindowsPrincipalAllowed(owner, principals) {
		return errors.New("private artifact Windows parent owner is not trusted")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	acl, defaulted, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PRESENT == 0 || defaulted || acl == nil {
		return errors.New("private artifact Windows parent DACL is unavailable")
	}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace == nil {
			return errors.New("private artifact Windows parent ACL contains an invalid entry")
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return errors.New("private artifact Windows parent ACL contains an unsupported access entry")
		}
		if ace.Mask&privateArtifactWindowsUnsafeParentAccess == 0 {
			continue
		}
		entrySID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !privateArtifactWindowsPrincipalAllowed(entrySID, principals) {
			return errors.New("private artifact Windows parent is writable by an untrusted principal")
		}
	}
	return nil
}

func privateArtifactWindowsPrincipalAllowed(candidate *windows.SID, allowed []*windows.SID) bool {
	if candidate == nil {
		return false
	}
	for _, principal := range allowed {
		if principal != nil && principal.Equals(candidate) {
			return true
		}
	}
	return false
}

func createPrivateArtifactWindowsFile(parent windows.Handle, parentPath, tempName string, descriptor *windows.SECURITY_DESCRIPTOR) (*os.File, error) {
	objectName, err := windows.NewNTUnicodeString(tempName)
	if err != nil {
		return nil, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:             uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory:      parent,
		ObjectName:         objectName,
		Attributes:         windows.OBJ_CASE_INSENSITIVE,
		SecurityDescriptor: descriptor,
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE,
		attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_CREATE,
		// Write-through also covers metadata changes made through this handle,
		// including the final rename.
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_WRITE_THROUGH,
		0,
		0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), filepath.Join(parentPath, tempName))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("private artifact Windows handle conversion failed")
	}
	return file, nil
}

func securePrivateArtifactFile(file *os.File, _ os.FileMode) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("private artifact Windows handle is not a regular file")
	}
	principals, err := privateArtifactWindowsPrincipals()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetSecurityInfo(
		windows.Handle(file.Fd()),
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	if descriptor == nil {
		return errors.New("private artifact Windows security descriptor is unavailable")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	verifiedACL, defaulted, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if control&(windows.SE_DACL_PRESENT|windows.SE_DACL_PROTECTED) != windows.SE_DACL_PRESENT|windows.SE_DACL_PROTECTED ||
		defaulted || verifiedACL == nil || verifiedACL.AceCount != uint16(len(principals)) {
		return errors.New("private artifact Windows ACL verification failed")
	}

	matched := make([]bool, len(principals))
	for index := uint32(0); index < uint32(verifiedACL.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(verifiedACL, index, &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags != 0 || ace.Mask != privateArtifactWindowsFullControl {
			return errors.New("private artifact Windows ACL contains an unexpected access entry")
		}
		entrySID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		principalIndex := -1
		for candidate, principal := range principals {
			if !matched[candidate] && principal.Equals(entrySID) {
				principalIndex = candidate
				break
			}
		}
		if principalIndex < 0 {
			return errors.New("private artifact Windows ACL contains an unexpected principal")
		}
		matched[principalIndex] = true
	}
	return nil
}

func privateArtifactWindowsPrincipals() ([]*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return nil, err
	}
	administrators, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return nil, err
	}
	candidates := []*windows.SID{user.User.Sid, system, administrators}
	principals := make([]*windows.SID, 0, len(candidates))
	for _, candidate := range candidates {
		duplicate := false
		for _, principal := range principals {
			if principal.Equals(candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			principals = append(principals, candidate)
		}
	}
	return principals, nil
}

func renamePrivateArtifactWindowsFile(file, parent windows.Handle, targetName string) error {
	encodedName, err := windows.UTF16FromString(targetName)
	if err != nil {
		return err
	}
	encodedName = encodedName[:len(encodedName)-1]
	nameOffset := int(unsafe.Offsetof(privateArtifactRenameInformationEx{}.FileName))
	buffer := make([]byte, int(unsafe.Sizeof(privateArtifactRenameInformationEx{}))+len(encodedName)*2)
	information := (*privateArtifactRenameInformationEx)(unsafe.Pointer(&buffer[0]))
	information.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	information.RootDirectory = parent
	information.FileNameLength = uint32(len(encodedName) * 2)
	for index, value := range encodedName {
		binary.LittleEndian.PutUint16(buffer[nameOffset+index*2:], value)
	}
	return windows.NtSetInformationFile(
		file,
		&windows.IO_STATUS_BLOCK{},
		&buffer[0],
		uint32(len(buffer)),
		privateArtifactFileRenameInformationEx,
	)
}

func deletePrivateArtifactWindowsFile(file windows.Handle) {
	disposition := privateArtifactDispositionInformationEx{
		Flags: windows.FILE_DISPOSITION_DELETE |
			windows.FILE_DISPOSITION_POSIX_SEMANTICS |
			windows.FILE_DISPOSITION_ON_CLOSE,
	}
	if err := windows.NtSetInformationFile(
		file,
		&windows.IO_STATUS_BLOCK{},
		(*byte)(unsafe.Pointer(&disposition)),
		uint32(unsafe.Sizeof(disposition)),
		windows.FileDispositionInformationEx,
	); err == nil {
		return
	}
	deleteFile := byte(1)
	_ = windows.NtSetInformationFile(
		file,
		&windows.IO_STATUS_BLOCK{},
		&deleteFile,
		uint32(unsafe.Sizeof(deleteFile)),
		windows.FileDispositionInformation,
	)
}

func privateArtifactWindowsNameCollision(err error) bool {
	status, ok := err.(windows.NTStatus)
	return ok && status == windows.STATUS_OBJECT_NAME_COLLISION
}
