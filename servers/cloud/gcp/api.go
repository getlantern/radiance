package gcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const (
	gcpOAuthClientID     = "1095197144751-4dq0l2sqvsq8fuc9q4uq0bn2bcq40dr9.apps.googleusercontent.com"
	gcpOAuthClientSecret = "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" // Not a secret for native apps
	gceV1API             = "https://compute.googleapis.com/compute/v1"
	resourceManagerV1API = "https://cloudresourcemanager.googleapis.com/v1"
	serviceUsageV1API    = "https://serviceusage.googleapis.com/v1"
	billingV1API         = "https://cloudbilling.googleapis.com/v1"
	openidConnectV1API   = "https://openidconnect.googleapis.com/v1"
	firewallName         = "lantern-firewall" // Default firewall name for GCP
	machineSize          = "e2-micro"
)

var requiredServices = []string{"compute.googleapis.com"}

// APIClient interacts with Google Cloud Platform APIs.
type APIClient struct {
	httpClient *http.Client // Automatically handles auth via oauth2 package
}

// NewAPIClient creates a new client using a refresh token.
// It configures an http.Client that automatically handles token refreshes.
func NewAPIClient(ctx context.Context, refreshToken string) (*APIClient, error) {
	conf := &oauth2.Config{
		ClientID:     gcpOAuthClientID,
		ClientSecret: gcpOAuthClientSecret,
		Endpoint:     google.Endpoint,
		Scopes: []string{ // Define required scopes here
			"https://www.googleapis.com/auth/compute",
			"https://www.googleapis.com/auth/cloud-platform",        // Project creation, billing, service usage
			"https://www.googleapis.com/auth/cloudplatformprojects", // Project listing/management
			"https://www.googleapis.com/auth/userinfo.email",        // For GetUserInfo
			"openid", // Required for userinfo endpoint
		},
	}

	// Create initial token struct with the refresh token
	token := &oauth2.Token{
		RefreshToken: refreshToken,
		// AccessToken and Expiry can be zero; the library will fetch/refresh as needed
	}

	// Create an HTTP client that automatically uses the token and refreshes it
	httpClient := conf.Client(ctx, token)

	return &APIClient{httpClient: httpClient}, nil
}

// --- Helper Methods ---

func projectURL(projectID string) string {
	return fmt.Sprintf("%s/projects/%s", gceV1API, projectID)
}

func regionURL(loc RegionLocator) string {
	return fmt.Sprintf("%s/regions/%s", projectURL(loc.ProjectID), loc.RegionID)
}

func zoneURL(loc Locator) string {
	return fmt.Sprintf("%s/zones/%s", projectURL(loc.ProjectID), loc.ZoneID)
}

func instanceURL(loc Locator) string {
	return fmt.Sprintf("%s/instances/%s", zoneURL(loc), loc.InstanceID)
}

// doRequest is a helper to make authenticated HTTP requests and handle responses.
func (c *APIClient) doRequest(ctx context.Context, method, urlStr string, queryParams url.Values, reqBody interface{}, respBody interface{}) error {
	var bodyReader io.Reader
	if reqBody != nil {
		jsonData, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal request body: %w", err)
		}
		bodyReader = bytes.NewBuffer(jsonData)
	}

	reqURL, err := url.Parse(urlStr)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	if queryParams != nil {
		reqURL.RawQuery = queryParams.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL.String(), bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body) // Ensure the body is closed

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Try to parse a GCP error format first
		var gcpErrResp struct {
			Error Status `json:"error"`
		}
		if json.Unmarshal(bodyBytes, &gcpErrResp) == nil && gcpErrResp.Error.Code != 0 {
			// Prefer the structured GcpError if available
			return &Error{Code: gcpErrResp.Error.Code, Message: gcpErrResp.Error.Message}
		}
		// Otherwise, return generic HttpError
		return &HttpError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       string(bodyBytes),
		}
	}

	// Handle 204 No Content specifically
	if resp.StatusCode == http.StatusNoContent || len(bodyBytes) == 0 {
		return nil // Success when there is nothing to parse
	}

	if respBody != nil {
		if err := json.Unmarshal(bodyBytes, respBody); err != nil {
			return fmt.Errorf("failed to unmarshal response body into %T: %w (body: %s)", respBody, err, string(bodyBytes))
		}
	}

	return nil
}

// --- Compute Engine API Methods ---

// CreateInstance creates a new GCE VM instance.
func (c *APIClient) CreateInstance(ctx context.Context, zone Locator, data Instance) (*ComputeEngineOperation, error) {
	var op ComputeEngineOperation
	err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("%s/instances", zoneURL(zone)), nil, data, &op)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// DeleteInstance deletes a specified GCE VM instance.
func (c *APIClient) DeleteInstance(ctx context.Context, instance Locator) (*ComputeEngineOperation, error) {
	var op ComputeEngineOperation
	err := c.doRequest(ctx, http.MethodDelete, instanceURL(instance), nil, nil, &op)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// GetInstance gets details of a specified GCE VM instance.
func (c *APIClient) GetInstance(ctx context.Context, instance Locator) (*Instance, error) {
	var inst Instance
	err := c.doRequest(ctx, http.MethodGet, instanceURL(instance), nil, nil, &inst)
	if err != nil {
		return nil, err
	}
	return &inst, nil
}

// ListInstances lists GCE VM instances in a specified zone. filter is optional.
// TODO: Implement pagination.
func (c *APIClient) ListInstances(ctx context.Context, zone Locator, filter string) ([]Instance, error) {
	params := url.Values{}
	if filter != "" {
		params.Set("filter", filter)
	}
	var resp listInstancesResponse
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/instances", zoneURL(zone)), params, nil, &resp)
	if err != nil {
		return nil, err
	}
	// Currently only returns the first page
	return resp.Items, nil
}

// ListAllInstances lists all GCE VM instances in a specified project. filter is optional.
// TODO: Implement pagination.
func (c *APIClient) ListAllInstances(ctx context.Context, projectID string, filter string) (map[string][]Instance, error) {
	params := url.Values{}
	if filter != "" {
		params.Set("filter", filter)
	}
	var resp listAllInstancesResponse
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/aggregated/instances", projectURL(projectID)), params, nil, &resp)
	if err != nil {
		return nil, err
	}

	result := make(map[string][]Instance)
	for key, val := range resp.Items {
		// Key is like "zones/us-central1-a"
		parts := strings.Split(key, "/")
		if len(parts) == 2 && len(val.Instances) > 0 {
			result[parts[1]] = val.Instances
		}
		// Ignore regions or zones with no instances or warnings for simplicity here
	}
	// Currently only returns the first page
	return result, nil
}

// CreateStaticIP creates or promotes an IP address to static.
// It waits for the operation to complete.
func (c *APIClient) CreateStaticIP(ctx context.Context, region RegionLocator, data StaticIpCreate) (*StaticIp, error) {
	var op ComputeEngineOperation

	if err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("%s/addresses", regionURL(region)), nil, data, &op); err != nil {
		return nil, fmt.Errorf("failed to initiate static IP creation: %w", err)
	}

	if _, err := c.ComputeEngineOperationRegionWait(ctx, region, op.Name); err != nil {
		return nil, fmt.Errorf("failed waiting for static IP creation: %w", err)
	}

	return c.GetStaticIP(ctx, region, data.Name)
}

// DeleteStaticIP deletes a static IP address.
func (c *APIClient) DeleteStaticIP(ctx context.Context, region RegionLocator, addressName string) (*ComputeEngineOperation, error) {
	var op ComputeEngineOperation

	if err := c.doRequest(ctx, http.MethodDelete, fmt.Sprintf("%s/addresses/%s", regionURL(region), addressName), nil, nil, &op); err != nil {
		return nil, err
	}
	return &op, nil
}

// GetStaticIP retrieves details of a static IP address.
func (c *APIClient) GetStaticIP(ctx context.Context, region RegionLocator, addressName string) (*StaticIp, error) {
	var ip StaticIp

	if err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/addresses/%s", regionURL(region), addressName), nil, nil, &ip); err != nil {
		// Handle 404 Not Found specifically if needed
		var httpErr *HttpError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return nil, nil // Indicate not found without erroring? Or return a specific error.
		}
		return nil, err
	}
	return &ip, nil
}

// GetGuestAttributes lists guest attributes for a specific namespace.
func (c *APIClient) GetGuestAttributes(ctx context.Context, instance Locator, namespace string) (*GuestAttributes, error) {
	params := url.Values{}
	params.Set("queryPath", namespace)

	var attrs GuestAttributes
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/getGuestAttributes", instanceURL(instance)), params, nil, &attrs)
	if err != nil {
		// Check for 404 specifically if needed (guest attributes not set)
		var httpErr *HttpError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return nil, nil // Return nil to indicate attributes not found/set
		}
		return nil, err
	}
	return &attrs, nil
}

// CreateFirewall creates a firewall rule.
// It waits for the operation to complete.
func (c *APIClient) CreateFirewall(ctx context.Context, projectID string, data Firewall) (*Firewall, error) {
	var op ComputeEngineOperation
	err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("%s/global/firewalls", projectURL(projectID)), nil, data, &op)
	if err != nil {
		return nil, fmt.Errorf("failed to initiate firewall creation: %w", err)
	}

	_, err = c.ComputeEngineOperationGlobalWait(ctx, projectID, op.Name)
	if err != nil {
		return nil, fmt.Errorf("failed waiting for firewall creation: %w", err)
	}

	return c.GetFirewall(ctx, projectID, data.Name)
}

// GetFirewall retrieves details of a specific firewall rule.
func (c *APIClient) GetFirewall(ctx context.Context, projectID, firewallName string) (*Firewall, error) {
	var fw Firewall
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/global/firewalls/%s", projectURL(projectID), firewallName), nil, nil, &fw)
	if err != nil {
		var httpErr *HttpError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			return nil, nil // Or a specific "not found" error
		}
		return nil, err
	}
	return &fw, nil
}

// ListFirewalls lists firewalls in a project. Filter is optional (e.g., "name=my-firewall").
// TODO: Implement pagination.
func (c *APIClient) ListFirewalls(ctx context.Context, projectID string, filter string) ([]Firewall, error) {
	params := url.Values{}
	if filter != "" {
		params.Set("filter", filter)
	}
	var resp listFirewallsResponse
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/global/firewalls", projectURL(projectID)), params, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// ListZones lists available zones for a project.
// TODO: Implement pagination.
func (c *APIClient) ListZones(ctx context.Context, projectID string) ([]Zone, error) {
	var resp listZonesResponse
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/zones", projectURL(projectID)), nil, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// --- Service Usage API Methods ---

// ListEnabledServices lists services enabled for a project.
// TODO: Implement pagination.
func (c *APIClient) ListEnabledServices(ctx context.Context, projectID string) ([]Service, error) {
	params := url.Values{}
	params.Set("filter", "state:ENABLED")

	var resp listEnabledServicesResponse
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%s/services", serviceUsageV1API, projectID), params, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Services, nil
}

// EnableServices enables multiple services for a project. data should be a struct like:
// type EnableServicesRequest struct { ServiceIDs []string `json:"serviceIds"` }
func (c *APIClient) EnableServices(ctx context.Context, projectID string, data interface{}) (*ServiceUsageOperation, error) {
	var op ServiceUsageOperation
	err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("%s/projects/%s/services:batchEnable", serviceUsageV1API, projectID), nil, data, &op)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// --- Cloud Resource Manager API Methods ---

// CreateProject creates a new GCP project. data should be a struct like:
// type CreateProjectRequest struct { ProjectID string `json:"projectId"`; Name string `json:"name,omitempty"`; Parent *Parent `json:"parent,omitempty"` }
// type Parent struct { Type string `json:"type"`; ID string `json:"id"`} // e.g., "organization", "folder"
func (c *APIClient) CreateProject(ctx context.Context, data interface{}) (*ResourceManagerOperation, error) {
	var op ResourceManagerOperation
	err := c.doRequest(ctx, http.MethodPost, fmt.Sprintf("%s/projects", resourceManagerV1API), nil, data, &op)
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// ListProjects lists projects the user has access to. filter is optional.
// TODO: Implement pagination.
func (c *APIClient) ListProjects(ctx context.Context, filter string) ([]Project, error) {
	params := url.Values{}
	if filter != "" {
		params.Set("filter", filter)
	}
	var resp listProjectsResponse
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/projects", resourceManagerV1API), params, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.Projects, nil
}

// --- Billing API Methods ---

// GetProjectBillingInfo gets the billing info for a project.
func (c *APIClient) GetProjectBillingInfo(ctx context.Context, projectID string) (*ProjectBillingInfo, error) {
	var info ProjectBillingInfo
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/projects/%s/billingInfo", billingV1API, projectID), nil, nil, &info)
	if err != nil {
		var httpErr *HttpError
		if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
			// API returns 403 if billing API not enabled, 404 might mean project doesn't exist
			// or sometimes if no billing account is linked. Treat as potentially "no info".
			return nil, nil // Or return a specific error type
		}
		return nil, err
	}
	return &info, nil
}

// UpdateProjectBillingInfo links a project to a billing account. data should be struct like:
// type UpdateBillingInfoRequest struct { BillingAccountName string `json:"billingAccountName"` }
func (c *APIClient) UpdateProjectBillingInfo(ctx context.Context, projectID string, data interface{}) (*ProjectBillingInfo, error) {
	var info ProjectBillingInfo
	err := c.doRequest(ctx, http.MethodPut, fmt.Sprintf("%s/projects/%s/billingInfo", billingV1API, projectID), nil, data, &info)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// ListBillingAccounts lists billing accounts the user has access to.
// TODO: Implement pagination.
func (c *APIClient) ListBillingAccounts(ctx context.Context) ([]BillingAccount, error) {
	var resp listBillingAccountsResponse
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/billingAccounts", billingV1API), nil, nil, &resp)
	if err != nil {
		return nil, err
	}
	return resp.BillingAccounts, nil
}

// --- Operation Wait Methods ---

const (
	operationPollInterval = 2 * time.Second
	operationPollTimeout  = 5 * time.Minute // Adjust as needed
)

// checkOperationError checks a completed operation for API errors.
func checkOperationError(status string, opError *computeEngineOperationError) error {
	if status == "DONE" && opError != nil && len(opError.Errors) > 0 {
		// Return the first error
		return &Error{
			Code:    opError.Errors[0].Code, // Note: GCE uses numeric codes in `errors.code`, often matching HTTP status
			Message: opError.Errors[0].Message,
		}
	}
	if status != "DONE" {
		// Should not happen if polling logic is correct, but safeguard
		return fmt.Errorf("operation finished polling but status is %s", status)
	}
	return nil
}

// ComputeEngineOperationZoneWait polls a zone operation until it's DONE.
func (c *APIClient) ComputeEngineOperationZoneWait(ctx context.Context, zone Locator, operationName string) (*ComputeEngineOperation, error) {
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled/timed out while waiting for zone operation %s: %w", operationName, ctx.Err())
		default:
			if time.Since(startTime) > operationPollTimeout {
				return nil, fmt.Errorf("timeout waiting for zone operation %s", operationName)
			}

			var op ComputeEngineOperation
			err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/operations/%s", zoneURL(zone), operationName), nil, nil, &op)
			if err != nil {
				// Don't necessarily stop polling on transient HTTP errors, but log/handle
				// If it's a 404, the operation might be invalid.
				var httpErr *HttpError
				if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
					return nil, fmt.Errorf("zone operation %s not found: %w", operationName, err)
				}
				// Maybe retry a few times before failing hard
				slog.Debug("Warning: error polling zone operation. Retrying...", "op", operationName, "err", err)
			} else {
				if op.Status == "DONE" {
					return &op, checkOperationError(op.Status, op.Error)
				}
			}

			time.Sleep(operationPollInterval)
		}
	}
}

// ComputeEngineOperationRegionWait polls a region operation until it's DONE.
func (c *APIClient) ComputeEngineOperationRegionWait(ctx context.Context, region RegionLocator, operationName string) (*ComputeEngineOperation, error) {
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled/timed out while waiting for region operation %s: %w", operationName, ctx.Err())
		default:
			if time.Since(startTime) > operationPollTimeout {
				return nil, fmt.Errorf("timeout waiting for region operation %s", operationName)
			}

			var op ComputeEngineOperation
			err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/operations/%s", regionURL(region), operationName), nil, nil, &op)
			if err != nil {
				var httpErr *HttpError
				if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
					return nil, fmt.Errorf("region operation %s not found: %w", operationName, err)
				}
				slog.Debug("Warning: error polling region operation. Retrying", "op", operationName, "err", err)
			} else {
				if op.Status == "DONE" {
					return &op, checkOperationError(op.Status, op.Error)
				}
			}

			time.Sleep(operationPollInterval)
		}
	}
}

// ComputeEngineOperationGlobalWait polls a global operation until it's DONE.
func (c *APIClient) ComputeEngineOperationGlobalWait(ctx context.Context, projectID string, operationName string) (*ComputeEngineOperation, error) {
	startTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled/timed out while waiting for global operation %s: %w", operationName, ctx.Err())
		default:
			if time.Since(startTime) > operationPollTimeout {
				return nil, fmt.Errorf("timeout waiting for global operation %s", operationName)
			}

			var op ComputeEngineOperation
			err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/global/operations/%s", projectURL(projectID), operationName), nil, nil, &op)
			if err != nil {
				var httpErr *HttpError
				if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
					return nil, fmt.Errorf("global operation %s not found: %w", operationName, err)
				}
				slog.Debug("Warning: error polling global operation. Retrying...", "op", operationName, "err", err)
			} else {
				if op.Status == "DONE" {
					return &op, checkOperationError(op.Status, op.Error)
				}
			}

			time.Sleep(operationPollInterval)
		}
	}
}

// --- Other Operation Getters ---

// ResourceManagerOperationGet gets the status of a Resource Manager operation.
func (c *APIClient) ResourceManagerOperationGet(ctx context.Context, operationName string) (*ResourceManagerOperation, error) {
	var op ResourceManagerOperation
	// operationName includes "operations/" prefix typically
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/%s", resourceManagerV1API, operationName), nil, nil, &op)
	if err != nil {
		return nil, err
	}
	if op.Done && op.Error != nil {
		return &op, &Error{Code: op.Error.Code, Message: op.Error.Message}
	}
	return &op, nil
}

// ServiceUsageOperationGet gets the status of a Service Usage operation.
func (c *APIClient) ServiceUsageOperationGet(ctx context.Context, operationName string) (*ServiceUsageOperation, error) {
	var op ServiceUsageOperation
	// operationName includes "operations/" prefix typically
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/%s", serviceUsageV1API, operationName), nil, nil, &op)
	if err != nil {
		return nil, err
	}
	if op.Done && op.Error != nil {
		return &op, &Error{Code: op.Error.Code, Message: op.Error.Message}
	}
	return &op, nil
}

// --- User Info ---

// GetUserInfo fetches OpenID Connect user profile information.
func (c *APIClient) GetUserInfo(ctx context.Context) (*UserInfo, error) {
	var userInfo UserInfo
	// UserInfo often uses GET, but POST is also supported by Google's endpoint
	err := c.doRequest(ctx, http.MethodGet, fmt.Sprintf("%s/userinfo", openidConnectV1API), nil, nil, &userInfo)
	if err != nil {
		return nil, err
	}
	return &userInfo, nil
}

func (p *Project) IsHealthy(ctx context.Context, client *APIClient) bool {
	bi, err := client.GetProjectBillingInfo(ctx, p.ProjectID)
	if err != nil {
		slog.Error("Error getting project billing info", "projectID", p.ProjectID, "error", err)
		return false
	}
	if bi == nil || bi.BillingEnabled == nil || !*bi.BillingEnabled {
		slog.Error("Project billing not enabled", "projectID", p.ProjectID)
		return false
	}
	services, err := client.ListEnabledServices(ctx, p.ProjectID)
	if err != nil {
		slog.Error("Error listing enabled services", "projectID", p.ProjectID, "error", err)
		return false
	}
	for _, service := range requiredServices {
		if !slices.ContainsFunc(services, func(s Service) bool {
			return strings.HasSuffix(s.Name, service)
		}) {
			slog.Error("Required service not enabled", "projectID", p.ProjectID, "service", service)
			return false
		}
	}
	return true
}

func (p *Project) CreateFirewallIfNeeded(ctx context.Context, client *APIClient) error {
	// Check if the firewall already exists
	firewalls, err := client.ListFirewalls(ctx, p.ProjectID, fmt.Sprintf("name=%s", firewallName))
	if err != nil {
		return fmt.Errorf("failed to list firewalls: %w", err)
	}
	if len(firewalls) > 0 {
		slog.Info("Firewall already exists", "firewallName", firewallName)
		return nil
	}

	// Create the firewall
	firewall := Firewall{
		Name:        firewallName,
		TargetTags:  []string{firewallName},
		Description: "Allow all",
		Priority:    1000,
		Direction:   "INGRESS",
		Allowed: []Rule{
			{IPProtocol: "all"},
		},
		SourceRanges: []string{"0.0.0.0/0"},
	}
	op, err := client.CreateFirewall(ctx, p.ProjectID, firewall)
	if err != nil {
		return fmt.Errorf("failed to create firewall: %w", err)
	}
	slog.Info("Firewall created successfully", "operationName", op.Name)
	return nil
}
func makeGcpInstanceName() string {
	now := time.Now().UTC()
	return fmt.Sprintf("lantern-%s-%s", now.Format("20060102"), now.Format("150405"))
}
func (p *Project) CreateInstance(ctx context.Context, client *APIClient, zoneID string, publicSSHKey string) (string, string, error) {
	name := makeGcpInstanceName()
	instance := Instance{
		Name:        name,
		MachineType: fmt.Sprintf("zones/%s/machineTypes/%s", zoneID, machineSize),
		Disks: []InstanceDisk{
			{
				Boot:             true,
				InitializeParams: DiskInitParams{SourceImage: "projects/ubuntu-os-cloud/global/images/family/ubuntu-2204-lts"},
			},
		},
		NetworkInterfaces: []InstanceNetworkInterface{
			{
				Network: "global/networks/default",
				// Empty accessConfigs necessary to allocate ephemeral IP
				AccessConfigs: []NetworkInterfaceAccessConfig{{}},
			},
		},
		Tags:   &InstanceTags{Items: []string{firewallName}},
		Labels: map[string]string{},
		Metadata: InstanceMetadata{Items: []InstanceMetadataItem{
			{
				Key:   "enable-guest-attributes",
				Value: "TRUE",
			},
			{
				Key:   "ssh-keys",
				Value: "ubuntu:" + publicSSHKey,
			},
		}},
	}
	loc := Locator{ZoneID: zoneID, ProjectID: p.ProjectID}
	op, err := client.CreateInstance(ctx, loc, instance)
	if err != nil {
		return "", "", fmt.Errorf("failed to create instance: %w", err)
	}
	_, err = client.ComputeEngineOperationZoneWait(ctx, loc, op.Name)
	if err != nil {
		return "", "", fmt.Errorf("failed waiting for instance creation: %w", err)
	}
	slog.Info("Instance created successfully", "operationName", op.Name)
	return name, op.TargetID, nil
}
