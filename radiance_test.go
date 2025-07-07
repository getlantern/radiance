package radiance

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRadiance(t *testing.T) {
	t.Run("it should create a new Radiance instance successfully", func(t *testing.T) {
		dir := t.TempDir()
		r, err := NewRadiance(&Options{
			DataDir: dir,
			Locale:  "en-US",
		})
		assert.NoError(t, err)
		defer r.Close()
		assert.NotNil(t, r)
		assert.NotNil(t, r.confHandler)
		assert.NotNil(t, r.stopChan)
		assert.NotNil(t, r.issueReporter)
	})
}

func TestReportIssue(t *testing.T) {
	var tests = []struct {
		name   string
		email  string
		report IssueReport
		assert func(*testing.T, error)
	}{
		{
			name:   "it should return error when issue report is missing both type and description",
			email:  "",
			report: IssueReport{},
			assert: func(t *testing.T, err error) {
				assert.Error(t, err)
			},
		},
		{
			name:  "it should return nil when issue report is valid",
			email: "radiancetest@getlantern.org",
			report: IssueReport{
				Type:        "Application crashes",
				Description: "internal test only",
				Device:      "test device",
				Model:       "a123",
			},
			assert: func(t *testing.T, err error) {
				assert.NoError(t, err)
			},
		},
		{
			name:  "it should return nil when issue report is valid with empty email",
			email: "",
			report: IssueReport{
				Type:        "Cannot sign in",
				Description: "internal test only",
				Device:      "test device 2",
				Model:       "b456",
			},
			assert: func(t *testing.T, err error) {
				assert.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewRadiance(&Options{DataDir: t.TempDir()})
			defer r.Close()
			require.NoError(t, err)
			err = r.ReportIssue(tt.email, &tt.report)
			tt.assert(t, err)
		})
	}
}
