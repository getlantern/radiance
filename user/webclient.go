package user

import (
	"bytes"
	"context"
	"io"

	"fmt"
	"net/http"

	"github.com/getlantern/errors"
	"github.com/getlantern/golog"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var (
	log        = golog.LoggerFor("webclient")
	APIBaseUrl = "iantem.io/api/v1"
)

type WebClient interface {
	// GetPROTOC sends a GET request and parses the Protobuf response into the target object
	GetPROTOC(ctx context.Context, path string, params map[string]any, target protoreflect.ProtoMessage) error

	// PostPROTOC sends a POST request and parses the Protobuf response into the target object
	PostPROTOC(ctx context.Context, path string, body protoreflect.ProtoMessage, target protoreflect.ProtoMessage) error
}

type webClient struct {
	*http.Client
}

// Construct an api client using the given httpClient (kindling)
func NewWebClient(httpClient *http.Client) WebClient {
	return &webClient{
		Client: httpClient,
	}
}

// GetPROTOC sends a GET request and parses the Protobuf response into the target object
// path - the URL
// params - the query parameters
// target - the target object to parse the response into
func (c *webClient) GetPROTOC(ctx context.Context, path string, params map[string]any, target protoreflect.ProtoMessage) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	if err != nil {
		return errors.New("Error creating request: %v", err)
	}
	if params != nil {
		q := req.URL.Query()
		for key, value := range params {
			q.Add(key, fmt.Sprint(value))
		}

		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "application/x-protobuf")

	resp, err := c.Do(req)

	if err != nil {
		return errors.New("Error sending request: %v", err)
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("Unexpected status code %v", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.New("Error reading response body: %v", err)
	}
	return proto.Unmarshal(body, target)
}

// PostPROTOC sends a POST request and parses the Protobuf response into the target object
// path - the URL
// msg - the message to send as body
// target - the target object to parse the response into
func (c *webClient) PostPROTOC(ctx context.Context, path string, msg, target protoreflect.ProtoMessage) error {
	bodyBytes, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, io.NopCloser(bytes.NewReader(bodyBytes)))
	if err != nil {
		return errors.New("Error creating request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "application/x-protobuf")

	resp, err := c.Do(req)

	if err != nil {
		return errors.New("Error sending request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return errors.New("Unexpected status code %v", resp.StatusCode)
	}

	respBodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.New("Error reading response body: %v", err)
	}

	return proto.Unmarshal(respBodyBytes, target)
}
