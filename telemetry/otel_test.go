package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/attribute"
)

func TestBuildResourcesIdentifiesTheBuild(t *testing.T) {
	res := attribute.NewSet(buildResources("radiance", Attributes{
		AppVersion:  "10.0.3",
		BuildCommit: "25a8d99",
		BuildTime:   "2026-10-18T22:30:00Z",
	})...)
	for key, want := range map[attribute.Key]string{
		"service.version":       "10.0.3",
		"vcs.ref.head.revision": "25a8d99",
		"build.time":            "2026-10-18T22:30:00Z",
	} {
		got, ok := res.Value(key)
		assert.True(t, ok, "missing %s", key)
		assert.Equal(t, want, got.AsString(), key)
	}
}
