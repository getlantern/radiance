//go:build windows

package legacymigration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func TestProtectedRegistryDescriptor(t *testing.T) {
	tests := []struct {
		name, sddl string
		valid      bool
	}{
		{"restricted", registrySecurity, true},
		{"system owner", `O:SYG:SYD:P(A;;KA;;;BA)(A;;KA;;;SY)`, true},
		{"inherited", `O:BAG:BAD:(A;;KA;;;SY)(A;;KA;;;BA)`, false},
		{"untrusted owner", `O:IUG:BAD:P(A;;KA;;;SY)(A;;KA;;;BA)`, false},
		{"interactive read", `O:BAG:BAD:P(A;;KA;;;SY)(A;;KA;;;BA)(A;;KR;;;IU)`, false},
		{"duplicate administrator", `O:BAG:BAD:P(A;;KA;;;BA)(A;;KA;;;BA)`, false},
		{"inherited only", `O:BAG:BAD:P(A;CIIO;KA;;;SY)(A;;KA;;;BA)`, false},
		{"insufficient access", `O:BAG:BAD:P(A;;KR;;;SY)(A;;KA;;;BA)`, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyProtectedDescriptor(sd, registry.ALL_ACCESS, true); (err == nil) != test.valid {
				t.Fatalf("verification error = %v, expected valid = %v", err, test.valid)
			}
		})
	}
}

func TestPendingEnrollmentCannotReplaceAcceptedState(t *testing.T) {
	base := Record{Version: 1, MigrationID: strings.Repeat("a", 32), SourceSID: "S-1-5-21-1-2-3-1001", SourceDirectory: `C:\Users\Original\AppData\Roaming\Lantern`, Status: "pending"}
	if !matchingPendingEnrollment(&base, base.SourceSID, strings.ToLower(base.SourceDirectory), base.MigrationID) {
		t.Fatal("equivalent pending enrollment rejected")
	}
	tests := map[string]func(*Record){
		"request":           func(r *Record) { r.Request = &Request{} },
		"receipt":           func(r *Record) { r.Receipt = &Receipt{} },
		"accepted":          func(r *Record) { r.Status = "adopted" },
		"completed":         func(r *Record) { r.Status = "completed" },
		"different user":    func(r *Record) { r.SourceSID += "1" },
		"different source":  func(r *Record) { r.SourceDirectory += "-other" },
		"different id":      func(r *Record) { r.MigrationID = strings.Repeat("b", 32) },
		"persisted hash":    func(r *Record) { r.RequestSHA256 = "hash" },
		"persisted account": func(r *Record) { r.UserData = []byte(`{}`) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if matchingPendingEnrollment(&r, base.SourceSID, base.SourceDirectory, base.MigrationID) {
				t.Fatal("conflicting enrollment accepted")
			}
		})
	}
}

func TestDestinationSettingsFreshness(t *testing.T) {
	for _, name := range []string{"settings.json", "local.json", "settings.yaml", filepath.Join("data", "settings.json"), filepath.Join("data", "local.json")} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := requireNoDestinationSettings(dir); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := requireNoDestinationSettings(dir); !errors.Is(err, ErrConflict) {
				t.Fatalf("expected conflict, got %v", err)
			}
		})
	}
}

func TestMigrationPipeSIDAfterReadAndAnonymousIsolation(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	path := fmt.Sprintf(`\\.\pipe\Lantern-Migration-Test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	raw, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: `D:P(A;;GA;;;` + sid + `)`})
	if err != nil {
		t.Fatal(err)
	}
	listener := &migrationListener{Listener: raw}
	t.Cleanup(func() { listener.Close() })
	for _, level := range []winio.PipeImpLevel{winio.PipeImpLevelAnonymous, winio.PipeImpLevelIdentification} {
		type acceptedConnection struct {
			conn net.Conn
			err  error
		}
		accepted := make(chan acceptedConnection, 1)
		go func() {
			conn, err := listener.Accept()
			accepted <- acceptedConnection{conn, err}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		client, err := winio.DialPipeAccessImpLevel(ctx, path, 0x12019b, level)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		connection := <-accepted
		if connection.err != nil {
			client.Close()
			t.Fatal(connection.err)
		}
		server := connection.conn
		if _, err := PeerSID(server); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("identity exposed before first read: %v", err)
		}
		server.SetReadDeadline(time.Now().Add(5 * time.Second))
		client.SetWriteDeadline(time.Now().Add(5 * time.Second))
		written := make(chan error, 1)
		go func() { _, err := client.Write([]byte("hello")); written <- err }()
		buffer := make([]byte, 5)
		_, readErr := io.ReadFull(server, buffer)
		if level == winio.PipeImpLevelAnonymous {
			if readErr == nil {
				t.Fatal("anonymous pipe client was authenticated")
			}
			if _, err := PeerSID(server); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("anonymous peer identity: %v", err)
			}
		} else {
			if readErr != nil {
				t.Fatal(readErr)
			}
			actual, err := PeerSID(server)
			if err != nil || actual != sid {
				t.Fatalf("peer = %q, %v; wanted %q", actual, err, sid)
			}
		}
		client.Close()
		server.Close()
		<-written
	}
}

func TestLocalMigrationPathsRejectAliases(t *testing.T) {
	for _, path := range []string{`relative`, `\\server\share\Lantern`, `\\?\C:\Lantern`, `C:Lantern`, `C:\Lantern:stream`, `C:\Lantern.`, `C:\Lantern `} {
		if _, err := localDirectoryPath(path); err == nil {
			t.Fatalf("accepted path %q", path)
		}
	}
}

func TestMigrationDataRejectsHardLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "alias.json")); err != nil {
		t.Fatal(err)
	}
	handle, _, err := openPath(path, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err == nil {
		windows.CloseHandle(handle)
		t.Fatal("accepted hard-linked migration data")
	}
}

func TestProtectedRegistryPersistence(t *testing.T) {
	if err := requireAdministrator(); err != nil {
		t.Skip("requires an elevated Windows test process")
	}
	name := fmt.Sprintf("Lantern-LegacyMigration-Test-%d-%d", os.Getpid(), time.Now().UnixNano())
	path := `Software\` + name
	if err := createEnrollmentKey(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		software, err := registry.OpenKey(registry.LOCAL_MACHINE, `Software`, registry.WRITE|registry.WOW64_64KEY)
		if err != nil {
			t.Error(err)
			return
		}
		defer software.Close()
		if err := registry.DeleteKey(software, name); err != nil {
			t.Error(err)
		}
	})
	store := &registryStore{keyPath: path}
	if record, err := store.Load(); err != nil || record != nil {
		t.Fatalf("empty store = %#v, %v", record, err)
	}
	record := &Record{Version: 1, MigrationID: strings.Repeat("a", 32), SourceSID: "S-1-5-21-1-2-3-1001", SourceDirectory: `C:\Users\Original\Lantern`, Status: "pending"}
	if err := store.save(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded == nil || loaded.MigrationID != record.MigrationID || loaded.SourceSID != record.SourceSID {
		t.Fatalf("loaded enrollment = %#v, %v", loaded, err)
	}
	key, err := openEnrollmentKey(path, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	if err := key.SetExpandStringValue(migrationValueName, `{"version":1}`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("accepted non-REG_SZ enrollment")
	}
}

func TestEnrollmentLockWaitsAndClosesHandles(t *testing.T) {
	if err := requireAdministrator(); err != nil {
		t.Skip("requires an elevated Windows test process")
	}
	name, err := windows.UTF16PtrFromString(`Global\Lantern.LegacyMigrationV1.Registry`)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err == nil {
		windows.CloseHandle(handle)
		t.Skip("migration enrollment mutex is already in use")
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatal(err)
	}
	var retainedHandle windows.Handle
	t.Cleanup(func() {
		if retainedHandle != 0 {
			windows.CloseHandle(retainedHandle)
		}
	})
	done := make(chan error, 1)
	if err := withEnrollmentLock(func() error {
		retainedHandle, err = windows.OpenMutex(windows.SYNCHRONIZE, false, name)
		if err != nil {
			return err
		}
		started := make(chan struct{})
		go func() {
			close(started)
			done <- withEnrollmentLock(func() error { return nil })
		}()
		<-started
		select {
		case err := <-done:
			return fmt.Errorf("waiter returned while enrollment lock was held: %v", err)
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not acquire the released enrollment lock")
	}
	if err := windows.CloseHandle(retainedHandle); err != nil {
		t.Fatal(err)
	}
	retainedHandle = 0
	handle, err = windows.OpenMutex(windows.SYNCHRONIZE, false, name)
	if err == nil {
		windows.CloseHandle(handle)
		t.Fatal("enrollment lock left an open mutex handle")
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatal(err)
	}
}

func TestEnrollmentUpdatePreservesAnUncertainCommit(t *testing.T) {
	existing := Record{Version: 1, MigrationID: strings.Repeat("a", 32), SourceSID: "S-1-5-21-1-2-3-1001", SourceDirectory: `C:\Users\Original\Lantern`, Status: "adopted", RequestSHA256: "accepted", Request: &Request{UserID: 123, Token: "first", DeviceID: "device"}}
	completed := existing
	completed.Status = "completed"
	if !compatibleEnrollmentUpdate(&existing, &completed) {
		t.Fatal("accepted completion rejected")
	}
	different := existing
	request := *existing.Request
	request.Token = "second"
	different.Request = &request
	if compatibleEnrollmentUpdate(&existing, &different) {
		t.Fatal("allowed replacing an already-written request after a flush failure")
	}
	different = existing
	different.RequestSHA256 = "replacement"
	if compatibleEnrollmentUpdate(&existing, &different) {
		t.Fatal("allowed changing the accepted payload digest")
	}
	if compatibleEnrollmentUpdate(&completed, &existing) {
		t.Fatal("allowed reverting a completed enrollment")
	}
}

func TestProtectionRejectsReparseWithoutChangingItsTarget(t *testing.T) {
	if err := requireAdministrator(); err != nil {
		t.Skip("requires an elevated Windows test process")
	}
	dir, target := t.TempDir(), t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "linked")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	handle, _, err := openPath(target, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	before, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := protectPath(dir, true); err == nil {
		t.Fatal("accepted a reparse point in migration data")
	}
	after, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if before.String() != after.String() {
		t.Fatal("changed permissions outside the migration data directory")
	}
}
