package peer

import (
	"encoding/json"
	"errors"
	"fmt"
)

// abuseRuleSetTags is the canonical list of abuse rule_set tags that
// the peer launch_cfg MUST carry. Mirrors the server-side abuseTags
// list that emits the rule_set entries into the registration response.
// If the server-side list grows or renames a tag, this list grows
// with it — the server-side test asserts the emit side; this list
// asserts the client side sees the same shape after registration.
var abuseRuleSetTags = []string{
	"geosite-malware",
	"geoip-malware",
	"geosite-phishing",
	"geosite-cryptominers",
}

// rfc1918CanaryCIDR and smtpCanaryPort are sentinel values that, if
// missing from the launch_cfg's reject rules, indicate the server-
// side static peer-egress-block list was dropped or mutated. We pick
// one IP-CIDR and one port from each block as a low-cost smoke test;
// a full structural check would be brittle to upstream additions.
const (
	rfc1918CanaryCIDR = "10.0.0.0/8"
	smtpCanaryPort    = float64(25)
)

// validateAbuseRules is a defence-in-depth check on the sing-box
// options returned by /v1/peer/register. The server is supposed to
// embed a set of route.rule_set + route.rules entries that block the
// peer from forwarding traffic to known-malicious destinations,
// RFC1918 CIDRs, and abuse-prone ports.
//
// If a future server-side regression ships a launch_cfg without those
// rules, every newly-registered peer would silently turn into an open
// residential proxy until someone noticed. This validator blocks
// Start before libbox runs an unsafe config; the peer prefers to fail
// to share at all rather than share unsafely.
//
// The check is structural-only — it confirms the expected rule_set
// tags appear in both route.rule_set and route.rules (as an
// unconditional reject), plus two canary entries from the static
// reject block. It does NOT verify the .srs files at the rule_set
// URLs are uncorrupted or that the URLs themselves are trustworthy;
// those are separate supply-chain concerns and are not in scope for
// this gate.
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
//
// Only counts *unconditional* rejects (see isUnconditionalReject):
// a reject rule with an extra match constraint (port, domain, source
// IP, etc.) or with invert=true would let traffic in the abuse list
// through under most conditions; counting it as covering the tag
// would mask a misconfigured launch_cfg.
func validateAbuseRejectRules(route map[string]any) error {
	rules, _ := route["rules"].([]any)
	rejectedTags := map[string]bool{}
	for _, r := range rules {
		body := ruleBody(r)
		if body == nil {
			continue
		}
		if !isUnconditionalReject(body, "rule_set") {
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
		return fmt.Errorf("route.rules has no unconditional reject for abuse tags: %v (rule_sets would download but not unconditionally block)", missing)
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
		// Each canary is checked against an unconditional reject scoped
		// to its own match field. A reject that ANDs ip_cidr with a port
		// or domain (or sets invert) would not actually cover the
		// destination class the canary represents, so don't credit it.
		if isUnconditionalReject(body, "ip_cidr") {
			for _, cidr := range asStringSlice(body["ip_cidr"]) {
				if cidr == rfc1918CanaryCIDR {
					gotRFC1918 = true
				}
			}
		}
		if isUnconditionalReject(body, "port") {
			for _, p := range asFloatSlice(body["port"]) {
				if p == smtpCanaryPort {
					gotSMTP = true
				}
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

// isUnconditionalReject reports whether the rule body is a reject
// action whose scope is defined solely by matchKey — no other match
// fields and no invert. matchKey is the field expected to carry the
// rule's scope ("rule_set", "ip_cidr", or "port").
//
// The pure-reject shape we want for each abuse-block category is:
//
//	{"action": "reject", "<matchKey>": [...]}                    // canonical
//	{"action": "reject", "<matchKey>": [...], "invert": false}   // explicit no-op
//
// Anything else either narrows the match (e.g. adding "port": 80
// to a rule_set reject limits it to port-80 traffic) or inverts the
// match (invert=true rejects everything OUTSIDE the matchKey). In
// both cases the launch_cfg would not actually block the abuse
// destination class the rule claims to cover, so callers must not
// credit it as covering the tag.
func isUnconditionalReject(body map[string]any, matchKey string) bool {
	if action, _ := body["action"].(string); action != "reject" {
		return false
	}
	if invert, _ := body["invert"].(bool); invert {
		return false
	}
	allowed := map[string]bool{"action": true, "invert": true, matchKey: true}
	for k := range body {
		if !allowed[k] {
			return false
		}
	}
	return true
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

// asStringSlice normalizes a sing-box rule field that can be encoded
// as either a scalar string or a string array. Both forms appear in
// practice: `{"rule_set": "sr-direct"}` and `{"rule_set": ["a","b"]}`
// are equivalent at the route layer. Treating only the array form as
// valid here would false-positive a launch_cfg that emits the scalar.
func asStringSlice(v any) []string {
	if s, ok := v.(string); ok {
		return []string{s}
	}
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

// asFloatSlice is the numeric counterpart of asStringSlice — fields
// like `port` can come back as `25` or `[25, 587]`. JSON unmarshals
// every number to float64, so the canary comparison uses float64 too.
func asFloatSlice(v any) []float64 {
	if f, ok := v.(float64); ok {
		return []float64{f}
	}
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
