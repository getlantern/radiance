package common

// GeoLocation represents a unified server location model for all cloud providers.
//
// Each ID identifies a location as displayed in the Outline
// user interface. To minimize confusion, Outline attempts to
// present each location in a manner consistent with the cloud
// provider's own interface and documentation. When cloud providers
// present a location in similar fashion, they may share an element
// (e.g. 'frankfurt' for GCP and DO), but if they present a similar
// location in different terms, they will need to be represented
// separately (e.g. 'SG' for DO, 'jurong-west' for GCP).
//
// When the ID and CountryCode are equal, this indicates that they are redundant.
type GeoLocation struct {
	ID          string
	CountryCode string
}

// CountryIsRedundant returns true if the country code is the same as the ID.
func (g *GeoLocation) CountryIsRedundant() bool {
	return g.CountryCode == g.ID
}

// Predefined GeoLocation constants
var (
	AMSTERDAM         = GeoLocation{ID: "amsterdam", CountryCode: "NL"}
	NORTHERN_VIRGINIA = GeoLocation{ID: "northern-virginia", CountryCode: "US"}
	BANGALORE         = GeoLocation{ID: "bangalore", CountryCode: "IN"}
	IOWA              = GeoLocation{ID: "iowa", CountryCode: "US"}
	CHANGHUA_COUNTY   = GeoLocation{ID: "changhua-county", CountryCode: "TW"}
	DELHI             = GeoLocation{ID: "delhi", CountryCode: "IN"}
	EEMSHAVEN         = GeoLocation{ID: "eemshaven", CountryCode: "NL"}
	FRANKFURT         = GeoLocation{ID: "frankfurt", CountryCode: "DE"}
	HAMINA            = GeoLocation{ID: "hamina", CountryCode: "FI"}
	HONG_KONG         = GeoLocation{ID: "HK", CountryCode: "HK"}
	JAKARTA           = GeoLocation{ID: "jakarta", CountryCode: "ID"}
	JURONG_WEST       = GeoLocation{ID: "jurong-west", CountryCode: "SG"}
	LAS_VEGAS         = GeoLocation{ID: "las-vegas", CountryCode: "US"}
	LONDON            = GeoLocation{ID: "london", CountryCode: "GB"}
	LOS_ANGELES       = GeoLocation{ID: "los-angeles", CountryCode: "US"}
	OREGON            = GeoLocation{ID: "oregon", CountryCode: "US"}
	MELBOURNE         = GeoLocation{ID: "melbourne", CountryCode: "AU"}
	MONTREAL          = GeoLocation{ID: "montreal", CountryCode: "CA"}
	MUMBAI            = GeoLocation{ID: "mumbai", CountryCode: "IN"}
	NEW_YORK_CITY     = GeoLocation{ID: "new-york-city", CountryCode: "US"}
	SAN_FRANCISCO     = GeoLocation{ID: "san-francisco", CountryCode: "US"}
	SINGAPORE         = GeoLocation{ID: "SG", CountryCode: "SG"}
	OSAKA             = GeoLocation{ID: "osaka", CountryCode: "JP"}
	SAO_PAULO         = GeoLocation{ID: "sao-paulo", CountryCode: "BR"}
	SALT_LAKE_CITY    = GeoLocation{ID: "salt-lake-city", CountryCode: "US"}
	SEOUL             = GeoLocation{ID: "seoul", CountryCode: "KR"}
	ST_GHISLAIN       = GeoLocation{ID: "st-ghislain", CountryCode: "BE"}
	SYDNEY            = GeoLocation{ID: "sydney", CountryCode: "AU"}
	SOUTH_CAROLINA    = GeoLocation{ID: "south-carolina", CountryCode: "US"}
	TOKYO             = GeoLocation{ID: "tokyo", CountryCode: "JP"}
	TORONTO           = GeoLocation{ID: "toronto", CountryCode: "CA"}
	WARSAW            = GeoLocation{ID: "warsaw", CountryCode: "PL"}
	ZURICH            = GeoLocation{ID: "zurich", CountryCode: "CH"}
)

// CloudLocation represents a location in a cloud provider.
type CloudLocation interface {
	// GetID returns the cloud-specific ID used for this location, or empty string to represent
	// a GeoID that lacks a usable datacenter.
	GetID() string

	// GetLocation returns the physical location of this datacenter, or nil if its location is
	// unknown.
	GetLocation() *GeoLocation
}

// CloudLocationOption represents a cloud location with availability information.
type CloudLocationOption struct {
	CloudLocation CloudLocation
	Available     bool
}
