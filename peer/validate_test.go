package peer

import (
	"strings"
	"testing"
)

// minimalValidLaunchCfg returns a launch_cfg JSON that passes
// validateAbuseRules: the four abuse rule_set tags from
// lantern-cloud's samizdat.go (each as a "remote" rule_set + a
// matching reject rule), plus one RFC1918 and one SMTP canary in
// reject rules. Shared by peer_test.go's stubServer so the existing
// Start-path tests do not regress on the new check.
const minimalValidLaunchCfg = `{
	"inbounds":[{"type":"samizdat","tag":"samizdat-in"}],
	"route":{
		"rule_set":[
			{"type":"remote","tag":"geosite-malware","format":"binary","url":"https://example/geosite-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geoip-malware","format":"binary","url":"https://example/geoip-malware.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},
			{"type":"remote","tag":"geosite-cryptominers","format":"binary","url":"https://example/geosite-cryptominers.srs","download_detour":"direct"}
		],
		"rules":[
			{"action":"reject","rule_set":["geosite-malware"]},
			{"action":"reject","rule_set":["geoip-malware"]},
			{"action":"reject","rule_set":["geosite-phishing"]},
			{"action":"reject","rule_set":["geosite-cryptominers"]},
			{"action":"reject","ip_cidr":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","127.0.0.0/8","169.254.0.0/16","::1/128","fc00::/7","fe80::/10"]},
			{"action":"reject","port":[25,465,587,2525,6660,6661,6662,6663,6664,6665,6666,6667,6668,6669,6697]}
		]
	}
}`

func TestValidateAbuseRules_HappyPath(t *testing.T) {
	if err := validateAbuseRules(minimalValidLaunchCfg); err != nil {
		t.Fatalf("expected canonical launch_cfg to pass, got: %v", err)
	}
}

func TestValidateAbuseRules_NestedDefaultForm(t *testing.T) {
	// sing-box may marshal "default" rules either inlined or nested
	// under a "default" key. validateAbuseRules must accept both —
	// otherwise a future libbox version change would silently break
	// the check.
	const nested = `{
		"route":{
			"rule_set":[
				{"type":"remote","tag":"geosite-malware"},
				{"type":"remote","tag":"geoip-malware"},
				{"type":"remote","tag":"geosite-phishing"},
				{"type":"remote","tag":"geosite-cryptominers"}
			],
			"rules":[
				{"type":"default","default":{"action":"reject","rule_set":["geosite-malware"]}},
				{"type":"default","default":{"action":"reject","rule_set":["geoip-malware"]}},
				{"type":"default","default":{"action":"reject","rule_set":["geosite-phishing"]}},
				{"type":"default","default":{"action":"reject","rule_set":["geosite-cryptominers"]}},
				{"type":"default","default":{"action":"reject","ip_cidr":["10.0.0.0/8"]}},
				{"type":"default","default":{"action":"reject","port":[25]}}
			]
		}
	}`
	if err := validateAbuseRules(nested); err != nil {
		t.Fatalf("nested default form should pass, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingRouteBlock(t *testing.T) {
	err := validateAbuseRules(`{"inbounds":[]}`)
	if err == nil {
		t.Fatal("expected error when route block is absent")
	}
	if !strings.Contains(err.Error(), "route block") {
		t.Errorf("error should mention route block, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingRuleSetTag(t *testing.T) {
	// Drop geosite-phishing from the rule_set list. The reject rule
	// for it can stay; the check should still flag the missing tag
	// because the reject is a no-op without the rule_set.
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"type":"remote","tag":"geosite-phishing","format":"binary","url":"https://example/geosite-phishing.srs","download_detour":"direct"},`,
		``, 1)
	err := validateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when an abuse tag is missing from route.rule_set")
	}
	if !strings.Contains(err.Error(), "geosite-phishing") {
		t.Errorf("error should name the missing tag, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingRejectRule(t *testing.T) {
	// Keep all rule_sets but drop one reject rule. sing-box will
	// download the list but never enforce it.
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"action":"reject","rule_set":["geosite-cryptominers"]},`,
		``, 1)
	err := validateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when an abuse tag has no reject rule")
	}
	if !strings.Contains(err.Error(), "geosite-cryptominers") {
		t.Errorf("error should name the unrejected tag, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingRFC1918Canary(t *testing.T) {
	// Strip the RFC1918 reject rule. SMTP block stays — we want to
	// see the RFC1918-specific error message.
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"action":"reject","ip_cidr":["10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","127.0.0.0/8","169.254.0.0/16","::1/128","fc00::/7","fe80::/10"]},`,
		``, 1)
	err := validateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when RFC1918 reject is missing")
	}
	if !strings.Contains(err.Error(), "RFC1918") {
		t.Errorf("error should mention RFC1918, got: %v", err)
	}
}

func TestValidateAbuseRules_MissingSMTPCanary(t *testing.T) {
	// Drop the SMTP/IRC port-reject rule. Removes the preceding
	// comma too so the resulting JSON is still well-formed (the
	// port-reject is the last entry in the rules array).
	bad := strings.Replace(minimalValidLaunchCfg,
		`,
			{"action":"reject","port":[25,465,587,2525,6660,6661,6662,6663,6664,6665,6666,6667,6668,6669,6697]}`,
		``, 1)
	err := validateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when SMTP port reject is missing")
	}
	if !strings.Contains(err.Error(), "SMTP") {
		t.Errorf("error should mention SMTP, got: %v", err)
	}
}

func TestValidateAbuseRules_NonRejectRulesIgnored(t *testing.T) {
	// A rule_set with action "route" (not reject) should NOT count
	// — sing-box would forward those flows to a named outbound
	// instead of dropping them. validateAbuseRules must demand the
	// reject action specifically.
	bad := strings.Replace(minimalValidLaunchCfg,
		`{"action":"reject","rule_set":["geosite-malware"]},`,
		`{"action":"route","outbound":"direct","rule_set":["geosite-malware"]},`, 1)
	err := validateAbuseRules(bad)
	if err == nil {
		t.Fatal("expected error when abuse tag has 'route' action instead of 'reject'")
	}
	if !strings.Contains(err.Error(), "geosite-malware") {
		t.Errorf("error should name the wrongly-actioned tag, got: %v", err)
	}
}

func TestValidateAbuseRules_AllErrorsReported(t *testing.T) {
	// errors.Join means a thoroughly-broken config should surface
	// all the missing pieces in one report. Operators triaging
	// "why is my peer refusing to start?" deserve a complete
	// picture, not a fix-one-thing-find-the-next loop.
	err := validateAbuseRules(`{"route":{}}`)
	if err == nil {
		t.Fatal("expected error for empty route block")
	}
	msg := err.Error()
	for _, want := range []string{"abuse tags", "RFC1918", "SMTP"} {
		if !strings.Contains(msg, want) {
			t.Errorf("combined error should mention %q, got: %v", want, err)
		}
	}
}

func TestValidateAbuseRules_BadJSON(t *testing.T) {
	err := validateAbuseRules(`{not valid json`)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "parse launch_cfg JSON") {
		t.Errorf("error should mention JSON parse failure, got: %v", err)
	}
}
