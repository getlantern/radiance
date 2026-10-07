//go:build windows

package legacymigration

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	migrationPipePath  = `\\.\pipe\Lantern\legacy-migration-v1`
	migrationKeyPath   = `Software\Lantern\LegacyMigrationV1`
	migrationValueName = "Record"
	registrySecurity   = `O:BAG:BAD:P(A;;KA;;;SY)(A;;KA;;;BA)`
	directorySecurity  = `O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)`
	fileSecurity       = `O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)`
	fileAllAccess      = 0x1f01ff
	// Client rights exclude FILE_CREATE_PIPE_INSTANCE, which generic write includes.
	pipeSecurity = `D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12019b;;;IU)`
)

var (
	migrationAdvapi       = windows.NewLazySystemDLL("advapi32.dll")
	regCreateKeyEx        = migrationAdvapi.NewProc("RegCreateKeyExW")
	regFlushKey           = migrationAdvapi.NewProc("RegFlushKey")
	impersonatePipeClient = migrationAdvapi.NewProc("ImpersonateNamedPipeClient")
)

type registryStore struct{ keyPath string }

// OpenStore returns the administrator-controlled Windows enrollment store.
func OpenStore() (Store, error) {
	return &registryStore{keyPath: migrationKeyPath}, nil
}

func (s *registryStore) Load() (*Record, error) {
	key, err := openEnrollmentKey(s.keyPath, registry.QUERY_VALUE)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer key.Close()
	value, valueType, err := key.GetStringValue(migrationValueName)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read migration enrollment: %w", err)
	}
	if valueType != registry.SZ {
		return nil, errors.New("invalid migration enrollment value type")
	}
	var record Record
	if err := json.Unmarshal([]byte(value), &record); err != nil {
		return nil, errors.New("invalid migration enrollment JSON")
	}
	if record.Version != 1 || record.MigrationID == "" || record.SourceSID == "" {
		return nil, errors.New("invalid migration enrollment")
	}
	return &record, nil
}

func (s *registryStore) Save(record *Record) error {
	if record == nil {
		return errors.New("missing migration enrollment")
	}
	return withEnrollmentLock(func() error { return s.save(record) })
}

func (s *registryStore) save(record *Record) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	existing, err := s.Load()
	if err != nil {
		return err
	}
	if existing != nil && !compatibleEnrollmentUpdate(existing, record) {
		return ErrConflict
	}
	value, err := json.Marshal(record)
	if err != nil {
		return errors.New("encode migration enrollment")
	}
	key, err := openEnrollmentKey(s.keyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if err := key.SetStringValue(migrationValueName, string(value)); err != nil {
		return fmt.Errorf("persist migration enrollment: %w", err)
	}
	result, _, _ := regFlushKey.Call(uintptr(key))
	if result != 0 {
		return fmt.Errorf("flush migration enrollment: %w", windows.Errno(result))
	}
	return nil
}

func compatibleEnrollmentUpdate(existing, next *Record) bool {
	if existing.Version != next.Version || existing.MigrationID != next.MigrationID || existing.SourceSID != next.SourceSID || !strings.EqualFold(existing.SourceDirectory, next.SourceDirectory) {
		return false
	}
	if existing.Status == "completed" && next.Status != "completed" {
		return false
	}
	if existing.Request != nil {
		return next.Request != nil && existing.RequestSHA256 == next.RequestSHA256 && *existing.Request == *next.Request
	}
	return true
}

func openEnrollmentKey(path string, access uint32) (registry.Key, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, access|windows.READ_CONTROL|registry.WOW64_64KEY)
	if err != nil {
		return 0, err
	}
	if err := verifyProtectedObject(windows.Handle(key), windows.SE_REGISTRY_KEY, registry.ALL_ACCESS, true); err != nil {
		key.Close()
		return 0, fmt.Errorf("untrusted migration registry permissions: %w", err)
	}
	return key, nil
}

func createEnrollmentKey(keyPath string) error {
	sd, err := windows.SecurityDescriptorFromString(registrySecurity)
	if err != nil {
		return err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	path, err := windows.UTF16PtrFromString(keyPath)
	if err != nil {
		return err
	}
	var key registry.Key
	var disposition uint32
	result, _, _ := regCreateKeyEx.Call(uintptr(registry.LOCAL_MACHINE), uintptr(unsafe.Pointer(path)), 0, 0, 0,
		uintptr(registry.READ|registry.WOW64_64KEY), uintptr(unsafe.Pointer(&sa)), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&disposition)))
	runtime.KeepAlive(sd)
	if result != 0 {
		return fmt.Errorf("create migration enrollment key: %w", windows.Errno(result))
	}
	defer key.Close()
	return verifyProtectedObject(windows.Handle(key), windows.SE_REGISTRY_KEY, registry.ALL_ACCESS, true)
}

// Prepare enrolls a user and protects the service data before credentials arrive.
func Prepare(sid, source, id string) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	parsedSID, err := windows.StringToSid(sid)
	if err != nil || parsedSID.String() != sid {
		return errors.New("invalid migration source SID")
	}
	_, _, accountType, err := parsedSID.LookupAccount("")
	if err != nil || accountType != windows.SidTypeUser {
		return errors.New("migration source SID does not identify a user")
	}
	if !migrationIDPattern.MatchString(id) {
		return errors.New("migration ID must contain 32 lowercase hexadecimal digits")
	}
	source, err = localDirectoryPath(source)
	if err != nil {
		return err
	}
	ancestors, err := openDirectoryChain(source)
	if err != nil {
		return fmt.Errorf("migration source directory: %w", err)
	}
	defer closeHandles(ancestors)
	return withEnrollmentLock(func() error {
		s := &registryStore{keyPath: migrationKeyPath}
		existing, err := s.Load()
		if err != nil {
			return err
		}
		if existing != nil && !matchingPendingEnrollment(existing, sid, source, id) {
			return ErrConflict
		}
		dataDir, err := serviceDataDirectory()
		if err != nil {
			return err
		}
		if err := requireFreshDestination(dataDir); err != nil {
			return err
		}
		if err := createEnrollmentKey(migrationKeyPath); err != nil {
			return err
		}
		if err := ProtectDataDir(dataDir); err != nil {
			return err
		}
		if existing != nil {
			return nil
		}
		return s.save(&Record{Version: 1, MigrationID: id, SourceSID: sid, SourceDirectory: source, Status: "pending"})
	})
}

func requireFreshDestination(path string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService("LanternSvc")
	if err == nil {
		service.Close()
		return ErrConflict
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return err
	}
	return requireNoDestinationSettings(path)
}

func requireNoDestinationSettings(path string) error {
	for _, relative := range []string{"settings.json", "local.json", "settings.yaml", "settings.yml", filepath.Join("data", "settings.json"), filepath.Join("data", "local.json"), filepath.Join("data", "settings.yaml"), filepath.Join("data", "settings.yml")} {
		_, err := os.Lstat(filepath.Join(path, relative))
		if err == nil {
			return ErrConflict
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func matchingPendingEnrollment(record *Record, sid, source, id string) bool {
	return record.Version == 1 && record.MigrationID == id && record.SourceSID == sid &&
		strings.EqualFold(filepath.Clean(record.SourceDirectory), source) && record.Status == "pending" &&
		record.Request == nil && record.Receipt == nil && record.RequestSHA256 == "" && len(record.UserData) == 0
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

func withEnrollmentLock(fn func() error) error {
	sd, err := windows.SecurityDescriptorFromString(`O:BAG:BAD:P(A;;0x1f0001;;;SY)(A;;0x1f0001;;;BA)`)
	if err != nil {
		return err
	}
	name, _ := windows.UTF16PtrFromString(`Global\Lantern.LegacyMigrationV1.Registry`)
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateMutexEx(&sa, name, 0, windows.MUTEX_ALL_ACCESS)
	// ERROR_ALREADY_EXISTS still returns a handle that must be closed.
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := verifyProtectedObject(handle, windows.SE_KERNEL_OBJECT, windows.MUTEX_ALL_ACCESS, true); err != nil {
		return err
	}
	// Windows mutex ownership follows the OS thread, including ReleaseMutex.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	result, err := windows.WaitForSingleObject(handle, 10000)
	if err != nil {
		return err
	}
	if result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED {
		return errors.New("migration enrollment is busy")
	}
	defer windows.ReleaseMutex(handle)
	return fn()
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

// Listen accepts local migration connections without granting pipe creation rights.
func Listen() (net.Listener, error) {
	if err := requireAdministrator(); err != nil {
		return nil, err
	}
	listener, err := winio.ListenPipe(migrationPipePath, &winio.PipeConfig{
		SecurityDescriptor: pipeSecurity,
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	if err != nil {
		return nil, err
	}
	return &migrationListener{Listener: listener}, nil
}

type migrationListener struct{ net.Listener }

type pipeConnection interface {
	net.Conn
	Fd() uintptr
}

type migrationConn struct {
	pipeConnection
	once  sync.Once
	sid   string
	err   error
	ready chan struct{}
}

func (l *migrationListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pipe, ok := conn.(pipeConnection)
	if !ok {
		conn.Close()
		return nil, errors.New("migration listener returned an unsupported connection")
	}
	return &migrationConn{pipeConnection: pipe, ready: make(chan struct{})}, nil
}

func (c *migrationConn) Read(buffer []byte) (int, error) {
	n, err := c.pipeConnection.Read(buffer)
	if n > 0 {
		c.once.Do(func() {
			c.sid, c.err = readPipeSID(windows.Handle(c.Fd()))
			close(c.ready)
		})
		if c.err != nil {
			c.Close()
			return 0, c.err
		}
	}
	return n, err
}

// PeerSID returns the authenticated SID after the connection's first successful read.
func PeerSID(conn net.Conn) (string, error) {
	pipe, ok := conn.(*migrationConn)
	if !ok {
		return "", ErrUnauthorized
	}
	select {
	case <-pipe.ready:
		if pipe.err != nil || pipe.sid == "" {
			return "", ErrUnauthorized
		}
		return pipe.sid, nil
	default:
		return "", ErrUnauthorized
	}
}

func readPipeSID(handle windows.Handle) (string, error) {
	type result struct {
		sid string
		err error
	}
	results := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		impersonated, _, callErr := impersonatePipeClient.Call(uintptr(handle))
		if impersonated == 0 {
			runtime.UnlockOSThread()
			results <- result{err: fmt.Errorf("identify migration pipe client: %w", callErr)}
			return
		}
		var token windows.Token
		err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token)
		var sid string
		if err == nil {
			var user *windows.Tokenuser
			user, err = token.GetTokenUser()
			if err == nil {
				sid = user.User.Sid.String()
			}
			token.Close()
		}
		if revertErr := windows.RevertToSelf(); revertErr != nil {
			results <- result{err: fmt.Errorf("revert migration pipe impersonation: %w", revertErr)}
			// A goroutine that exits while locked causes Go to discard its OS thread.
			return
		}
		runtime.UnlockOSThread()
		results <- result{sid: sid, err: err}
	}()
	r := <-results
	return r.sid, r.err
}

func serviceDataDirectory() (string, error) {
	path, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return "", err
	}
	return filepath.Join(path, "Lantern"), nil
}

func localDirectoryPath(path string) (string, error) {
	path = filepath.Clean(path)
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
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
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
