package ipc

import (
	"context"
	"log/slog"
	"os/exec"
	"runtime"
	"time"

	"github.com/getlantern/radiance/internal"
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
	slog.Log(nil, internal.LevelTrace, "Checking sudo access for user", "user", usr)
	if err := exec.CommandContext(ctx, "sudo", "-n", "--other-user="+usr, "--list").Run(); err != nil {
		return false
	}
	return true
}

// func ContextWithID(ctx context.Context, id ID) context.Context {
// 	return context.WithValue(ctx, (*idKey)(nil), id)
// }
//
// func IDFromContext(ctx context.Context) (ID, bool) {
// 	id, loaded := ctx.Value((*idKey)(nil)).(ID)
// 	return id, loaded
// }
//
