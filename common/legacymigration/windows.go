//go:build windows

package legacymigration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	directorySecurity = `O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)`
	fileSecurity      = `O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)`
	fileAllAccess     = 0x1f01ff
)

// Import verifies and imports a private snapshot before the destination service starts.
func Import(ctx context.Context, sid, source, id, requestPath string, verify Verify, saveOwner func(string) error) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	parsedSID, err := windows.StringToSid(sid)
	if err != nil || parsedSID.String() != sid || parsedSID.IsWellKnown(windows.WinLocalSystemSid) {
		return errors.New("invalid migration source SID")
	}
	_, _, accountType, err := parsedSID.LookupAccount("")
	if err != nil || accountType != windows.SidTypeUser {
		return errors.New("migration source SID does not identify a user")
	}
	source, err = localDirectoryPath(source)
	if err != nil {
		return err
	}
	ancestors, err := openDirectoryChain(source)
	if err != nil {
		return errors.New("invalid migration source directory")
	}
	defer closeHandles(ancestors)
	request, err := readRequestFile(requestPath, sid, source, id)
	if err != nil {
		return err
	}
	dataDir, err := serviceDataDirectory()
	if err != nil {
		return err
	}
	return withImportLock(func() error {
		if err := requireStoppedDestination(); err != nil {
			return err
		}
		if err := ProtectDataDir(dataDir); err != nil {
			return err
		}
		return importSettings(ctx, dataDir, request, verify, func() error {
			if saveOwner == nil {
				return errors.New("missing service owner persistence")
			}
			return saveOwner(sid)
		})
	})
}

func requireStoppedDestination() error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService("LanternSvc")
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return err
	}
	if status.State != svc.Stopped {
		return ErrConflict
	}
	return nil
}

func requireAdministrator() error {
	sid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return err
	}
	member, err := windows.Token(0).IsMember(sid)
	if err != nil || !member {
		return ErrUnauthorized
	}
	return nil
}

func withImportLock(fn func() error) error {
	sd, err := windows.SecurityDescriptorFromString(`O:BAG:BAD:P(A;;0x1f0001;;;SY)(A;;0x1f0001;;;BA)`)
	if err != nil {
		return err
	}
	name, _ := windows.UTF16PtrFromString(`Global\Lantern.LegacyImport`)
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateMutexEx(&sa, name, 0, windows.MUTEX_ALL_ACCESS)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := verifyProtectedObject(handle, windows.SE_KERNEL_OBJECT, windows.MUTEX_ALL_ACCESS, true); err != nil {
		return err
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	result, err := windows.WaitForSingleObject(handle, 10000)
	if err != nil {
		return err
	}
	if result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED {
		return errors.New("legacy import is busy")
	}
	defer windows.ReleaseMutex(handle)
	return fn()
}

func readRequestFile(path, sid, source, id string) (Request, error) {
	path, err := localDirectoryPath(path)
	if err != nil {
		return Request{}, errors.New("invalid migration snapshot path")
	}
	ancestors, err := openDirectoryChain(filepath.Dir(path))
	if err != nil {
		return Request{}, errors.New("invalid migration snapshot directory")
	}
	defer closeHandles(ancestors)
	handle, info, err := openPath(path, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ)
	if err != nil {
		return Request{}, errors.New("cannot open migration snapshot")
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 || info.FileSizeHigh != 0 || info.FileSizeLow > maxRequestBytes {
		return Request{}, errors.New("invalid migration snapshot size")
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || verifyRequestDescriptor(sd, sid) != nil {
		return Request{}, errors.New("migration snapshot permissions are not private")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxRequestBytes+1))
	if err != nil {
		return Request{}, errors.New("cannot read migration snapshot")
	}
	return decodeRequest(raw, source, id)
}

func verifyRequestDescriptor(sd *windows.SECURITY_DESCRIPTOR, sourceSID string) error {
	if sd == nil {
		return ErrUnauthorized
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !(trustedOwner(owner.String()) || owner.String() == sourceSID) {
		return ErrUnauthorized
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return ErrUnauthorized
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return ErrUnauthorized
		}
		principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if !(trustedOwner(principal) || principal == sourceSID) {
			return ErrUnauthorized
		}
	}
	return nil
}

func verifyProtectedObject(handle windows.Handle, kind windows.SE_OBJECT_TYPE, access uint32, protected bool) error {
	sd, err := windows.GetSecurityInfo(handle, kind, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return verifyProtectedDescriptor(sd, access, protected)
}

func verifyProtectedDescriptor(sd *windows.SECURITY_DESCRIPTOR, access uint32, protected bool) error {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !trustedOwner(owner.String()) {
		return errors.New("owner is not SYSTEM or Administrators")
	}
	control, _, err := sd.Control()
	if err != nil || protected && control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("security descriptor permits inherited access")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		return errors.New("expected exactly two restricted access entries")
	}
	seen := make(map[string]bool, 2)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || uint32(ace.Mask) != access || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			return errors.New("unexpected access entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if !trustedOwner(sid) || seen[sid] {
			return errors.New("unexpected access principal")
		}
		seen[sid] = true
	}
	return nil
}

func trustedOwner(sid string) bool {
	return sid == "S-1-5-18" || sid == "S-1-5-32-544"
}

func serviceDataDirectory() (string, error) {
	path, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", err
	}
	return filepath.Join(path, "Lantern"), nil
}

func localDirectoryPath(path string) (string, error) {
	if path != filepath.Clean(path) {
		return "", errors.New("ambiguous migration directory path")
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' || !filepath.IsAbs(path) || strings.ContainsRune(path[2:], ':') {
		return "", errors.New("migration directory must use an absolute local drive path")
	}
	for _, component := range strings.Split(path[3:], `\`) {
		if component == "" || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
			return "", errors.New("ambiguous migration directory path")
		}
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil || windows.GetDriveType(root) != windows.DRIVE_FIXED {
		return "", errors.New("migration directory must be on a fixed local drive")
	}
	return path, nil
}

func closeHandles(handles []windows.Handle) {
	for i := len(handles) - 1; i >= 0; i-- {
		windows.CloseHandle(handles[i])
	}
}

func openDirectoryChain(path string) ([]windows.Handle, error) {
	var handles []windows.Handle
	current := filepath.VolumeName(path) + `\`
	parts := append([]string{""}, strings.Split(strings.TrimPrefix(path, current), `\`)...)
	for _, part := range parts {
		if part != "" {
			current = filepath.Join(current, part)
		}
		handle, info, err := openPath(current, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
		if err != nil {
			closeHandles(handles)
			return nil, err
		}
		handles = append(handles, handle)
		if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			closeHandles(handles)
			return nil, errors.New("migration path contains a non-directory")
		}
	}
	return handles, nil
}

func openPath(path string, access, share uint32) (windows.Handle, windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, info, err
	}
	handle, err := windows.CreateFile(name, access, share, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, info, err
	}
	err = windows.GetFileInformationByHandle(handle, &info)
	if err == nil && info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		err = errors.New("migration path contains a reparse point")
	}
	if err == nil && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 && info.NumberOfLinks != 1 {
		err = errors.New("migration data contains a hard link")
	}
	if err != nil {
		windows.CloseHandle(handle)
		return 0, info, err
	}
	return handle, info, nil
}

// ProtectDataDir restricts the fixed service directory and descendants to SYSTEM and administrators.
func ProtectDataDir(path string) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	expected, err := serviceDataDirectory()
	if err != nil {
		return err
	}
	path, err = localDirectoryPath(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(path, expected) {
		return errors.New("unexpected migration service data directory")
	}
	parents, err := openDirectoryChain(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer closeHandles(parents)
	sd, err := windows.SecurityDescriptorFromString(directorySecurity)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	name, _ := windows.UTF16PtrFromString(path)
	if err := windows.CreateDirectory(name, &sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	return protectPath(path, true)
}

func protectPath(path string, root bool) error {
	handle, info, err := openPath(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := verifyProtectedObject(handle, windows.SE_FILE_OBJECT, fileAllAccess, root); err != nil {
		// MAXIMUM_ALLOWED prevents SetSecurityInfo from traversing unchecked descendants.
		exclusive, lockedInfo, err := openPath(path, windows.MAXIMUM_ALLOWED, windows.FILE_SHARE_READ)
		if err != nil {
			return fmt.Errorf("protect migration data: %w", err)
		}
		defer windows.CloseHandle(exclusive)
		if info.VolumeSerialNumber != lockedInfo.VolumeSerialNumber || info.FileIndexHigh != lockedInfo.FileIndexHigh || info.FileIndexLow != lockedInfo.FileIndexLow {
			return errors.New("migration data changed during protection")
		}
		security := fileSecurity
		if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
			security = directorySecurity
		}
		sd, err := windows.SecurityDescriptorFromString(security)
		if err != nil {
			return err
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return err
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return err
		}
		if err := windows.SetSecurityInfo(exclusive, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil); err != nil {
			return err
		}
		if err := verifyProtectedObject(exclusive, windows.SE_FILE_OBJECT, fileAllAccess, true); err != nil {
			return err
		}
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return nil
	}
	readHandle, _, err := openPath(path, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(readHandle), path)
	defer directory.Close()
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			if err := protectPath(filepath.Join(path, entry.Name()), false); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
