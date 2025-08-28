package ipc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// empty is a placeholder type for requests that do not expect a response body.
type empty struct{}

// sendRequest sends an HTTP request to the specified endpoint with the given method and data.
func sendRequest[T any](method, endpoint string, data any) (T, error) {
	buf, err := json.Marshal(data)
	var res T
	if err != nil {
		return res, fmt.Errorf("failed to marshal payload: %w", err)
	}
	req, err := http.NewRequest(method, apiURL+endpoint, bytes.NewReader(buf))
	if err != nil {
		return res, err
	}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: dialContext,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return res, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("received error response: %s", resp.Status)
	}
	if _, ok := any(&res).(*empty); ok {
		return res, nil
	}

	err = json.NewDecoder(resp.Body).Decode(&res)
	if err != nil {
		return res, fmt.Errorf("failed to decode response: %w", err)
	}
	return res, nil
}
