package memmon

// parseStatmRSS extracts the resident set size in bytes from the contents of
// /proc/self/statm. The second whitespace-separated field is the resident page
// count; the result is that count × pageSize. ok is false if the second field
// is missing or non-numeric.
//
// Kept pure so RSS parsing remains unit-testable on platforms without Android's
// /proc behavior.
func parseStatmRSS(data []byte, pageSize int) (uint64, bool) {
	i, n := 0, len(data)
	readField := func() (uint64, bool) {
		for i < n && data[i] == ' ' {
			i++
		}
		start := i
		var v uint64
		for i < n && data[i] >= '0' && data[i] <= '9' {
			v = v*10 + uint64(data[i]-'0')
			i++
		}
		return v, i > start
	}
	if _, ok := readField(); !ok { // field 0: total program size
		return 0, false
	}
	pages, ok := readField() // field 1: resident set size
	if !ok {
		return 0, false
	}
	return pages * uint64(pageSize), true
}
