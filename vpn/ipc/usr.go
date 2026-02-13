package ipc

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

type usrKey struct{}

type usr struct {
	uid     string
	uname   string
	isAdmin bool
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
	if err := exec.CommandContext(ctx, "sudo", "-n", "--other-user="+usr, "--list").Run(); err != nil {
		return false
	}
	return true
}
