// Package ops wraps github.com/getlantern/ops with convenience methods
// for radiance
package ops

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/getlantern/jibber_jabber"
	"github.com/getlantern/ops"
	"github.com/getlantern/osversion"
)

type contextKey string

const (
	ctxKeyOp = contextKey("op")
)

// Op decorates an ops.Op with convenience methods.
type Op struct {
	wrapped ops.Op
}

func (op *Op) Wrapped() ops.Op {
	return op.wrapped
}

// Begin mimics the similar method from ops.Op
func (op *Op) Begin(name string) *Op {
	return &Op{op.wrapped.Begin(name)}
}

// Begin mimics the similar method from ops
func Begin(name string) *Op {
	return &Op{ops.Begin(name)}
}

// BeginCtx mimics the similar method from ops and add to the context
func BeginCtx(ctx context.Context, name string) (context.Context, *Op) {
	op := &Op{ops.Begin(name)}
	return WithOp(ctx, op), op
}

// WithOp adds an Op to the context to be retrieved later via FromContext
func WithOp(ctx context.Context, op *Op) context.Context {
	return context.WithValue(ctx, ctxKeyOp, op)
}

// FromContext extracts an Op from the context, or nil if doesn't exist
func FromContext(ctx context.Context) *Op {
	if op, exist := ctx.Value(ctxKeyOp).(*Op); exist {
		return op
	}
	return nil
}

// RegisterReporter mimics the similar method from ops
func RegisterReporter(reporter ops.Reporter) {
	ops.RegisterReporter(reporter)
}

// Go mimics the similar method from ops.Op
func (op *Op) Go(fn func()) {
	op.wrapped.Go(fn)
}

// Go mimics the similar method from ops.
func Go(fn func()) {
	ops.Go(fn)
}

// Cancel mimics the similar method from ops.Op
func (op *Op) Cancel() {
	op.wrapped.Cancel()
}

// End mimics the similar method from ops.Op
func (op *Op) End() {
	op.wrapped.End()
}

// Set mimics the similar method from ops.Op
func (op *Op) Set(key string, value any) *Op {
	op.wrapped.Set(key, value)
	return op
}

// SetGlobalDynamic mimics the similar method from ops
func SetGlobalDynamic(key string, valueFN func() any) {
	ops.SetGlobalDynamic(key, valueFN)
}

// FailIf mimics the similar method from ops.op
func (op *Op) FailIf(err error) error {
	return op.wrapped.FailIf(err)
}

// InitGlobalContext configures global context info
func InitGlobalContext(appName, applicationVersion, platform, revisionDate, deviceID string, isPro func() bool, getCountry func() string) {
	// Using "application" allows us to distinguish between errors from the
	// lantern client vs other sources like the http-proxy, etop.
	ops.SetGlobal("app", fmt.Sprintf("%s-client", strings.ToLower(appName)))
	ops.SetGlobal("app_version", fmt.Sprintf("%v (%v)", applicationVersion, revisionDate))
	ops.SetGlobal("go_version", runtime.Version())
	ops.SetGlobal("os_name", platform)
	ops.SetGlobal("os_arch", runtime.GOARCH)
	ops.SetGlobal("device_id", deviceID)
	ops.SetGlobalDynamic("geo_country", func() any { return getCountry() })
	ops.SetGlobalDynamic("timezone", func() any { return time.Now().Format("MST") })
	ops.SetGlobalDynamic("locale_language", func() any {
		lang, _ := jibber_jabber.DetectLanguage()
		return lang
	})
	ops.SetGlobalDynamic("locale_country", func() any {
		country, _ := jibber_jabber.DetectTerritory()
		return country
	})
	ops.SetGlobalDynamic("is_pro", func() any {
		return isPro()
	})
	// still need to find a way for data cap
	// ops.SetGlobalDynamic("is_data_capped", func() interface{} {
	// 	if isPro() {
	// 		return false
	// 	}
	// 	quota, _ := bandwidth.GetQuota()
	// 	return quota != nil && quota.MiBUsed >= quota.MiBAllowed
	// })

	if osStr, err := osversion.GetHumanReadable(); err == nil {
		ops.SetGlobal("os_version", osStr)
	}
}
