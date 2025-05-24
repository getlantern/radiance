package radiance

import (
	"log/slog"
	"os"
	"testing"
)

var logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
	AddSource: true,
	Level:     slog.LevelDebug,
}))

func TestStringify(t *testing.T) {
	cases := []any{
		"string",
		1,
		false,
		struct{}{},
		[]string{"a", "b"},
		[]string{},
		nil,
	}
	for _, c := range cases {
		s := stringify(c)
		t.Logf("Stringify(%v)\n  %s", c, s)
	}
}

func TestLog(t *testing.T) {
	slog.SetDefault(logger)
	Info("")
	Info("one")
	Info("one", "two")
	Info("one", "two", "three", "four")
}
