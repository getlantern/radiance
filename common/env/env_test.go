package env

import (
	"testing"
)

// TestSetStagingEnv_DoesNotOverrideExisting guards the contract
// spelled out in SetStagingEnv's docstring: the Flutter UI's persisted
// `environment` app-setting (which triggers SetStagingEnv) must not
// override a developer's explicit shell RADIANCE_ENV. Without this
// guard, a CLI override like `RADIANCE_ENV=staging Lantern` ends up
// silently wiped when Flutter later passes its persisted env through
// SetStagingEnv — making it near-impossible to run a staging-pointed
// desktop client when the last GUI session persisted "prod".
func TestSetStagingEnv_DoesNotOverrideExisting(t *testing.T) {
	saved := envVars
	defer func() { envVars = saved }()

	envVars = map[string]any{ENV: "prod"}
	SetStagingEnv()
	if got := envVars[ENV]; got != "prod" {
		t.Fatalf("RADIANCE_ENV=prod should be preserved, got %v", got)
	}
	// PrintCurl is an instrumentation side-effect that's safe to set
	// regardless — assert the helper still primes it for dev ergonomics.
	if got, _ := envVars[PrintCurl].(bool); !got {
		t.Fatalf("PrintCurl should be enabled by SetStagingEnv, got %v", envVars[PrintCurl])
	}
}

// TestSetStagingEnv_SetsWhenUnset covers the original happy path: if
// nothing has set RADIANCE_ENV yet (no shell export, no .env file), a
// Flutter opts.Env="staging" should still result in staging mode.
func TestSetStagingEnv_SetsWhenUnset(t *testing.T) {
	saved := envVars
	defer func() { envVars = saved }()

	envVars = map[string]any{}
	SetStagingEnv()
	if got := envVars[ENV]; got != "staging" {
		t.Fatalf("expected staging, got %v", got)
	}
}
