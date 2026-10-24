// Package redact removes what a client must not export about its helper
// pairings from text it logs, uploads or reports: helper- outbound tags, which
// are client-side pseudonyms for pairings, and the addresses of the helpers
// those outbounds dial.
package redact

import (
	"bytes"
	"net"
	"regexp"
	"strings"

	lbA "github.com/getlantern/lantern-box/adapter"
)

// Redacted replaces an address on a line that names a helper outbound.
const Redacted = "<redacted>"

var (
	// helperNamed matches a helper outbound named in a line: its tag, or the
	// "[helper]" an earlier redaction left of "outbound/<type>[helper-…]".
	helperNamed = regexp.MustCompile(`\b` + lbA.HelperTagPrefix + `[0-9a-f]{12}\b|\[helper\]`)
	// ipCandidate matches text that may be an IP address, with or without a
	// port: a bracketed host, or a run of hex digits, dots and colons. Each
	// match is checked with net.ParseIP before it is replaced.
	ipCandidate = regexp.MustCompile(`\[[0-9A-Fa-f:.]+(?:%[0-9A-Za-z_.\-]+)?\](?::[0-9]{1,5})?|[0-9A-Fa-f]*[.:][0-9A-Fa-f.:]*(?:%[0-9A-Za-z_.\-]+)?`)
)

// Helper returns s with helper tags replaced by "helper" and, on every line
// that names a helper outbound, every IP address (with any port) replaced by
// Redacted. Lines that name no helper outbound keep their addresses.
func Helper(s string) string {
	if !strings.Contains(s, "helper") {
		return s
	}
	if helperNamed.MatchString(s) {
		lines := strings.SplitAfter(s, "\n")
		for i, line := range lines {
			if helperNamed.MatchString(line) {
				lines[i] = ipCandidate.ReplaceAllStringFunc(line, redactIP)
			}
		}
		s = strings.Join(lines, "")
	}
	return lbA.RedactHelperTags(s)
}

// HelperBytes is Helper for a byte slice. It returns b itself when there is
// nothing to redact.
func HelperBytes(b []byte) []byte {
	if !bytes.Contains(b, []byte("helper")) {
		return b
	}
	return []byte(Helper(string(b)))
}

// redactIP replaces m with Redacted if m is an IP address, or is one followed
// by trailing dots or colons, which the candidate pattern takes in from the
// surrounding text ("… to 2001:db8::." or "203.0.113.7: refused"). m is
// checked whole first, since "::" can end a valid IPv6 address; then one
// trailing character at a time is set aside and the rest checked again.
// Other punctuation never enters a candidate.
func redactIP(m string) string {
	for core := m; core != ""; core = core[:len(core)-1] {
		if isIPAddr(core) {
			return Redacted + m[len(core):]
		}
		if last := core[len(core)-1]; last != '.' && last != ':' {
			return m
		}
	}
	return m
}

// isIPAddr reports whether s is an IP address, optionally bracketed, zoned or
// followed by a port.
func isIPAddr(s string) bool {
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return false
		}
		return parseIP(s[1:end])
	}
	if parseIP(s) {
		return true
	}
	host, port, err := net.SplitHostPort(s)
	return err == nil && port != "" && parseIP(host)
}

func parseIP(s string) bool {
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	return net.ParseIP(s) != nil
}
