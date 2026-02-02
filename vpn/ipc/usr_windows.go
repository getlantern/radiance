package ipc

import (
	"fmt"
	"log/slog"

	"golang.org/x/sys/windows"
)

func usrFromToken(t windows.Token) (p usr, err error) {
	u, err := t.GetTokenUser()
	if err != nil {
		return p, fmt.Errorf("failed to get token user: %w", err)
	}
	uname, _, _, err := u.User.Sid.LookupAccount("")
	if err != nil {
		return p, fmt.Errorf("failed to lookup account name: %w", err)
	}
	isAdm, err := isAdmin(t)
	if err != nil {
		slog.Warn("failed to check admin status", "error", err)
	}
	return usr{
		uid:     u.User.Sid.String(),
		uname:   uname,
		isAdmin: isAdm,
	}, nil
}

func isAdmin(t windows.Token) (bool, error) {
	adminSid, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return false, fmt.Errorf("failed to create admin sid: %w", err)
	}
	return t.IsMember(adminSid)
}
