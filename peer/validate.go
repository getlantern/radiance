package peer

import (
	"encoding/json"
	"errors"
	"fmt"
)

// abuseRuleSetTags is the canonical list of abuse rule_set tags that the
// peer launch_cfg MUST carry. Mirrors abuseTags in
// lantern-cloud/cmd/api/pcfg/samizdat.go. If samizdat.go grows or
// renames a tag, this list grows with it — the test in
// lantern-cloud asserts the server side; this list asserts the client
// side sees the same shape after registration.
var abuseRuleSetTags = []string{
	"geosite-malware",
	"geoip-malware",
	"geosite-phishing",
	"geosite-cryptominers",
}

// rfc1918CanaryCIDR and smtpCanaryPort are sentinel values that, if
// missing from the launch_cfg's reject rules, indicate the static
// peerEgressBlockRules block in samizdat.go was dropped or mutated.
// We pick one IP-CIDR and one port from each block as a low-cost smoke
// test; a full structural check would be brittle to upstream additions.
const (
	rfc1918CanaryCIDR = "10.0.0.0/8"
	smtpCanaryPort    = float64(25)
)

// validateAbuseRules is a defence-in-depth check on the sing-box
// options returned by /v1/peer/register. The server is supposed to
// embed a set of route.rule_set + route.rules entries that block the
// peer from forwarding traffic to known-malicious destinations,
// RFC1918 CIDRs, and abuse-prone ports. Those rules live in
// lantern-cloud/cmd/api/pcfg/samizdat.go.
//
// If a future regression in that server-side file ships a launch_cfg
// without those rules, every newly-registered peer would silently turn
// into an open residential proxy until someone noticed. This validator
// blocks Start before libbox runs an unsafe config; the peer prefers
// to fail to share at all rather than share unsafely.
//
// The check is structural-only — it confirms the expected rule_set
// tags appear in both route.rule_set and route.rules (as a reject
// action), plus two canary entries from the static reject block. It
// does NOT verify the .srs files at the rule_set URLs are uncorrupted
// or that the URLs themselves are trustworthy; those are separate
// supply-chain concerns tracked in engineering#TODO.
func validateAbuseRules(optionsJSON string) error {
	var raw map[string]any
	if err := json.Unmarshal([]byte(optionsJSON), &raw); err != nil {
		return fmt.Errorf("parse launch_cfg JSON: %w", err)
	}
	route, ok := raw["route"].(map[string]any)
	if !ok {
		return errors.New("launch_cfg is missing route block — peer would have no abuse blocking at all")
	}

	var errs []error
	if err := validateAbuseRuleSetTags(route); err != nil {
		errs = append(errs, err)
	}
	if err := validateAbuseRejectRules(route); err != nil {
		errs = append(errs, err)
	}
	if err := validateStaticRejectCanaries(route); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// validateAbuseRuleSetTags asserts every entry in abuseRuleSetTags is
// declared in route.rule_set. Missing entries mean sing-box won't even
// download the abuse list, so no destination check ever happens.
func validateAbuseRuleSetTags(route map[string]any) error {
	rsList, _ := route["rule_set"].([]any)
	got := map[string]bool{}
	for _, rs := range rsList {
		rsMap, ok := rs.(map[string]any)
		if !ok {
			continue
		}
		if tag, _ := rsMap["tag"].(string); tag != "" {
			got[tag] = true
		}
	}
	var missing []string
	for _, want := range abuseRuleSetTags {
		if !got[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("route.rule_set is missing abuse tags: %v (peer would not block matching destinations)", missing)
	}
	return nil
}

// validateAbuseRejectRules asserts every abuseRuleSetTags entry also
// has a matching reject rule in route.rules. A rule_set without a
// matching reject is a no-op — sing-box downloads the list and does
// nothing with it.
func validateAbuseRejectRules(route map[string]any) error {
	rules, _ := route["rules"].([]any)
	rejectedTags := map[string]bool{}
	for _, r := range rules {
		body := ruleBody(r)
		if body == nil {
			continue
		}
		if action, _ := body["action"].(string); action != "reject" {
			continue
		}
		for _, t := range asStringSlice(body["rule_set"]) {
			rejectedTags[t] = true
		}
	}
	var missing []string
	for _, want := range abuseRuleSetTags {
		if !rejectedTags[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("route.rules has no reject action for abuse tags: %v (rule_sets would download but not block)", missing)
	}
	return nil
}

// validateStaticRejectCanaries spot-checks that the static
// destination-based reject rules (RFC1918 CIDRs + abuse ports) are
// present. Picks one canary from each block rather than asserting the
// full set so legitimate additions in samizdat.go don't break this
// check.
func validateStaticRejectCanaries(route map[string]any) error {
	rules, _ := route["rules"].([]any)
	gotRFC1918 := false
	gotSMTP := false
	for _, r := range rules {
		body := ruleBody(r)
		if body == nil {
			continue
		}
		if action, _ := body["action"].(string); action != "reject" {
			continue
		}
		for _, cidr := range asStringSlice(body["ip_cidr"]) {
			if cidr == rfc1918CanaryCIDR {
				gotRFC1918 = true
			}
		}
		for _, p := range asFloatSlice(body["port"]) {
			if p == smtpCanaryPort {
				gotSMTP = true
			}
		}
	}
	var missing []string
	if !gotRFC1918 {
		missing = append(missing, fmt.Sprintf("RFC1918 reject (canary %s)", rfc1918CanaryCIDR))
	}
	if !gotSMTP {
		missing = append(missing, fmt.Sprintf("SMTP-port reject (canary :%d)", int(smtpCanaryPort)))
	}
	if len(missing) > 0 {
		return fmt.Errorf("route.rules is missing static abuse blocks: %v", missing)
	}
	return nil
}

// ruleBody returns the field-bearing inner object of a sing-box
// route Rule. sing-box marshals "default" rules in two equivalent
// shapes: inlined at the top level (no "default" wrapper) or nested
// under "default". We accept both.
func ruleBody(r any) map[string]any {
	m, ok := r.(map[string]any)
	if !ok {
		return nil
	}
	if nested, ok := m["default"].(map[string]any); ok {
		return nested
	}
	return m
}

func asStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func asFloatSlice(v any) []float64 {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]float64, 0, len(arr))
	for _, x := range arr {
		if f, ok := x.(float64); ok {
			out = append(out, f)
		}
	}
	return out
}
