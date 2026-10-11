//go:build windows

package legacymigration

import (
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/windows"
)

// GrantProcessQuery grants the enrolled user limited query access to the current LocalSystem process when protected migration enrollment exists.
func GrantProcessQuery() error {
	record, err := Load()
	if err != nil || record == nil {
		return err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	if !user.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return ErrUnauthorized
	}
	sid, err := processQuerySID(record)
	if err != nil {
		return err
	}
	return grantProcessQuery(windows.CurrentProcess(), sid)
}

func processQuerySID(record *Record) (*windows.SID, error) {
	if err := validateRecord(record); err != nil {
		return nil, err
	}
	sid, err := windows.StringToSid(record.SourceSID)
	if err != nil || sid.String() != record.SourceSID || sid.IsWellKnown(windows.WinLocalSystemSid) {
		return nil, errors.New("invalid migration source SID")
	}
	return sid, nil
}

func grantProcessQuery(handle windows.Handle, sid *windows.SID) error {
	if sid == nil {
		return errors.New("missing migration source SID")
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read migration process permissions: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("migration process permissions are unrestricted")
	}
	var pinned runtime.Pinner
	pinned.Pin(sid)
	defer pinned.Unpin()
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.PROCESS_QUERY_LIMITED_INFORMATION,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}
	dacl, err = windows.ACLFromEntries(entries, dacl)
	if err != nil {
		return fmt.Errorf("merge migration process permissions: %w", err)
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("grant migration process query access: %w", err)
	}
	return nil
}
