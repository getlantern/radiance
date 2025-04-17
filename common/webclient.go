package common

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
	"unicode"

	"fmt"
	"net/http"

	"github.com/go-resty/resty/v2"
	"github.com/moul/http2curl"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

var (
	APIBaseUrl      = "https://iantem.io/api/v1"
	ProServerUrl    = "https://api.getiantem.org"
	ContentTypeJSON = "application/json"
)

type WebClient interface {
	// GetPROTOC sends a GET request and parses the Protobuf response into the target object
	GetPROTOC(ctx context.Context, path string, params map[string]any, target protoreflect.ProtoMessage) error

	// PostPROTOC sends a POST request and parses the Protobuf response into the target object
	PostPROTOC(ctx context.Context, path string, body protoreflect.ProtoMessage, target protoreflect.ProtoMessage) error

	// Get sends a GET request and parses the response into the target object
	Get(ctx context.Context, path string, params map[string]any, target any) error

	// Post sends a POST request and parses the response into the target object
	Post(ctx context.Context, path string, params map[string]any, target any) error
}

type webClient struct {
	*resty.Client
}

// Opts are common options that RESTClient may be configured with
type Opts struct {
	// The OnAfterResponse option sets response middleware
	OnAfterResponse resty.ResponseMiddleware
	// BaseURL is the primary URL the client is configured with
	BaseURL string
	// The OnBeforeRequest option appends the given request middleware into the before request chain.
	OnBeforeRequest resty.PreRequestHook
	// HttpClient represents an http.Client that should be used by the resty client
	HttpClient *http.Client
	// Timeout represents a time limit for requests made by the web client
	Timeout time.Duration
}

// Construct an api client using the given httpClient (kindling)
func NewWebClient(opts *Opts) WebClient {
	if opts.HttpClient == nil {
		opts.HttpClient = &http.Client{}
	}
	c := resty.NewWithClient(opts.HttpClient)

	if opts.OnBeforeRequest != nil {
		c.SetPreRequestHook(opts.OnBeforeRequest)
	}
	if opts.OnAfterResponse != nil {
		c.OnAfterResponse(opts.OnAfterResponse)
	}
	if opts.BaseURL != "" {
		c.SetBaseURL(opts.BaseURL)
	}
	return &webClient{
		Client: c,
	}
}

// GetPROTOC sends a GET request and parses the Protobuf response into the target object
// path - the URL. Must start with a forward slash (/)
// params - the query parameters
// target - the target object to parse the response into
func (c *webClient) GetPROTOC(ctx context.Context, path string, params map[string]any, target protoreflect.ProtoMessage) error {
	req := c.R().SetContext(ctx)
	if params != nil {
		req.SetQueryParams(convertToStringMap(params))
	}
	//Overide the default content type
	// to application/x-protobuf
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Accept", ContentTypeJSON)
	resp, err := req.Get(path)

	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("unexpected status code %v", resp.StatusCode())
	}
	body := sanitizeResponseBody(resp.Body())
	return proto.Unmarshal(body, target)
}

// PostPROTOC sends a POST request and parses the Protobuf response into the target object
// path - the URL. Must start with a forward slash (/)
// msg - the message to send as body
// target - the target object to parse the response into
func (c *webClient) PostPROTOC(ctx context.Context, path string, msg, target protoreflect.ProtoMessage) error {
	bodyBytes, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	req := c.R().
		SetContext(ctx).
		SetBody(bodyBytes)

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Accept", "application/x-protobuf")

	// Execute request
	resp, err := req.Post(path)
	if err != nil {
		return fmt.Errorf("error sending POST request: %w", err)
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("unexpected status code %v", resp.StatusCode())
	}

	respBodyBytes := sanitizeResponseBody(resp.Body())
	return proto.Unmarshal(respBodyBytes, target)
}

// Get sends a GET request and parses the Protobuf response into the target object
// path - the URL. Must start with a forward slash (/)
// params - the query parameters
// target - the target object to parse the response into
func (c *webClient) Get(ctx context.Context, path string, params map[string]any, target any) error {
	req := c.R().SetContext(ctx)
	if params != nil {
		req.SetQueryParams(convertToStringMap(params))
	}
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Accept", ContentTypeJSON)

	resp, err := req.Get(path)

	command, _ := http2curl.GetCurlCommand(req.RawRequest)
	fmt.Printf("curl command: %v", command)

	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("unexpected status code %v body %v url %v", resp.StatusCode(), string(resp.Body()), resp.Request.URL)
	}

	body := sanitizeResponseBody(resp.Body())
	return json.Unmarshal(body, target)
}

// Post sends a POST request and parses the Protobuf response into the target object
// path - the URL. Must start with a forward slash (/)
// params - the query parameters
// target - the target object to parse the response into
func (c *webClient) Post(ctx context.Context, path string, params map[string]any, target any) error {
	req := c.R().SetContext(ctx)
	if params != nil {
		req.SetBody(params)
	}
	req.Header.Set("Content-Type", ContentTypeJSON)
	req.Header.Set("Accept", ContentTypeJSON)

	resp, err := req.Post(path)
	
	command, _ := http2curl.GetCurlCommand(req.RawRequest)
	fmt.Printf("curl command: %v", command)

	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("unexpected status code %v body %v url %v", resp.StatusCode(), string(resp.Body()), resp.Request.URL)
	}

	body := sanitizeResponseBody(resp.Body())
	slog.Info("Post response", "body", string(body), "url", resp.Request.URL)

	return json.Unmarshal(body, target)
}

func convertToStringMap(params map[string]interface{}) map[string]string {
	result := make(map[string]string)
	for key, val := range params {
		result[key] = fmt.Sprint(val)
	}
	return result
}

func sanitizeResponseBody(data []byte) []byte {
	var cleaned []byte
	for _, b := range data {
		if unicode.IsPrint(rune(b)) {
			cleaned = append(cleaned, b)
		}
	}
	return cleaned
}
