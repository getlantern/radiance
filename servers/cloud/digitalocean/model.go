package digitalocean

import (
	"github.com/getlantern/radiance/servers/cloud/common"
	"strings"
)

// Region represents a DigitalOcean DigitalOceanRegion, e.g. "NYC2".
type Region struct {
	ID string
}

// GetID implements the CloudLocation interface.
func (r *Region) GetID() string {
	return r.ID
}

// GetLocation implements the CloudLocation interface.
func (r *Region) GetLocation() *common.GeoLocation {
	// Map of city IDs to GeoLocations
	locationMap := map[string]*common.GeoLocation{
		"ams": &common.AMSTERDAM,
		"blr": &common.BANGALORE,
		"fra": &common.FRANKFURT,
		"lon": &common.LONDON,
		"nyc": &common.NEW_YORK_CITY,
		"sfo": &common.SAN_FRANCISCO,
		"sgp": &common.SINGAPORE,
		"syd": &common.SYDNEY,
		"tor": &common.TORONTO,
	}

	// Extract the city code (first 3 characters) and convert to lowercase
	cityCode := strings.ToLower(r.ID[0:3])
	return locationMap[cityCode]
}

// DropletSpecification defines the specification for creating a DigitalOcean droplet.
type DropletSpecification struct {
	InstallCommand string   `json:"installCommand"`
	Size           string   `json:"size"`
	Image          string   `json:"image"`
	Tags           []string `json:"tags"`
}

// DropletInfo represents information about a DigitalOcean droplet.
// See definition and example at
// https://developers.digitalocean.com/documentation/v2/#retrieve-an-existing-droplet-by-id
type DropletInfo struct {
	ID     int      `json:"id"`
	Status string   `json:"status"` // 'new' or 'active'
	Tags   []string `json:"tags"`
	Region struct {
		Slug string `json:"slug"`
	} `json:"region"`
	Size struct {
		Transfer     float64 `json:"transfer"`
		PriceMonthly float64 `json:"price_monthly"`
	} `json:"size"`
	Networks struct {
		V4 []struct {
			Type      string `json:"type"`
			IPAddress string `json:"ip_address"`
		} `json:"v4"`
	} `json:"networks"`
}

// Account represents a DigitalOcean account.
// Reference:
// https://developers.digitalocean.com/documentation/v2/#get-user-information
type Account struct {
	DropletLimit  int    `json:"droplet_limit"`
	Email         string `json:"email"`
	UUID          string `json:"uuid"`
	EmailVerified bool   `json:"email_verified"`
	Status        string `json:"status"` // 'active', 'warning', or 'locked'
	StatusMessage string `json:"status_message"`
}

// RegionInfo represents information about a DigitalOcean region.
// Reference:
// https://developers.digitalocean.com/documentation/v2/#regions
type RegionInfo struct {
	Slug      string   `json:"slug"`
	Name      string   `json:"name"`
	Sizes     []string `json:"sizes"`
	Available bool     `json:"available"`
	Features  []string `json:"features"`
}
