package common

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common/env"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/internal"
	"github.com/getlantern/radiance/log"
)

func TestLoggerPathUsesLogFileInDirectory(t *testing.T) {
	t.Setenv(env.LogToStdout.String(), "false")
	logs := t.TempDir()
	require.Equal(t, filepath.Join(logs, internal.LogFileName), loggerPath(logs))
}

func TestLoggerPathRedirectsToStdout(t *testing.T) {
	t.Setenv(env.LogToStdout.String(), "true")
	require.Equal(t, log.StdoutPath, loggerPath(t.TempDir()))
}

func TestWriteRecoveredPanic_NoLogDir(t *testing.T) {
	t.Cleanup(settings.Reset)
	settings.Reset()
	t.Chdir(t.TempDir())

	func() {
		defer func() { WriteRecoveredPanic("testing", recover()) }()
		panic("boom")
	}()
	assert.NoFileExists(t, internal.CrashLogFileName)
}
