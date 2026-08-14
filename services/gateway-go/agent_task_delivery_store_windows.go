//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	agentTaskWindowsFileCreated             = 2
	agentTaskWindowsFileRenameInformationEx = 65
	agentTaskWindowsFullControl             = windows.ACCESS_MASK(windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff)
)

type agentTaskFileStat struct {
	raw    windows.ByHandleFileInformation
	basic  agentTaskWindowsBasicInfo
	Size   int64
	Device uint64
	FileID uint64
}

type agentTaskWindowsBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
}

type agentTaskWindowsRenameInformationEx struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

type agentTaskWindowsLinkInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

type agentTaskWindowsDispositionInformationEx struct {
	Flags uint32
}

type agentTaskWindowsRootTraversalHook func(stage string, componentIndex int, component string) error

const (
	agentTaskWindowsTraversalBeforeComponent = "before_component"
	agentTaskWindowsTraversalAfterComponent  = "after_component"
)

func agentTaskWindowsNormalizeError(err error) error {
	if err == nil {
		return nil
	}
	if status, ok := err.(windows.NTStatus); ok {
		switch status {
		case windows.STATUS_OBJECT_NAME_NOT_FOUND, windows.STATUS_OBJECT_PATH_NOT_FOUND:
			return fmt.Errorf("%w: %v", os.ErrNotExist, err)
		case windows.STATUS_OBJECT_NAME_COLLISION:
			return fmt.Errorf("%w: %v", os.ErrExist, err)
		}
	}
	switch {
	case errors.Is(err, windows.ERROR_FILE_NOT_FOUND), errors.Is(err, windows.ERROR_PATH_NOT_FOUND):
		return fmt.Errorf("%w: %v", os.ErrNotExist, err)
	case errors.Is(err, windows.ERROR_FILE_EXISTS), errors.Is(err, windows.ERROR_ALREADY_EXISTS):
		return fmt.Errorf("%w: %v", os.ErrExist, err)
	default:
		return err
	}
}

func agentTaskWindowsPrincipals() ([]*windows.SID, error) {
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
		if candidate == nil {
			return nil, errors.New("task artifact Windows principal is unavailable")
		}
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

func agentTaskWindowsSecurityDescriptor(directory bool) (*windows.SECURITY_DESCRIPTOR, error) {
	principals, err := agentTaskWindowsPrincipals()
	if err != nil {
		return nil, err
	}
	if len(principals) == 0 || principals[0] == nil {
		return nil, errors.New("task artifact Windows owner is unavailable")
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	sddl := "O:" + principals[0].String() + "D:P"
	for _, principal := range principals {
		if principal == nil || principal.String() == "" {
			return nil, errors.New("task artifact Windows principal is unavailable")
		}
		sddl += "(A;" + inheritance + ";GA;;;" + principal.String() + ")"
	}
	return windows.SecurityDescriptorFromString(sddl)
}

func agentTaskWindowsVerifyOwnerOnlyACL(handle windows.Handle) error {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return err
	}
	if descriptor == nil {
		return errors.New("task artifact Windows security descriptor is unavailable")
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil {
		return errors.New("task artifact Windows owner is unavailable")
	}
	principals, err := agentTaskWindowsPrincipals()
	if err != nil {
		return err
	}
	if !principals[0].Equals(owner) {
		return errors.New("task artifact Windows owner is not the current user")
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	acl, defaulted, err := descriptor.DACL()
	if err != nil {
		return err
	}
	if control&(windows.SE_DACL_PRESENT|windows.SE_DACL_PROTECTED) != windows.SE_DACL_PRESENT|windows.SE_DACL_PROTECTED ||
		defaulted || acl == nil || acl.AceCount != uint16(len(principals)) {
		return errors.New("task artifact Windows owner-only DACL verification failed")
	}
	matched := make([]bool, len(principals))
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			(ace.Mask != windows.ACCESS_MASK(windows.GENERIC_ALL) && ace.Mask != agentTaskWindowsFullControl) {
			return errors.New("task artifact Windows DACL contains an unexpected access entry")
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
			return errors.New("task artifact Windows DACL contains an unexpected principal")
		}
		matched[principalIndex] = true
	}
	return nil
}

func agentTaskWindowsPrepareOwnerOnlyHandle(file *os.File, directory, created bool) error {
	if file == nil {
		return errors.New("task artifact Windows descriptor is unavailable")
	}
	if err := agentTaskWindowsVerifyKind(file, directory); err != nil {
		return err
	}
	if created {
		if err := enforceOwnerOnlyHandle(windows.Handle(file.Fd()), directory); err != nil {
			return err
		}
	}
	return agentTaskWindowsVerifyOwnerOnlyACL(windows.Handle(file.Fd()))
}

func agentTaskWindowsVerifyKind(file *os.File, directory bool) error {
	if file == nil {
		return errors.New("task artifact Windows descriptor is unavailable")
	}
	handle := windows.Handle(file.Fd())
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("task artifact Windows descriptor is a reparse point")
	}
	isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	if isDirectory != directory {
		return errors.New("task artifact Windows descriptor has an unexpected filesystem type")
	}
	if !directory {
		fileType, err := windows.GetFileType(handle)
		if err != nil {
			return err
		}
		if fileType != windows.FILE_TYPE_DISK {
			return errors.New("task artifact Windows descriptor is not a regular disk file")
		}
	}
	return nil
}

func agentTaskWindowsOpenRelative(
	parent *os.File,
	name string,
	directory bool,
	access uint32,
	disposition uint32,
) (*os.File, bool, error) {
	if parent == nil || strings.TrimSpace(name) == "" || filepath.Base(name) != name {
		return nil, false, errors.New("task artifact Windows descriptor-relative request is invalid")
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, false, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	if disposition == windows.FILE_CREATE || disposition == windows.FILE_OPEN_IF {
		descriptor, err := agentTaskWindowsSecurityDescriptor(directory)
		if err != nil {
			return nil, false, err
		}
		attributes.SecurityDescriptor = descriptor
	}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_FOR_BACKUP_INTENT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE | windows.FILE_WRITE_THROUGH
	}
	var status windows.IO_STATUS_BLOCK
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle,
		access,
		attributes,
		&status,
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		options,
		0,
		0,
	); err != nil {
		return nil, false, agentTaskWindowsNormalizeError(err)
	}
	file := os.NewFile(uintptr(handle), filepath.Join(parent.Name(), name))
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, false, errors.New("task artifact Windows descriptor is unavailable")
	}
	return file, status.Information == agentTaskWindowsFileCreated, nil
}

// NtCreateFile's RootDirectory is the Windows equivalent of openat(2). The
// volume root is the only directory opened by path; every descendant is then
// opened relative to an already verified handle with OBJ_DONT_REPARSE. Opening
// the complete path with FILE_FLAG_OPEN_REPARSE_POINT would protect only the
// final component and permit an ancestor junction to redirect the lookup.
func openAgentTaskDirectoryNoFollow(path string) (*os.File, error) {
	return openAgentTaskDirectoryNoFollowWithHook(path, nil)
}

func openAgentTaskDirectoryNoFollowWithHook(path string, hook agentTaskWindowsRootTraversalHook) (*os.File, error) {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." || !filepath.IsAbs(clean) {
		return nil, errors.New("task artifact directory must be an absolute path")
	}
	volume := filepath.VolumeName(clean)
	if volume == "" {
		return nil, errors.New("task artifact directory must have an explicit volume")
	}
	remainder := strings.TrimLeft(strings.TrimPrefix(clean, volume), `\/`)
	parts := strings.FieldsFunc(remainder, func(r rune) bool { return r == '\\' || r == '/' })
	if len(parts) == 0 {
		return nil, errors.New("task artifact directory cannot be a volume root")
	}
	volumeRoot := volume + string(filepath.Separator)
	encoded, err := windows.UTF16PtrFromString(volumeRoot)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return nil, agentTaskWindowsNormalizeError(err)
	}
	file := os.NewFile(uintptr(handle), volumeRoot)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("task artifact Windows directory descriptor is unavailable")
	}
	if err := agentTaskWindowsVerifyKind(file, true); err != nil {
		_ = file.Close()
		return nil, errors.New("task artifact volume root is not a real directory")
	}
	for index, part := range parts {
		if part == "" || part == "." || part == ".." || filepath.Base(part) != part {
			_ = file.Close()
			return nil, errors.New("task artifact directory contains an invalid ancestor component")
		}
		if hook != nil {
			if err := hook(agentTaskWindowsTraversalBeforeComponent, index, part); err != nil {
				_ = file.Close()
				return nil, err
			}
		}
		access := uint32(windows.FILE_GENERIC_READ)
		final := index == len(parts)-1
		if final {
			access |= windows.FILE_GENERIC_WRITE | windows.READ_CONTROL | windows.WRITE_DAC | windows.DELETE
		}
		next, _, err := agentTaskWindowsOpenRelative(file, part, true, access, windows.FILE_OPEN)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := agentTaskWindowsVerifyKind(next, true); err != nil {
			_ = next.Close()
			_ = file.Close()
			return nil, errors.New("task artifact directory ancestor is not a real directory")
		}
		if final {
			if err := agentTaskWindowsPrepareOwnerOnlyHandle(next, true, false); err != nil {
				_ = next.Close()
				_ = file.Close()
				return nil, errors.New("task artifact directory is not an owner-only real directory")
			}
		}
		if hook != nil {
			if err := hook(agentTaskWindowsTraversalAfterComponent, index, part); err != nil {
				_ = next.Close()
				_ = file.Close()
				return nil, err
			}
		}
		_ = file.Close()
		file = next
	}
	return file, nil
}

func validateAgentTaskRegularFile(file *os.File, maxBytes int64) (agentTaskFileStat, error) {
	var result agentTaskFileStat
	if file == nil {
		return result, errors.New("task artifact file descriptor is unavailable")
	}
	if err := agentTaskWindowsVerifyKind(file, false); err != nil {
		return result, err
	}
	if err := agentTaskWindowsVerifyOwnerOnlyACL(windows.Handle(file.Fd())); err != nil {
		return result, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return result, err
	}
	var basic agentTaskWindowsBasicInfo
	if err := windows.GetFileInformationByHandleEx(
		windows.Handle(file.Fd()),
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)),
		uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return result, err
	}
	unsignedSize := uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)
	if unsignedSize > math.MaxInt64 {
		return result, errors.New("task artifact file exceeds the supported size range")
	}
	result = agentTaskFileStat{
		raw:    info,
		basic:  basic,
		Size:   int64(unsignedSize),
		Device: uint64(info.VolumeSerialNumber),
		FileID: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}
	if info.NumberOfLinks != 1 {
		return result, errors.New("task artifact file is not an owner-only unlinked-safe regular file")
	}
	if result.Size < 0 || result.Size > maxBytes {
		return result, errors.New("task artifact file exceeds its configured size bound")
	}
	return result, nil
}

func agentTaskWindowsFileAccess(openMode agentTaskFileOpenMode) (uint32, uint32, error) {
	readAccess := uint32(windows.FILE_GENERIC_READ | windows.READ_CONTROL | windows.WRITE_DAC)
	writeAccess := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.READ_CONTROL | windows.WRITE_DAC | windows.DELETE)
	switch openMode {
	case agentTaskFileReadOnly:
		return readAccess, windows.FILE_OPEN, nil
	case agentTaskFileReadWrite:
		return writeAccess, windows.FILE_OPEN, nil
	case agentTaskFileReadWriteCreate:
		return writeAccess, windows.FILE_OPEN_IF, nil
	case agentTaskFileReadWriteCreateExclusive:
		return writeAccess, windows.FILE_CREATE, nil
	default:
		return 0, 0, errors.New("task artifact file open mode is invalid")
	}
}

func openAgentTaskFileAt(directory *os.File, name string, openMode agentTaskFileOpenMode, _ uint32, maxBytes int64) (*os.File, error) {
	access, disposition, err := agentTaskWindowsFileAccess(openMode)
	if err != nil {
		return nil, err
	}
	file, created, err := agentTaskWindowsOpenRelative(directory, name, false, access, disposition)
	if err != nil {
		return nil, err
	}
	if err := agentTaskWindowsPrepareOwnerOnlyHandle(file, false, created); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := validateAgentTaskRegularFile(file, maxBytes); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openAgentTaskFileNoFollow(path string, maxBytes int64) (*os.File, error) {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, agentTaskWindowsNormalizeError(err)
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("task artifact Windows file descriptor is unavailable")
	}
	if err := agentTaskWindowsPrepareOwnerOnlyHandle(file, false, false); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := validateAgentTaskRegularFile(file, maxBytes); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openAgentTaskDirectoryAt(parent *os.File, name string, create bool) (*os.File, error) {
	disposition := uint32(windows.FILE_OPEN)
	if create {
		disposition = windows.FILE_OPEN_IF
	}
	access := uint32(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.READ_CONTROL | windows.WRITE_DAC | windows.DELETE)
	file, created, err := agentTaskWindowsOpenRelative(parent, name, true, access, disposition)
	if err != nil {
		return nil, err
	}
	if err := agentTaskWindowsPrepareOwnerOnlyHandle(file, true, created); err != nil {
		_ = file.Close()
		return nil, errors.New("task artifact shard is not an owner-only real directory")
	}
	return file, nil
}

func agentTaskFlockContext(ctx context.Context, file *os.File, exclusive bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if file == nil {
		return errors.New("task artifact namespace lock file is unavailable")
	}
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	for {
		overlapped := &windows.Overlapped{}
		err := windows.LockFileEx(windows.Handle(file.Fd()), flags, 0, 1, 0, overlapped)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func agentTaskUnlock(file *os.File) error {
	if file == nil {
		return nil
	}
	return windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &windows.Overlapped{})
}

func agentTaskWindowsRelativeNameBuffer(name string, nameOffset int) ([]byte, error) {
	encoded, err := windows.UTF16FromString(name)
	if err != nil {
		return nil, err
	}
	encoded = encoded[:len(encoded)-1]
	buffer := make([]byte, nameOffset+len(encoded)*2)
	for index, value := range encoded {
		binary.LittleEndian.PutUint16(buffer[nameOffset+index*2:], value)
	}
	return buffer, nil
}

func agentTaskRenameAt(parent *os.File, oldName, newName string) error {
	if parent == nil || filepath.Base(oldName) != oldName || filepath.Base(newName) != newName {
		return errors.New("task artifact rename descriptor request is invalid")
	}
	access := uint32(windows.FILE_GENERIC_READ | windows.DELETE | windows.READ_CONTROL | windows.WRITE_DAC)
	file, _, err := agentTaskWindowsOpenRelative(parent, oldName, false, access, windows.FILE_OPEN)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := agentTaskWindowsPrepareOwnerOnlyHandle(file, false, false); err != nil {
		return err
	}
	if _, err := validateAgentTaskRegularFile(file, math.MaxInt64); err != nil {
		return err
	}
	nameOffset := int(unsafe.Offsetof(agentTaskWindowsRenameInformationEx{}.FileName))
	buffer, err := agentTaskWindowsRelativeNameBuffer(newName, nameOffset)
	if err != nil {
		return err
	}
	info := (*agentTaskWindowsRenameInformationEx)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_POSIX_SEMANTICS
	info.RootDirectory = windows.Handle(parent.Fd())
	info.FileNameLength = uint32(len(buffer) - nameOffset)
	err = windows.NtSetInformationFile(
		windows.Handle(file.Fd()),
		&windows.IO_STATUS_BLOCK{},
		&buffer[0],
		uint32(len(buffer)),
		agentTaskWindowsFileRenameInformationEx,
	)
	return agentTaskWindowsNormalizeError(err)
}

func agentTaskLinkAt(parent, source *os.File, sourceName, targetName string) error {
	if parent == nil || source == nil || filepath.Base(sourceName) != sourceName || filepath.Base(targetName) != targetName {
		return errors.New("task artifact link descriptor request is invalid")
	}
	if _, err := validateAgentTaskRegularFile(source, math.MaxInt64); err != nil {
		return err
	}
	nameOffset := int(unsafe.Offsetof(agentTaskWindowsLinkInformation{}.FileName))
	buffer, err := agentTaskWindowsRelativeNameBuffer(targetName, nameOffset)
	if err != nil {
		return err
	}
	info := (*agentTaskWindowsLinkInformation)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_LINK_POSIX_SEMANTICS
	info.RootDirectory = windows.Handle(parent.Fd())
	info.FileNameLength = uint32(len(buffer) - nameOffset)
	err = windows.NtSetInformationFile(
		windows.Handle(source.Fd()),
		&windows.IO_STATUS_BLOCK{},
		&buffer[0],
		uint32(len(buffer)),
		windows.FileLinkInformation,
	)
	return agentTaskWindowsNormalizeError(err)
}

func agentTaskUnlinkAt(parent *os.File, name string) error {
	if parent == nil || filepath.Base(name) != name {
		return errors.New("task artifact unlink descriptor request is invalid")
	}
	access := uint32(windows.FILE_GENERIC_READ | windows.DELETE | windows.READ_CONTROL | windows.WRITE_DAC)
	file, _, err := agentTaskWindowsOpenRelative(parent, name, false, access, windows.FILE_OPEN)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := agentTaskWindowsPrepareOwnerOnlyHandle(file, false, false); err != nil {
		return err
	}
	disposition := agentTaskWindowsDispositionInformationEx{
		Flags: windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_ON_CLOSE,
	}
	return windows.NtSetInformationFile(
		windows.Handle(file.Fd()),
		&windows.IO_STATUS_BLOCK{},
		(*byte)(unsafe.Pointer(&disposition)),
		uint32(unsafe.Sizeof(disposition)),
		windows.FileDispositionInformationEx,
	)
}

func agentTaskSyncDirectory(directory *os.File) error {
	if directory == nil {
		return errors.New("task artifact directory descriptor is unavailable")
	}
	return windows.FlushFileBuffers(windows.Handle(directory.Fd()))
}
