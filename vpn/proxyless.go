package vpn

// proxylessDetourTag is the tag lantern-cloud gives the proxyless (Outline
// smart-dialer) outbound it sends for fetching remote rule-sets under DPI: the
// server owns that outbound's config (DoH resolvers, TLS-fragmentation
// strategies, probe hosts) and sets it as the rule-sets' download_detour, so it
// stays tunable without a client release. radiance only needs to know the tag
// names an infrastructure outbound — merged into the box config but excluded
// from the user-selectable proxy groups (see reservedTags).
const proxylessDetourTag = "proxyless"
