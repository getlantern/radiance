package log

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoggerLevelAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: LevelTrace, ReplaceAttr: replaceLogAttr}))

	for _, tt := range []struct {
		name  string
		value any
		want  string
	}{
		{"string", "user-level", "level=user-level"},
		{"integer", 7, "level=7"},
		{"boolean", true, "level=true"},
		{"nil", nil, "level=<nil>"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			require.NotPanics(t, func() {
				logger.Info(tt.name, "level", tt.value)
				logger.With("level", tt.value).Info(tt.name)
				logger.WithGroup("user").Info(tt.name, "level", tt.value)
			})
			lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
			require.Len(t, lines, 3)
			assert.Contains(t, lines[0], "attr."+tt.want)
			assert.Contains(t, lines[1], "attr."+tt.want)
			assert.Contains(t, lines[2], "user."+tt.want)
			for _, line := range lines {
				assert.Equal(t, 1, strings.Count(" "+line, " level="), line)
			}
		})
	}
	buf.Reset()
	for _, level := range []slog.Level{LevelTrace, LevelDebug, LevelInfo, LevelWarn, LevelError, LevelFatal, LevelPanic} {
		logger.Log(t.Context(), level, "built-in")
	}
	for _, name := range []string{"TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL", "PANIC"} {
		assert.Contains(t, buf.String(), "level="+name+" msg=built-in")
	}
}

func TestJSONLoggerLevelAttributes(t *testing.T) {
	for _, value := range []any{"user-level", 7, true, nil} {
		var buf bytes.Buffer
		logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: replaceLogAttr}))
		logger.Info("direct", "level", value)
		logger.With("level", value).Info("with")
		logger.Info("inline", slog.Group("", slog.Any("level", value)))
		logger.WithGroup("user").Info("grouped", "level", value)

		want, err := json.Marshal(value)
		require.NoError(t, err)
		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		require.Len(t, lines, 4)
		for _, line := range lines[:3] {
			assert.Equal(t, 1, strings.Count(line, `"level":`), line)
			var record map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(line), &record))
			assert.JSONEq(t, `"INFO"`, string(record["level"]))
			assert.JSONEq(t, string(want), string(record["attr.level"]))
		}
		var grouped struct {
			Level string `json:"level"`
			User  struct {
				Level json.RawMessage `json:"level"`
			} `json:"user"`
		}
		require.NoError(t, json.Unmarshal([]byte(lines[3]), &grouped))
		assert.Equal(t, "INFO", grouped.Level)
		assert.JSONEq(t, string(want), string(grouped.User.Level))
		assert.NotContains(t, lines[3], `"attr.level":`)
	}
}
