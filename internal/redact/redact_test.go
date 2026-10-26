package redact

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const tag = "helper-0123456789ab"

func TestHelperRedactsTagsAndIPsOnHelperLines(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"ipv4 host:port", "outbound/samizdat[" + tag + "]: dial tcp 203.0.113.7:443: i/o timeout",
			"outbound/samizdat[helper]: dial tcp <redacted>: i/o timeout"},
		{"bare ipv4", "outbound/samizdat[" + tag + "]: no route to 203.0.113.7.",
			"outbound/samizdat[helper]: no route to <redacted>."},
		{"bracketed ipv6 host:port", "outbound/samizdat[" + tag + "]: dial tcp [2001:db8::7]:8443: refused",
			"outbound/samizdat[helper]: dial tcp <redacted>: refused"},
		{"bare ipv6", "outbound/samizdat[" + tag + "]: unreachable 2001:db8::7",
			"outbound/samizdat[helper]: unreachable <redacted>"},
		{"suffixed tag", tag + "_failed: dial tcp 203.0.113.7:443",
			"helper_failed: dial tcp <redacted>"},
		{"tunnel logger name", `msg="dial tcp 203.0.113.7:443: refused" tag=outbound/helper[samizdat]`,
			`msg="dial tcp <redacted>: refused" tag=outbound/helper[samizdat]`},
		{"tunnel endpoint logger name", `msg="handshake with [2001:db8::7]:51820 failed" tag=endpoint/helper[wireguard]`,
			`msg="handshake with <redacted> failed" tag=endpoint/helper[wireguard]`},
		{"already tag-redacted", "outbound/samizdat[helper]: dial tcp 203.0.113.7:443",
			"outbound/samizdat[helper]: dial tcp <redacted>"},
		{"timestamps survive", `time="2026-10-23 13:30:00.000 UTC" msg="outbound/samizdat[` + tag + `]: dial tcp 203.0.113.7:443"`,
			`time="2026-10-23 13:30:00.000 UTC" msg="outbound/samizdat[helper]: dial tcp <redacted>"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Helper(tc.in))
		})
	}
}

func TestHelperLeavesOtherLinesAlone(t *testing.T) {
	in := "outbound/samizdat[lantern-1]: dial tcp 198.51.100.9:443\n" +
		"outbound/samizdat[" + tag + "]: dial tcp 203.0.113.7:443\n" +
		"dial tcp [2001:db8::9]:443 ok\n"
	want := "outbound/samizdat[lantern-1]: dial tcp 198.51.100.9:443\n" +
		"outbound/samizdat[helper]: dial tcp <redacted>\n" +
		"dial tcp [2001:db8::9]:443 ok\n"
	assert.Equal(t, want, Helper(in))
	assert.Equal(t, []byte(want), HelperBytes([]byte(in)))
	plain := "nothing to see 198.51.100.9"
	assert.Equal(t, plain, Helper(plain))
}

func TestHelperRedactsEveryIPv6Form(t *testing.T) {
	const pre = "outbound/samizdat[" + tag + "]: "
	for _, tc := range []struct{ in, want string }{
		{"dial ::1", "dial <redacted>"},
		{"dial fe80::", "dial <redacted>"},
		{"dial 2001:db8::", "dial <redacted>"},
		{"dial 2001:db8:1234::", "dial <redacted>"},
		{"no route to 2001:db8::.", "no route to <redacted>."},
		{"dial tcp [2001:db8::]:443", "dial tcp <redacted>"},
		{"dial tcp [fe80::1%eth0]:443", "dial tcp <redacted>"},
		{"dial fe80::1%eth0, retrying", "dial <redacted>, retrying"},
		{"dial (2001:db8::7); retrying", "dial (<redacted>); retrying"},
		{"dial 203.0.113.7: refused", "dial <redacted>: refused"},
		{"at 12:34:56 and 13:30:00.000, step 1.2", "at 12:34:56 and 13:30:00.000, step 1.2"},
		{"code dead:beef ok", "code dead:beef ok"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, "outbound/samizdat[helper]: "+tc.want, Helper(pre+tc.in))
		})
	}
}
