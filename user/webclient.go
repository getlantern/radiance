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
	ProAPIHost = "api.getiantem.org"

	DFBaseUrl  = "df.iantem.io/api/v1"
	APIBaseUrl = "iantem.io/api/v1"
)

type RESTClient interface {
	// Get data from server and parse to protoc file
	GetPROTOC(ctx context.Context, path string, params map[string]interface{}, target protoreflect.ProtoMessage) error

	// PostPROTOC sends a POST request with protoc file and parse the response to protoc file
	PostPROTOC(ctx context.Context, path string, body protoreflect.ProtoMessage, target protoreflect.ProtoMessage) error
}

type restClient struct {
	*http.Client
}

// Construct a REST client using the given SendRequest function
func NewRESTClient(httpClient *http.Client) RESTClient {
	return &restClient{
		Client: httpClient,
	}
}

func (c *restClient) GetPROTOC(ctx context.Context, path string, params map[string]interface{}, target protoreflect.ProtoMessage) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
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

	resp, err := http.DefaultClient.Do(req)

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

func (c *restClient) PostPROTOC(ctx context.Context, path string, msg protoreflect.ProtoMessage, target protoreflect.ProtoMessage) error {
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

	resp, err := http.DefaultClient.Do(req)

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
