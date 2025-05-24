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
	log := NewRadLogger(logger)
	log.Info("")
	log.Info("one")
	log.Info("one", "two")
	log.Info("one", "two", "three")
	log.Info("one", "two", "three", "four")
}
