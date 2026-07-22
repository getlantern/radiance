package main

import (
	"testing"

	"github.com/alexflint/go-arg"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
	commonenv "github.com/getlantern/radiance/common/env"
)

func TestDaemonEnvironmentArguments(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantRun     daemonEnvironment
		wantInstall daemonEnvironment
		wantErr     string
	}{
		{
			name:    "run defaults to production",
			args:    []string{"run"},
			wantRun: daemonEnvironmentProd,
		},
		{
			name:        "install defaults to production",
			args:        []string{"install"},
			wantInstall: daemonEnvironmentProd,
		},
		{
			name:    "run accepts staging",
			args:    []string{"run", "--environment", "staging"},
			wantRun: daemonEnvironmentStaging,
		},
		{
			name:        "install accepts staging",
			args:        []string{"install", "--environment", "staging"},
			wantInstall: daemonEnvironmentStaging,
		},
		{
			name:    "run rejects alias",
			args:    []string{"run", "--environment", "stage"},
			wantErr: `unsupported environment "stage"`,
		},
		{
			name:    "run rejects arbitrary value",
			args:    []string{"run", "--environment", "dev"},
			wantErr: `unsupported environment "dev"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var parsed daemonArgs
			parser, err := arg.NewParser(arg.Config{}, &parsed)
			require.NoError(t, err)
			err = parser.Parse(test.args)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			if parsed.Run != nil {
				require.Equal(t, test.wantRun, parsed.Run.Environment)
			}
			if parsed.Install != nil {
				require.Equal(t, test.wantInstall, parsed.Install.Environment)
			}
		})
	}
}

func TestServiceRunConfigPersistsEnvironment(t *testing.T) {
	want := serviceRunConfig{
		dataPath:    "/data",
		logPath:     "/logs",
		logLevel:    "debug",
		environment: daemonEnvironmentStaging,
	}

	args := want.args()
	require.Equal(t, []string{
		"run",
		"--data-path", "/data",
		"--log-path", "/logs",
		"--log-level", "debug",
		"--environment", "staging",
	}, args)

	got, err := parseServiceRunArgs(args)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

func TestServiceRunConfigDefaultsToProduction(t *testing.T) {
	config, err := parseServiceRunArgs([]string{"run"})
	require.NoError(t, err)
	require.Equal(t, daemonEnvironmentProd, config.environment)
	require.Equal(t, "info", config.logLevel)
}

func TestServiceRunConfigRejectsInvalidEnvironment(t *testing.T) {
	_, err := parseServiceRunArgs([]string{"run", "--environment", "development"})
	require.ErrorContains(t, err, `unsupported environment "development"`)

	_, err = parseServiceRunArgs([]string{"run", "--environment"})
	require.ErrorContains(t, err, "missing value for --environment")
}

func TestStagingServiceRunConfigSelectsStagingAccountEndpoints(t *testing.T) {
	installed := serviceRunConfig{
		dataPath:    "/data",
		logPath:     "/logs",
		logLevel:    "info",
		environment: daemonEnvironmentStaging,
	}
	parsed, err := parseServiceRunArgs(installed.args())
	require.NoError(t, err)

	options := daemonBackendOptions(parsed.dataPath, parsed.logPath, parsed.logLevel, parsed.environment)
	require.Equal(t, "staging", options.EnvOverrides[commonenv.ENV.String()])
	t.Setenv(commonenv.ENV.String(), options.EnvOverrides[commonenv.ENV.String()])
	require.Equal(t, common.StageBaseURL, common.GetBaseURL())
	require.Equal(t, common.StageProServerURL, common.GetProServerURL())

	authURL, proServerURL := daemonBackendURLs(parsed.environment)
	require.Equal(t, common.StageBaseURL, authURL)
	require.Equal(t, common.StageProServerURL, proServerURL)
}
