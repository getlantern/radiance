package env

import (
	"os"
	"testing"
)

// TestGet_OSEnvWinsOverDotenv guards the precedence promised by the
// package docstring: OS env > .env / runtime Set values.
//
// The regression this catches: the Flutter UI persists its current
// environment as an app-setting and passes it to lantern-core on every
// launch. When persisted = "staging", core calls SetStagingEnv which
// writes dotenv[RADIANCE_ENV]="staging". If a developer starts the app
// with `RADIANCE_ENV=prod Lantern` expecting prod, the dotenv value
// silently wins without this fix — making it near-impossible to point
// a desktop client at a different env than the last GUI session used.
func TestGet_OSEnvWinsOverDotenv(t *testing.T) {
	saved := cloneDotenv()
	defer restoreDotenv(saved)

	t.Setenv(ENV.String(), "prod")
	Set(ENV.String(), "staging")

	got, ok := Get(ENV)
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got != "prod" {
		t.Fatalf("OS env should win; got %q, want %q", got, "prod")
	}
}

// TestGet_DotenvFallsBackWhenOSUnset documents the other half of the
// contract: Set / .env values are still consulted for keys the shell
// hasn't explicitly set, so runtime instrumentation like SetStagingEnv
// keeps working for users who don't export anything themselves.
func TestGet_DotenvFallsBackWhenOSUnset(t *testing.T) {
	saved := cloneDotenv()
	defer restoreDotenv(saved)

	// Use a test-only key to avoid colliding with anything the init
	// loop may have read from .env or inherited from the process env.
	const testKey = "RADIANCE_UNIT_TEST_KEY_DOES_NOT_EXIST"
	_ = os.Unsetenv(testKey) // make absolutely sure OS doesn't have it

	Set(testKey, "from-dotenv")
	got, ok := Get(_key(testKey))
	if !ok {
		t.Fatal("Get returned ok=false when only dotenv had the value")
	}
	if got != "from-dotenv" {
		t.Fatalf("dotenv should be used when OS env unset; got %q", got)
	}
}

func cloneDotenv() map[string]string {
	mu.RLock()
	defer mu.RUnlock()
	out := make(map[string]string, len(dotenv))
	for k, v := range dotenv {
		out[k] = v
	}
	return out
}

func restoreDotenv(m map[string]string) {
	mu.Lock()
	defer mu.Unlock()
	dotenv = m
}
