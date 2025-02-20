package sentry

import (
	"time"

	"github.com/getlantern/golog"
	"github.com/getlantern/radiance/common"
	"github.com/getsentry/sentry-go"
)

var log = golog.LoggerFor("sentry")

func InitSentry() {
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              "https://f0b8f24478c68542e418ba644051ef56@o75725.ingest.us.sentry.io/4508853370093568",
		AttachStacktrace: true,
		Release:          common.Version,
	})
	if err != nil {
		log.Fatalf("sentry.Init: %s", err)
	}
}

func PanicListener(msg string) {
	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetLevel(sentry.LevelFatal)
	})

	sentry.CaptureMessage(msg)
	if result := sentry.Flush(6 * time.Second); !result {
		log.Error("sentry.Flush: timeout")
	}
}
