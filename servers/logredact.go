package servers

import (
	"errors"
	"log/slog"
	"regexp"
)

var queryComponent = regexp.MustCompile(`\?\S*`)

// redactingLogger strips URL query components from string and error values,
// since private-server URLs carry the access token in the query and
// go-retryablehttp redacts only userinfo passwords.
type redactingLogger struct {
	logger *slog.Logger
}

func (l redactingLogger) Error(msg string, keysAndValues ...any) {
	l.logger.Error(msg, redactQueries(keysAndValues)...)
}

func (l redactingLogger) Warn(msg string, keysAndValues ...any) {
	l.logger.Warn(msg, redactQueries(keysAndValues)...)
}

func (l redactingLogger) Info(msg string, keysAndValues ...any) {
	l.logger.Info(msg, redactQueries(keysAndValues)...)
}

func (l redactingLogger) Debug(msg string, keysAndValues ...any) {
	l.logger.Debug(msg, redactQueries(keysAndValues)...)
}

func redactQueries(values []any) []any {
	redacted := make([]any, len(values))
	for i, value := range values {
		switch v := value.(type) {
		case string:
			redacted[i] = stripQueries(v)
		case error:
			redacted[i] = stripQueries(v.Error())
		default:
			redacted[i] = value
		}
	}
	return redacted
}

func stripQueries(s string) string {
	return queryComponent.ReplaceAllString(s, "")
}

// redactError returns an error carrying err's message with URL query components
// removed. The result does not wrap err, whose chain may still carry the access
// token.
func redactError(err error) error {
	return errors.New(stripQueries(err.Error()))
}
