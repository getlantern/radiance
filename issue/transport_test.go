package issue

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateFirstClassAttachmentsCombinedBudget(t *testing.T) {
	pngHeader := []byte("\x89PNG\r\n\x1a\n")

	makeScreenshot := func(name string, size int) *Attachment {
		data := make([]byte, size)
		copy(data, pngHeader)
		return &Attachment{Name: name, Type: "image/png", Data: data}
	}

	t.Run("screenshot fits under combined cap", func(t *testing.T) {
		err := validateFirstClassAttachments(
			[]*Attachment{makeScreenshot("s.png", 1024)},
			MaxAttachmentBytes-2048,
		)
		require.NoError(t, err)
	})

	t.Run("screenshot pushes total over cap", func(t *testing.T) {
		err := validateFirstClassAttachments(
			[]*Attachment{makeScreenshot("s.png", 2048)},
			MaxAttachmentBytes-1024,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "total attachment size exceeds")
	})

	t.Run("existingBytes alone already at cap rejects any screenshot", func(t *testing.T) {
		err := validateFirstClassAttachments(
			[]*Attachment{makeScreenshot("s.png", 1)},
			MaxAttachmentBytes,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "total attachment size exceeds")
	})

	t.Run("no existing bytes; screenshot within screenshot-only slice", func(t *testing.T) {
		err := validateFirstClassAttachments(
			[]*Attachment{makeScreenshot("s.png", 1024)},
			0,
		)
		require.NoError(t, err)
	})

	t.Run("screenshot count cap still enforced", func(t *testing.T) {
		screenshots := []*Attachment{
			makeScreenshot("a.png", 1),
			makeScreenshot("b.png", 1),
			makeScreenshot("c.png", 1),
			makeScreenshot("d.png", 1),
		}
		err := validateFirstClassAttachments(screenshots, 0)
		require.Error(t, err)
		assert.True(t,
			strings.Contains(err.Error(), "too many screenshot attachments"),
			"expected count-cap error, got %q", err.Error(),
		)
	})
}
