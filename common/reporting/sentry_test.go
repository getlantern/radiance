package reporting

import (
	"errors"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

func TestInitSentry(t *testing.T) {
	// Call the function to test
	Init()

	defer sentry.Flush(2 * time.Second)
	sentry.CaptureException(errors.New("Hello, Sentry!"))
}
