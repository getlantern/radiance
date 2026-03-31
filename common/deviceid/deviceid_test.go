package deviceid

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGet(t *testing.T) {
	tmp := t.TempDir()
	id1 := Get(tmp)
	require.True(t, len(id1) > 8)
	id2 := Get(tmp)
	require.Equal(t, id1, id2)
}
