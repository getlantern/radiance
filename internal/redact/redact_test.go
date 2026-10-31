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
		{"redacted outbound field", "outbound attempt failed outbound helper attempt 1 error dial tcp 203.0.113.7:443",
			"outbound attempt failed outbound helper attempt 1 error dial tcp <redacted>"},
		{"redacted suffixed tag", "helper_failed: dial tcp 203.0.113.7:443", "helper_failed: dial tcp <redacted>"},
		{"ipv4 after a colon", "helper peer:203.0.113.7:443 refused", "helper peer:<redacted> refused"},
		{"ipv6 after a colon", "helper peer:2001:db8::7 refused", "helper peer:<redacted> refused"},
		{"loopback ipv6 whole", "helper dial [::1]:443 and ::1", "helper dial <redacted> and <redacted>"},
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

func TestHelperRedactsThePersonalProxiesTag(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"group logger", "outbound/mutableurltest[personal-proxies]: dial tcp 203.0.113.7:443: refused",
			"outbound/mutableurltest[helper]: dial tcp <redacted>: refused"},
		{"selected outbound", `selected outbound="personal-proxies"`, `selected outbound="helper"`},
		{"urltest attribute", "urltest_group=personal-proxies", "urltest_group=helper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Helper(tc.in))
			assert.Equal(t, []byte(tc.want), HelperBytes([]byte(tc.in)))
		})
	}
	assert.True(t, NamesHelper(PersonalProxiesTag))
}

func TestHelperRedactsThePersonalProxyTag(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"group logger", "outbound/mutableurltest[personal-proxy]: dial tcp 203.0.113.7:443: refused",
			"outbound/mutableurltest[helper]: dial tcp <redacted>: refused"},
		{"selection", "Selected server tag=personal-proxy:helper-0123456789ab", "Selected server tag=helper:helper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Helper(tc.in))
			assert.Equal(t, []byte(tc.want), HelperBytes([]byte(tc.in)))
		})
	}
	assert.True(t, NamesHelper(PersonalProxyTag))
	assert.Equal(t, "helper", Tag(PersonalProxyTag))
	assert.Equal(t, "helper / helper", Tag("personal-proxy / personal-proxies"))
}

func TestTagRedactsHelperAndGroupTags(t *testing.T) {
	assert.Equal(t, "helper", Tag(PersonalProxiesTag))
	assert.Equal(t, "helper", Tag(tag))
	assert.Equal(t, "final => helper", Tag("final => personal-proxies"))
	assert.Equal(t, "lantern-1", Tag("lantern-1"))
	assert.Equal(t, "dial 203.0.113.7", Tag("dial 203.0.113.7"), "Tag leaves addresses alone")
}
