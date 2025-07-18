package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"unicode"

	"fmt"
	"net/http"

	"github.com/go-resty/resty/v2"
	"google.golang.org/protobuf/proto"

	"github.com/getlantern/radiance/app"
	"github.com/getlantern/radiance/backend"
)

type webClient struct {
	client *resty.Client
}

func newWebClient(httpClient *http.Client, baseURL string) *webClient {
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	client := resty.NewWithClient(httpClient)
	if baseURL != "" {
		client.SetBaseURL(baseURL)
	}

	client.SetHeaders(map[string]string{
		backend.AppNameHeader:  app.Name,
		backend.VersionHeader:  app.Version,
		backend.PlatformHeader: app.Platform,
	})

	// Add a request middleware to marshal the request body to protobuf or JSON
	client.OnBeforeRequest(func(c *resty.Client, req *resty.Request) error {
		if req.Body == nil {
			return nil
		}
		if pb, ok := req.Body.(proto.Message); ok {
			data, err := proto.Marshal(pb)
			if err != nil {
				return err
			}
			req.Body = data
			req.Header.Set("Content-Type", "application/x-protobuf")
			req.Header.Set("Accept", "application/x-protobuf")
		} else {
			data, err := json.Marshal(req.Body)
			if err != nil {
				return err
			}
			req.Body = data
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
		}

		return nil
	})

	// Add a response middleware to unmarshal the response body from protobuf or JSON
	client.OnAfterResponse(func(c *resty.Client, resp *resty.Response) error {
		if len(resp.Body()) == 0 || resp.Request.Result == nil {
			return nil
		}
		switch ct := resp.RawResponse.Header.Get("Content-Type"); ct {
		case "application/x-protobuf":
			pb, ok := resp.Request.Result.(proto.Message)
			if !ok {
				return fmt.Errorf("response body is not a protobuf message")
			}
			return proto.Unmarshal(resp.Body(), pb)
		case "application/json":
			body := sanitizeResponseBody(resp.Body())
			return json.Unmarshal(body, resp.Request.Result)
		}
		return nil
	})
	return &webClient{client: client}
}

func (wc *webClient) NewRequest(queryParams, headers map[string]string, body any) *resty.Request {
	return wc.client.NewRequest().SetQueryParams(queryParams).SetHeaders(headers).SetBody(body)
}

func (wc *webClient) Get(ctx context.Context, path string, req *resty.Request, res any) error {
	return wc.send(ctx, resty.MethodGet, path, req, res)
}

func (wc *webClient) Post(ctx context.Context, path string, req *resty.Request, res any) error {
	return wc.send(ctx, resty.MethodPost, path, req, res)
}

func (wc *webClient) send(ctx context.Context, method, path string, req *resty.Request, res any) error {
	if req == nil {
		req = wc.client.NewRequest()
	}
	req.SetContext(ctx)
	if res != nil {
		req.SetResult(res)
	}

	resp, err := req.Execute(method, path)
	if err != nil {
		return fmt.Errorf("error sending request: %w", err)
	}

	// print cutl command for debugging
	fmt.Println(req.GenerateCurlCommand())

	if resp.StatusCode() < 200 || resp.StatusCode() >= 300 {
		slog.Debug("error sending request", "status", resp.StatusCode(), "body", string(resp.Body()))
		return fmt.Errorf("unexpected status %v body %s ", resp.StatusCode(), string(resp.Body()))
	}
	return nil
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
