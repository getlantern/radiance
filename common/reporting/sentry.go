package reporting

import (
	"log/slog"
	"time"

	"github.com/getsentry/sentry-go"
)

func Init(version string) {
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              "https://f0b8f24478c68542e418ba644051ef56@o75725.ingest.us.sentry.io/4508853370093568",
		AttachStacktrace: true,
		Release:          version,
	})
	if err != nil {
		slog.Error("sentry.Init:", "error", err)
	}
}

func PanicListener(msg string) {
	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetLevel(sentry.LevelFatal)
	})

	sentry.CaptureMessage(msg)
	if result := sentry.Flush(6 * time.Second); !result {
		slog.Error("sentry.Flush: timeout")
	}
}
