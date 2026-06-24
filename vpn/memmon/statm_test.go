package memmon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseStatmRSS(t *testing.T) {
	const pageSize = 4096
	tests := []struct {
		name     string
		data     string
		pageSize int
		want     uint64
		wantOK   bool
	}{
		{name: "typical line", data: "1234 567 89 1 0 100 0", pageSize: pageSize, want: 567 * pageSize, wantOK: true},
		{name: "only two fields", data: "1234 567", pageSize: pageSize, want: 567 * pageSize, wantOK: true},
		{name: "leading spaces", data: "   1234 567 89", pageSize: pageSize, want: 567 * pageSize, wantOK: true},
		{name: "multiple inner spaces", data: "1234   567   89", pageSize: pageSize, want: 567 * pageSize, wantOK: true},
		{name: "trailing newline", data: "1234 567 89\n", pageSize: pageSize, want: 567 * pageSize, wantOK: true},
		{name: "zero resident pages is valid", data: "1234 0 89", pageSize: pageSize, want: 0, wantOK: true},
		{name: "non-default page size multiplies", data: "1234 10 5", pageSize: 16384, want: 10 * 16384, wantOK: true},
		{name: "empty", data: "", pageSize: pageSize, want: 0, wantOK: false},
		{name: "only spaces", data: "   ", pageSize: pageSize, want: 0, wantOK: false},
		{name: "resident field missing", data: "1234", pageSize: pageSize, want: 0, wantOK: false},
		{name: "resident field missing with trailing space", data: "1234 ", pageSize: pageSize, want: 0, wantOK: false},
		{name: "total field non-numeric", data: "abc 567", pageSize: pageSize, want: 0, wantOK: false},
		{name: "resident field non-numeric", data: "1234 abc", pageSize: pageSize, want: 0, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseStatmRSS([]byte(tt.data), tt.pageSize)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
