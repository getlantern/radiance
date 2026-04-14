package ipc

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"time"
)

var executable string

func init() {
	exe, err := os.Executable()
	if err != nil {
		// if by chance we can't get our executable path, default to 'ls'
		executable = "ls"
	} else {
		executable = exe
	}
}

type usrKey struct{}

type usr struct {
	uid            string
	uname          string
	isAdmin        bool
	inControlGroup bool
}

func contextWithUsr(ctx context.Context, u usr) context.Context {
	return context.WithValue(ctx, (*usrKey)(nil), u)
}

func usrFromContext(ctx context.Context) usr {
	u := ctx.Value((*usrKey)(nil))
	if u == nil {
		return usr{}
	}
	return u.(usr)
}

func canSudo(usr string) bool {
	if runtime.GOOS == "windows" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "sudo", "--other-user="+usr, "--list", executable).Run(); err != nil {
		return false
	}
	return true
}
