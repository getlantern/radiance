package issue

import (
	"archive/zip"
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getlantern/radiance/internal"
)

func init() {
	slog.SetDefault(internal.NoOpLogger())
}

func TestZipFilesWithoutPath(t *testing.T) {
	var buf bytes.Buffer
	err := zipFiles(&buf, zipOptions{Globs: map[string]string{"": "**/*.txt*"}})
	if !assert.NoError(t, err) {
		return
	}
	expectedFiles := []string{
		"test_data/hello.txt",
		"test_data/hello.txt.1",
		"test_data/large.txt",
		"test_data/zzzz.txt.2",
	}
	testZipFiles(t, buf.Bytes(), expectedFiles)
}

func TestZipFilesWithMaxBytes(t *testing.T) {
	var buf bytes.Buffer
	err := zipFiles(&buf,
		zipOptions{
			Globs:    map[string]string{"": "test_data/*.txt*"},
			MaxBytes: 1024, // 1KB
		},
	)
	if !assert.NoError(t, err) {
		return
	}
	expectedFiles := []string{
		"test_data/hello.txt",
		"test_data/hello.txt.1",
	}
	testZipFiles(t, buf.Bytes(), expectedFiles)
}

func TestZipFilesWithNewRoot(t *testing.T) {
	var buf bytes.Buffer
	err := zipFiles(&buf, zipOptions{Globs: map[string]string{"new_root": "**/*.txt*"}})
	if !assert.NoError(t, err) {
		return
	}
	expectedFiles := []string{
		"new_root/hello.txt",
		"new_root/hello.txt.1",
		"new_root/large.txt",
		"new_root/zzzz.txt.2",
	}
	testZipFiles(t, buf.Bytes(), expectedFiles)
}

func testZipFiles(t *testing.T, zipped []byte, expectedFiles []string) {
	reader, eread := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if !assert.NoError(t, eread) {
		return
	}
	if !assert.Equal(t, len(expectedFiles), len(reader.File), "should not include extra files and files that would exceed MaxBytes") {
		return
	}
	for idx, file := range reader.File {
		t.Log(file.Name)
		assert.Equal(t, expectedFiles[idx], file.Name)
		if !strings.Contains(file.Name, "hello.txt") {
			continue
		}
		fileReader, err := file.Open()
		if !assert.NoError(t, err) {
			return
		}
		defer fileReader.Close()
		actual, _ := io.ReadAll(fileReader)
		assert.Equal(t, []byte("world\n"), actual)
	}
}
