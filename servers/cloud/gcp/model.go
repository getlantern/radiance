package gcp

import (
	"fmt"
	"github.com/getlantern/radiance/servers/cloud/common"
	"strings"
)

// --- Error Types ---

// Error represents a specific error returned by a GCP API operation.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string {
	return fmt.Sprintf("GCP API Error %d: %s", e.Code, e.Message)
}

// HttpError represents an HTTP error encountered during an API call.
type HttpError struct {
	StatusCode int
	Status     string
	Body       string // Optionally include response body for debugging
}

func (e *HttpError) Error() string {
	return fmt.Sprintf("HTTP Error %d: %s", e.StatusCode, e.Status)
}

// --- Data Structures (Translated from TypeScript types) ---

type Status struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type DiskInitParams struct {
	SourceImage string `json:"sourceImage"` // URL format
}

type InstanceDisk struct {
	Boot             bool           `json:"boot"`
	InitializeParams DiskInitParams `json:"initializeParams"`
}

type NetworkInterfaceAccessConfig struct {
	Type                string `json:"type,omitempty"` // e.g., "ONE_TO_ONE_NAT"
	Name                string `json:"name,omitempty"`
	NatIP               string `json:"natIP,omitempty"`
	SetPublicPtr        bool   `json:"setPublicPtr,omitempty"`
	PublicPtrDomainName string `json:"publicPtrDomainName,omitempty"`
	NetworkTier         string `json:"networkTier,omitempty"` // e.g., "PREMIUM"
	Kind                string `json:"kind,omitempty"`        // e.g., "compute#accessConfig"
}

type InstanceNetworkInterface struct {
	Network       string                         `json:"network"`              // URL format
	Subnetwork    string                         `json:"subnetwork,omitempty"` // URL format
	NetworkIP     string                         `json:"networkIP,omitempty"`
	Ipv6Address   string                         `json:"ipv6Address,omitempty"`
	Name          string                         `json:"name,omitempty"`
	AccessConfigs []NetworkInterfaceAccessConfig `json:"accessConfigs"`
}

type InstanceMetadataItem struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type InstanceMetadata struct {
	Items []InstanceMetadataItem `json:"items"`
}

// Instance represents a GCE VM instance.
type Instance struct {
	ID                string                     `json:"id,omitempty"`
	CreationTimestamp string                     `json:"creationTimestamp,omitempty"`
	Name              string                     `json:"name"`
	Description       string                     `json:"description"`
	Tags              *InstanceTags              `json:"tags,omitempty"`
	MachineType       string                     `json:"machineType"`    // URL format
	Zone              string                     `json:"zone,omitempty"` // URL format
	NetworkInterfaces []InstanceNetworkInterface `json:"networkInterfaces"`
	Disks             []InstanceDisk             `json:"disks"`
	Metadata          InstanceMetadata           `json:"metadata"`
	Labels            map[string]string          `json:"labels,omitempty"` // Key-value pairs
}

type InstanceTags struct {
	Items       []string `json:"items"`
	Fingerprint string   `json:"fingerprint"` // Needed for updates
}
type StaticIpCreate struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Address     string `json:"address,omitempty"` // Optional for creation
}

// StaticIp represents a GCE static IP address.
type StaticIp struct {
	ID                string `json:"id"`
	CreationTimestamp string `json:"creationTimestamp"`
	Name              string `json:"name"`
	Description       string `json:"description,omitempty"`
	Address           string `json:"address"`
	Status            string `json:"status"` // e.g., "RESERVED", "IN_USE"
	Region            string `json:"region"` // URL format
	NetworkTier       string `json:"networkTier"`
	AddressType       string `json:"addressType"` // "INTERNAL" or "EXTERNAL"
	Purpose           string `json:"purpose"`     // e.g. "GCE_ENDPOINT"
	// Add other fields as needed
}

type RegionLocator struct {
	ProjectID string
	RegionID  string
}

type Locator struct {
	ProjectID  string
	ZoneID     string
	InstanceID string
}

type GuestAttributes struct {
	VariableKey   string `json:"variableKey,omitempty"`
	VariableValue string `json:"variableValue,omitempty"`
	QueryPath     string `json:"queryPath"`
	QueryValue    *struct {
		Items []struct {
			Namespace string `json:"namespace"`
			Key       string `json:"key"`
			Value     string `json:"value"`
		} `json:"items"`
	} `json:"queryValue,omitempty"`
	Kind string `json:"kind"` // e.g. compute#GuestAttributes
}

type Zone struct {
	ID                string `json:"id"`
	CreationTimestamp string `json:"creationTimestamp"`
	Name              string `json:"name"`
	Description       string `json:"description"`
	Status            string `json:"status"` // "UP" or "DOWN"
	Region            string `json:"region"` // URL format
	Kind              string `json:"kind"`   // e.g. compute#zone
}

type computeEngineOperationError struct {
	Errors []Status `json:"errors"`
}

type ComputeEngineOperation struct {
	ID         string                       `json:"id"`
	Name       string                       `json:"name"`
	TargetID   string                       `json:"targetId,omitempty"` // Sometimes present
	TargetLink string                       `json:"targetLink,omitempty"`
	Status     string                       `json:"status"` // "PENDING", "RUNNING", "DONE"
	Error      *computeEngineOperationError `json:"error,omitempty"`
	Kind       string                       `json:"kind"` // e.g. "compute#operation"
}

type ResourceManagerOperation struct {
	Name  string  `json:"name"`
	Done  bool    `json:"done"`
	Error *Status `json:"error,omitempty"`
}

type ServiceUsageOperation struct {
	Name  string  `json:"name"`
	Done  bool    `json:"done"`
	Error *Status `json:"error,omitempty"`
	// Could also include Metadata and Response fields if needed
}

type Project struct {
	ProjectNumber  string `json:"projectNumber"`
	ProjectID      string `json:"projectId"`
	Name           string `json:"name"`
	LifecycleState string `json:"lifecycleState"` // e.g., "ACTIVE"
	CreateTime     string `json:"createTime"`
	// Add other fields like parent, labels as needed
}

type Firewall struct {
	ID                string   `json:"id,omitempty"`
	CreationTimestamp string   `json:"creationTimestamp"`
	Name              string   `json:"name"`
	Description       string   `json:"description,omitempty"`
	Network           string   `json:"network,omitempty"` // URL format
	Priority          int      `json:"priority"`
	Direction         string   `json:"direction"` // "INGRESS" or "EGRESS"
	Allowed           []Rule   `json:"allowed,omitempty"`
	Denied            []Rule   `json:"denied,omitempty"`
	SourceRanges      []string `json:"sourceRanges,omitempty"`
	DestinationRanges []string `json:"destinationRanges,omitempty"`
	TargetTags        []string `json:"targetTags,omitempty"`
	// Add other fields as needed
}

type Rule struct {
	IPProtocol string   `json:"IPProtocol"`      // e.g. "tcp", "udp", "icmp"
	Ports      []string `json:"ports,omitempty"` // e.g. ["22", "80", "1000-2000"]
}

type BillingAccount struct {
	Name                 string `json:"name"` // Format: billingAccounts/{billing_account_id}
	Open                 bool   `json:"open"`
	DisplayName          string `json:"displayName"`
	MasterBillingAccount string `json:"masterBillingAccount,omitempty"` // If subaccount
}

type ProjectBillingInfo struct {
	Name               string  `json:"name"` // Format: projects/{project_id}/billingInfo
	ProjectID          string  `json:"projectId"`
	BillingAccountName *string `json:"billingAccountName,omitempty"` // Pointer to distinguish nil/empty
	BillingEnabled     *bool   `json:"billingEnabled,omitempty"`     // Pointer to distinguish nil/false
}

type UserInfo struct {
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Sub           string `json:"sub"` // Subject identifier
	Name          string `json:"name,omitempty"`
	GivenName     string `json:"given_name,omitempty"`
	FamilyName    string `json:"family_name,omitempty"`
	Picture       string `json:"picture,omitempty"`
	Locale        string `json:"locale,omitempty"`
}

type Service struct {
	Name string `json:"name"` // Format: projects/{project}/services/{service}
	// Config object might exist but is often minimal in list response
	// Config struct { Name string `json:"name"`} `json:"config"`
	State string `json:"state"` // "STATE_UNSPECIFIED", "DISABLED", "ENABLED"
}

// --- Response Types ---

type listInstancesResponse struct {
	Items         []Instance `json:"items"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
	Kind          string     `json:"kind"` // e.g. compute#instanceList
}

// Note: The structure for aggregated list is different
type listAllInstancesResponse struct {
	Items map[string]struct {
		Instances []Instance `json:"instances,omitempty"`
		Warning   *struct {  // Optional warning if zone is unreachable etc.
			Code    string                        `json:"code"` // e.g. "NO_RESULTS_ON_PAGE"
			Message string                        `json:"message"`
			Data    []struct{ Key, Value string } `json:"data"`
		} `json:"warning,omitempty"`
	} `json:"items"` // Key is "zones/{zone_name}" or "regions/{region_name}"
	NextPageToken string `json:"nextPageToken,omitempty"`
	Kind          string `json:"kind"` // e.g. compute#instanceAggregatedList
}

type listZonesResponse struct {
	Items         []Zone `json:"items"`
	NextPageToken string `json:"nextPageToken,omitempty"`
	Kind          string `json:"kind"` // e.g. compute#zoneList
}

type listProjectsResponse struct {
	Projects      []Project `json:"projects"`
	NextPageToken string    `json:"nextPageToken,omitempty"`
}

type listFirewallsResponse struct {
	Items         []Firewall `json:"items"`
	NextPageToken string     `json:"nextPageToken,omitempty"`
	Kind          string     `json:"kind"` // e.g. compute#firewallList
}

type listBillingAccountsResponse struct {
	BillingAccounts []BillingAccount `json:"billingAccounts"`
	NextPageToken   string           `json:"nextPageToken,omitempty"`
}

type listEnabledServicesResponse struct {
	Services      []Service `json:"services"`
	NextPageToken string    `json:"nextPageToken,omitempty"`
}

// GetRegionID returns a region ID like "us-central1".
func (z *Zone) GetRegionID() string {
	lastDashIndex := strings.LastIndex(z.ID, "-")
	if lastDashIndex == -1 {
		return z.ID
	}
	return z.ID[:lastDashIndex]
}

// GetLocation implements the CloudLocation interface.
func (z *Zone) GetLocation() *common.GeoLocation {
	// Map of region IDs to GeoLocations
	locationMap := map[string]*common.GeoLocation{
		"asia-east1":              &common.CHANGHUA_COUNTY,
		"asia-east2":              &common.HONG_KONG,
		"asia-northeast1":         &common.TOKYO,
		"asia-northeast2":         &common.OSAKA,
		"asia-northeast3":         &common.SEOUL,
		"asia-south1":             &common.MUMBAI,
		"asia-south2":             &common.DELHI,
		"asia-southeast1":         &common.JURONG_WEST,
		"asia-southeast2":         &common.JAKARTA,
		"australia-southeast1":    &common.SYDNEY,
		"australia-southeast2":    &common.MELBOURNE,
		"europe-north1":           &common.HAMINA,
		"europe-west1":            &common.ST_GHISLAIN,
		"europe-west2":            &common.LONDON,
		"europe-west3":            &common.FRANKFURT,
		"europe-west4":            &common.EEMSHAVEN,
		"europe-west6":            &common.ZURICH,
		"europe-central2":         &common.WARSAW,
		"northamerica-northeast1": &common.MONTREAL,
		"northamerica-northeast2": &common.TORONTO,
		"southamerica-east1":      &common.SAO_PAULO,
		"us-central1":             &common.IOWA,
		"us-east1":                &common.SOUTH_CAROLINA,
		"us-east4":                &common.NORTHERN_VIRGINIA,
		"us-west1":                &common.OREGON,
		"us-west2":                &common.LOS_ANGELES,
		"us-west3":                &common.SALT_LAKE_CITY,
		"us-west4":                &common.LAS_VEGAS,
	}

	return locationMap[z.GetRegionID()]
}
