package vpn

import (
	"reflect"
	"slices"

	lcommon "github.com/getlantern/common"
	lbO "github.com/getlantern/lantern-box/option"
	O "github.com/sagernet/sing-box/option"
)

// availableOutboundTags returns configured tags and any additional tags supplied by the caller.
func availableOutboundTags(options O.Options, additionalTags ...string) map[string]bool {
	available := make(map[string]bool)
	for _, tag := range additionalTags {
		available[tag] = true
	}
	for _, out := range options.Outbounds {
		available[out.Tag] = true
	}
	for _, endpoint := range options.Endpoints {
		available[endpoint.Tag] = true
	}
	return available
}

// normalizeOutbounds enforces proxy policy and removes unavailable dependencies without modifying its inputs.
// Retained tags belong to the surrounding tunnel; incoming options take precedence over them.
func normalizeOutbounds(options O.Options, rules lcommon.SmartRoutingRules, retainedTags ...string) (O.Options, lcommon.SmartRoutingRules) {
	incoming := availableOutboundTags(options)
	retainedTags = slices.DeleteFunc(slices.Clone(retainedTags), func(tag string) bool { return incoming[tag] })
	options.Outbounds = allowedOutbounds(options.Outbounds)
	options.Endpoints = allowedEndpoints(options.Endpoints)
	smartOutbounds, _, _ := rules.ToOptions(urlTestInterval, urlTestIdleTimeout)
	for {
		available := availableOutboundTags(options, retainedTags...)
		for _, out := range smartOutbounds {
			available[out.Tag] = true
		}
		smartRules := normalizeSmartRoutingRules(rules, available)
		missingDependency := func(options any) bool {
			return slices.ContainsFunc(requiredOutboundDependencies(options), func(tag string) bool { return !available[tag] })
		}
		outbounds := make([]O.Outbound, 0, len(options.Outbounds))
		for _, out := range options.Outbounds {
			if missingDependency(out.Options) {
				continue
			}
			group, members, defaultTag := copyOutboundGroup(out.Options)
			if members != nil {
				*members = slices.DeleteFunc(slices.Clone(*members), func(tag string) bool { return !available[tag] })
				if len(*members) == 0 {
					continue
				}
				if defaultTag != nil && !slices.Contains(*members, *defaultTag) {
					*defaultTag = ""
				}
				out.Options = group
			}
			outbounds = append(outbounds, out)
		}
		endpoints := slices.DeleteFunc(slices.Clone(options.Endpoints), func(endpoint O.Endpoint) bool {
			return missingDependency(endpoint.Options)
		})
		nextSmartOutbounds, _, _ := smartRules.ToOptions(urlTestInterval, urlTestIdleTimeout)
		stable := len(outbounds) == len(options.Outbounds) && len(endpoints) == len(options.Endpoints) && len(nextSmartOutbounds) == len(smartOutbounds)
		options.Outbounds = outbounds
		options.Endpoints = endpoints
		if stable {
			return options, smartRules
		}
		// Dropping a dependency can invalidate other outbounds or endpoints.
		rules = smartRules
		smartOutbounds = nextSmartOutbounds
	}
}

// requiredOutboundDependencies returns dependencies that cannot be removed without changing routing.
func requiredOutboundDependencies(options any) []string {
	v := reflect.ValueOf(options)
	if v.IsValid() && v.Kind() == reflect.Struct {
		copy := reflect.New(v.Type())
		copy.Elem().Set(v)
		options = copy.Interface()
	}
	switch opts := options.(type) {
	case *lbO.FallbackOutboundOptions:
		if opts == nil {
			return []string{""}
		}
		return []string{opts.Primary, opts.Fallback}
	case O.DialerOptionsWrapper:
		if v.Kind() == reflect.Pointer && v.IsNil() {
			return nil
		}
		if detour := opts.TakeDialerOptions().Detour; detour != "" {
			return []string{detour}
		}
	}
	return nil
}

// outboundDependencies includes group members as well as required routing dependencies.
func outboundDependencies(options any) []string {
	_, members, _ := copyOutboundGroup(options)
	if members != nil {
		return *members
	}
	return requiredOutboundDependencies(options)
}

// copyOutboundGroup returns a shallow copy and pointers to its members and optional default.
// Options without a member slice are returned unchanged, with nil field pointers.
func copyOutboundGroup(options any) (any, *[]string, *string) {
	switch opts := options.(type) {
	case O.SelectorOutboundOptions:
		return &opts, &opts.Outbounds, &opts.Default
	case *O.SelectorOutboundOptions:
		if opts != nil {
			return copyOutboundGroup(*opts)
		}
	case O.URLTestOutboundOptions:
		return &opts, &opts.Outbounds, nil
	case *O.URLTestOutboundOptions:
		if opts != nil {
			return copyOutboundGroup(*opts)
		}
	case lbO.MutableSelectorOutboundOptions:
		return &opts, &opts.Outbounds, nil
	case *lbO.MutableSelectorOutboundOptions:
		if opts != nil {
			return copyOutboundGroup(*opts)
		}
	case lbO.MutableURLTestOutboundOptions:
		return &opts, &opts.Outbounds, nil
	case *lbO.MutableURLTestOutboundOptions:
		if opts != nil {
			return copyOutboundGroup(*opts)
		}
	case lbO.MutableAutoSelectOutboundOptions:
		return &opts, &opts.Outbounds, nil
	case *lbO.MutableAutoSelectOutboundOptions:
		if opts != nil {
			return copyOutboundGroup(*opts)
		}
	}
	return options, nil, nil
}
