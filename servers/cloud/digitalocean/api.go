package digitalocean

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// APIClient implements DigitalOceanSession using the DigitalOcean REST API.
type APIClient struct {
	accessToken string
	client      *http.Client
}

// NewRestApiSession creates a new APIClient with the given access token.
func NewRestApiSession(accessToken string) *APIClient {
	return &APIClient{
		accessToken: accessToken,
		client:      &http.Client{Timeout: 30 * time.Second},
	}
}

// GetAccessToken returns the access token used by this session.
func (s *APIClient) GetAccessToken() string {
	return s.accessToken
}

// GetAccount retrieves the DigitalOcean account information.
func (s *APIClient) GetAccount(ctx context.Context) (*Account, error) {
	slog.Debug("Requesting account")
	var response struct {
		Account Account `json:"account"`
	}
	err := s.request(ctx, "GET", "account", nil, &response)
	if err != nil {
		return nil, err
	}
	return &response.Account, nil
}

// CreateDroplet creates a new DigitalOcean droplet.
func (s *APIClient) CreateDroplet(ctx context.Context, displayName, region, publicKeyForSSH string, dropletSpec DropletSpecification) (*DropletInfo, error) {
	dropletName := makeValidDropletName(displayName)
	// Register a key with DigitalOcean, so the user will not get a potentially
	// confusing email with their droplet password, which could get mistaken for
	// an invite.
	keyID, err := s.registerKey(ctx, dropletName, publicKeyForSSH)
	if err != nil {
		return nil, err
	}

	return s.makeCreateDropletRequest(ctx, dropletName, region, keyID, dropletSpec)
}

// makeCreateDropletRequest makes the actual API request to create a droplet.
func (s *APIClient) makeCreateDropletRequest(ctx context.Context, dropletName, region string, keyID int, dropletSpec DropletSpecification) (*DropletInfo, error) {
	maxRequests := 10
	retryTimeoutMs := 5000
	dropletID := 0
	for requestCount := 0; requestCount < maxRequests; requestCount++ {
		slog.Debug("Requesting droplet creation", "requestCount", requestCount, "maxRequests", maxRequests)

		// See https://docs.digitalocean.com/reference/api/api-reference/#operation/droplets_create
		data := map[string]interface{}{
			"name":               dropletName,
			"region":             region,
			"size":               dropletSpec.Size,
			"image":              dropletSpec.Image,
			"ssh_keys":           []int{keyID},
			"user_data":          dropletSpec.InstallCommand,
			"tags":               dropletSpec.Tags,
			"ipv6":               true,
			"monitoring":         false,
			"with_droplet_agent": false,
		}

		var response struct {
			Droplet DropletInfo `json:"droplet"`
		}

		err := s.request(ctx, "POST", "droplets", data, &response)
		if err == nil {
			dropletID = response.Droplet.ID
			break
		}
		slog.Error("Failed to create droplet", "error", err)
		if requestCount == maxRequests-1 {
			return nil, fmt.Errorf("failed to create droplet after %d attempts: %w", maxRequests, err)
		} else {
			time.Sleep(time.Duration(retryTimeoutMs) * time.Millisecond)
		}
	}
	slog.Debug("Droplet creation request sent", "dropletID", dropletID)

	for requestCount := 0; requestCount < maxRequests; requestCount++ {
		slog.Debug("Requesting droplet state", "requestCount", requestCount, "maxRequests", maxRequests)

		droplet, err := s.GetDroplet(ctx, dropletID)
		if err != nil {
			slog.Error("Failed to get droplet", "error", err)
			time.Sleep(time.Duration(retryTimeoutMs) * time.Millisecond)
		} else {
			slog.Debug("Droplet state", "status", droplet.Status)
			if droplet.Status == "active" {
				return droplet, nil
			}
			time.Sleep(time.Duration(retryTimeoutMs) * time.Millisecond)
		}
	}
	return nil, fmt.Errorf("droplet didn't become active after %d attempts", maxRequests)
}

// DeleteDroplet deletes a DigitalOcean droplet.
func (s *APIClient) DeleteDroplet(ctx context.Context, dropletID int) error {
	slog.Debug("Requesting droplet deletion")
	return s.request(ctx, "DELETE", fmt.Sprintf("droplets/%d", dropletID), nil, nil)
}

// GetRegionInfo retrieves information about DigitalOcean regions.
func (s *APIClient) GetRegionInfo(ctx context.Context) ([]RegionInfo, error) {
	slog.Debug("Requesting region info")
	var response struct {
		Regions []RegionInfo `json:"regions"`
	}
	err := s.request(ctx, "GET", "regions", nil, &response)
	if err != nil {
		return nil, err
	}
	return response.Regions, nil
}

// getSSHKeyID checks if an SSH key is already registered with DigitalOcean.
// It returns the key ID if it exists, or 0 if it doesn't.
func (s *APIClient) getSSHKeyID(ctx context.Context, publicSSHKey string) int {
	slog.Debug("Requesting ssh key registration check")
	var response struct {
		SSHKeys []struct {
			ID        int    `json:"id"`
			PublicKey string `json:"public_key"`
		} `json:"ssh_keys"`
	}
	err := s.request(ctx, "GET", "account/keys", nil, &response)
	if err != nil {
		slog.Error("Failed to get SSH key ID", "error", err)
		return 0

	}
	for _, key := range response.SSHKeys {
		if key.PublicKey == publicSSHKey {
			return key.ID
		}
	}
	slog.Debug("SSH key not registered")
	return 0
}

// registerKey registers an SSH key with DigitalOcean.
func (s *APIClient) registerKey(ctx context.Context, keyName, publicKeyForSSH string) (int, error) {
	// Check if the key is already registered
	keyID := s.getSSHKeyID(ctx, publicKeyForSSH)
	if keyID != 0 {
		slog.Debug("SSH key already registered", "keyID", keyID)
		return keyID, nil
	}
	slog.Debug("Requesting ssh key registration")
	data := map[string]string{
		"name":       keyName,
		"public_key": publicKeyForSSH,
	}
	var response struct {
		SSHKey struct {
			ID int `json:"id"`
		} `json:"ssh_key"`
	}
	err := s.request(ctx, "POST", "account/keys", data, &response)
	if err != nil {
		return 0, err
	}
	return response.SSHKey.ID, nil
}

// GetDroplet retrieves information about a specific DigitalOcean droplet.
func (s *APIClient) GetDroplet(ctx context.Context, dropletID int) (*DropletInfo, error) {
	slog.Debug("Requesting droplet")
	var response struct {
		Droplet DropletInfo `json:"droplet"`
	}
	err := s.request(ctx, "GET", fmt.Sprintf("droplets/%d", dropletID), nil, &response)
	if err != nil {
		return nil, err
	}
	return &response.Droplet, nil
}

// GetDropletTags retrieves the tags for a specific DigitalOcean droplet.
func (s *APIClient) GetDropletTags(ctx context.Context, dropletID int) ([]string, error) {
	droplet, err := s.GetDroplet(ctx, dropletID)
	if err != nil {
		return nil, err
	}
	return droplet.Tags, nil
}

// GetDroplets retrieves DigitalOcean droplets
// tag is optional and can be empty.
// name is optional and can be empty.
// tag and name are mutually exclusive.
func (s *APIClient) GetDroplets(ctx context.Context, tag string, name string) ([]DropletInfo, error) {
	slog.Debug("Requesting droplet", "tag", tag, "name", name)

	var response struct {
		Droplets []DropletInfo `json:"droplets"`
	}
	reqURL := "droplets?per_page=100"
	if tag != "" {
		reqURL += "&tag_name=" + url.QueryEscape(tag)
	}
	if name != "" {
		reqURL += "&name=" + url.QueryEscape(name)
	}
	err := s.request(ctx, "GET", reqURL, nil, &response)
	if err != nil {
		return nil, err
	}
	return response.Droplets, nil
}

// request makes an HTTP request to the DigitalOcean API.
func (s *APIClient) request(ctx context.Context, method, actionPath string, data interface{}, response interface{}) error {
	apiURL := fmt.Sprintf("https://api.digitalocean.com/v2/%s", actionPath)

	var reqBody io.Reader
	if data != nil {
		jsonData, err := json.Marshal(data)
		if err != nil {
			return err
		}
		reqBody = bytes.NewBuffer(jsonData)
	}

	req, err := http.NewRequestWithContext(ctx, method, apiURL, reqBody)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", s.accessToken))
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		slog.Debug("Failed to perform DigitalOcean request")
		return fmt.Errorf("error performing DigitalOcean request: %w", err)
	}
	defer resp.Body.Close()

	// DigitalOcean may return any 2xx status code for success.
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		// For requests like DELETE, the response may be empty
		if response != nil && resp.StatusCode != 204 {
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}

			err = json.Unmarshal(body, response)
			if err != nil {
				return fmt.Errorf("error parsing response body: %v", err)
			}
		}
		return nil
	} else if resp.StatusCode == 401 {
		slog.Debug("DigitalOcean request failed with Unauthorized error")
		return fmt.Errorf("DigitalOcean request failed with Unauthorized error")
	}

	var responseJSON struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}

	if err = json.NewDecoder(resp.Body).Decode(&responseJSON); err != nil {
		return fmt.Errorf("error parsing response body: %v", err)
	}

	slog.Debug("DigitalOcean request failed", "status", resp.StatusCode)
	return fmt.Errorf("fetch %s failed with %d: %s", responseJSON.ID, resp.StatusCode, responseJSON.Message)
}

// makeValidDropletName removes invalid characters from input name so it can be used with
// DigitalOcean APIs.
func makeValidDropletName(name string) string {
	// Remove all characters outside A-Z, a-z, 0-9 and '-'.
	re := regexp.MustCompile("[^A-Za-z0-9-]")
	return re.ReplaceAllString(name, "")
}
