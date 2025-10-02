package servers

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/getlantern/sing-box-extensions/protocol"
	"github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badoption"
)

func validURL(value []byte) (*url.URL, error) {
	providedURL, err := url.Parse(string(value))
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}

	proto := providedURL.Scheme
	if proto == "ss" {
		proto = "shadowsocks"
	}

	if isSupported := slices.Contains(protocol.SupportedProtocols(), proto); !isSupported {
		return nil, fmt.Errorf("protocol not supported: %s", providedURL.Scheme)
	}
	return providedURL, nil
}

func parseURL(providedURL *url.URL) (Options, error) {
	o := Options{
		Outbounds: make([]option.Outbound, 0),
	}

	port, err := strconv.ParseUint(providedURL.Port(), 10, 16)
	if err != nil {
		return o, fmt.Errorf("couldn't parse server port: %w", err)
	}

	switch providedURL.Scheme {
	case "ss", "shadowsocks":
		ssOptions := option.ShadowsocksOutboundOptions{
			ServerOptions: option.ServerOptions{
				Server:     providedURL.Hostname(),
				ServerPort: uint16(port),
			},
		}
		decodedUsername, err := base64.StdEncoding.DecodeString(providedURL.User.Username())
		if err != nil {
			// If the username is not base64 encoded, use it directly
			ssOptions.Method = "none"
			ssOptions.Password = providedURL.User.Username()
		} else {
			splitUsername := strings.Split(string(decodedUsername), ":")
			if len(splitUsername) != 2 {
				return o, fmt.Errorf("couldn't parse shadowsocks method and password from username")
			}
			ssOptions.Method = splitUsername[0]
			ssOptions.Password = splitUsername[1]
		}

		o.Outbounds = append(o.Outbounds, option.Outbound{
			Type:    constant.TypeShadowsocks,
			Tag:     providedURL.Fragment,
			Options: ssOptions,
		})
	case "trojan":
		trojanOptions := option.TrojanOutboundOptions{
			Password: providedURL.User.Username(),
			ServerOptions: option.ServerOptions{
				Server:     providedURL.Hostname(),
				ServerPort: uint16(port),
			},
			OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{
				TLS: &option.OutboundTLSOptions{
					Enabled: true,
				},
			},
			Transport: parseV2RayTransportOptionsFromQuery(providedURL.Query()),
		}
		queryParams := providedURL.Query()
		if queryParams.Has("sni") {
			trojanOptions.OutboundTLSOptionsContainer.TLS.ServerName = queryParams.Get("sni")
		}
		if queryParams.Has("alpn") {
			trojanOptions.OutboundTLSOptionsContainer.TLS.ALPN = make(badoption.Listable[string], 0)
			trojanOptions.OutboundTLSOptionsContainer.TLS.ALPN = append(trojanOptions.OutboundTLSOptionsContainer.TLS.ALPN, queryParams.Get("alpn"))
		}
		if queryParams.Has("allowInsecure") && queryParams.Get("allowInsecure") == "1" {
			trojanOptions.OutboundTLSOptionsContainer.TLS.Insecure = true
		}

		o.Outbounds = append(o.Outbounds, option.Outbound{
			Type:    constant.TypeTrojan,
			Tag:     providedURL.Fragment,
			Options: trojanOptions,
		})
	case "vless":
		o.Outbounds = append(o.Outbounds, option.Outbound{
			Type: constant.TypeVLESS,
			Tag:  providedURL.Fragment,
			Options: option.VLESSOutboundOptions{
				UUID: providedURL.User.Username(),
				ServerOptions: option.ServerOptions{
					Server:     providedURL.Hostname(),
					ServerPort: uint16(port),
				},
				Transport: parseV2RayTransportOptionsFromQuery(providedURL.Query()),
			},
		})
	case "vmess":
		jsonEncoded, err := base64.StdEncoding.DecodeString(providedURL.Opaque)
		if err != nil {
			return o, fmt.Errorf("couldn't parse decode vmess base64: %w", err)
		}

		var vmessConfig vmess
		if err := json.Unmarshal(jsonEncoded, &vmessConfig); err != nil {
			return o, fmt.Errorf("couldn't parse vmess json: %w", err)
		}

		vmessOptions := option.VMessOutboundOptions{
			UUID:     vmessConfig.ID,
			Security: vmessConfig.Security,
			AlterId:  vmessConfig.Aid,
			ServerOptions: option.ServerOptions{
				Server:     vmessConfig.Addr,
				ServerPort: vmessConfig.Port,
			},
		}
		if vmessConfig.TLS == "tls" {
			vmessOptions.OutboundTLSOptionsContainer = option.OutboundTLSOptionsContainer{
				TLS: &option.OutboundTLSOptions{
					Enabled:    true,
					ServerName: vmessConfig.Sni,
					ALPN:       badoption.Listable[string]{vmessConfig.ALPN},
				},
			}
		}
		o.Outbounds = append(o.Outbounds, option.Outbound{
			Type:    constant.TypeVMess,
			Tag:     providedURL.Fragment,
			Options: vmessOptions,
		})
	}

	return o, nil
}

type vmess struct {
	Addr     string `json:"add"`
	Port     uint16 `json:"port"`
	Aid      int    `json:"aid"`
	ALPN     string `json:"alpn"`
	Fp       string `json:"fp"`
	Host     string `json:"host"`
	ID       string `json:"id"`
	Net      string `json:"net"`
	Path     string `json:"path"`
	PS       string `json:"ps"`
	Security string `json:"scy"`
	Sni      string `json:"sni"`
	TLS      string `json:"tls"`
	Type     string `json:"type"`
	V        string `json:"v"`
}

func parseV2RayTransportOptionsFromQuery(queryParams url.Values) *option.V2RayTransportOptions {
	switch queryParams.Get("type") {
	case "http":
		return &option.V2RayTransportOptions{
			Type: constant.V2RayTransportTypeHTTP,
			HTTPOptions: option.V2RayHTTPOptions{
				Host:   badoption.Listable[string]{queryParams.Get("host")},
				Path:   queryParams.Get("path"),
				Method: queryParams.Get("method"),
			},
		}
	case "httpupgrade", "xhttp":
		return &option.V2RayTransportOptions{
			Type: constant.V2RayTransportTypeHTTPUpgrade,
			HTTPUpgradeOptions: option.V2RayHTTPUpgradeOptions{
				Host: queryParams.Get("host"),
				Path: queryParams.Get("path"),
			},
		}
	case "ws", "wss":
		return &option.V2RayTransportOptions{
			Type: constant.V2RayTransportTypeWebsocket,
			WebsocketOptions: option.V2RayWebsocketOptions{
				Path: queryParams.Get("path"),
			},
		}
	case "grpc":
		return &option.V2RayTransportOptions{
			Type: constant.V2RayTransportTypeGRPC,
			GRPCOptions: option.V2RayGRPCOptions{
				ServiceName: queryParams.Get("serviceName"),
			},
		}
	case "quic":
		return &option.V2RayTransportOptions{
			Type:        constant.V2RayTransportTypeQUIC,
			QUICOptions: option.V2RayQUICOptions{},
		}
	case "tcp":
		return nil
	default:
		return nil
	}
}

func parseV2RayTransportOptionsFromVmessConfig(config vmess) *option.V2RayTransportOptions {
	switch config.Net {
	case "http":
		return &option.V2RayTransportOptions{
			Type: constant.V2RayTransportTypeHTTP,
			HTTPOptions: option.V2RayHTTPOptions{
				Host: badoption.Listable[string]{config.Host},
				Path: config.Path,
			},
		}
	case "httpupgrade", "xhttp":
		return &option.V2RayTransportOptions{
			Type: constant.V2RayTransportTypeHTTPUpgrade,
			HTTPUpgradeOptions: option.V2RayHTTPUpgradeOptions{
				Host: config.Host,
				Path: config.Path,
			},
		}
	case "ws", "wss":
		return &option.V2RayTransportOptions{
			Type: constant.V2RayTransportTypeWebsocket,
			WebsocketOptions: option.V2RayWebsocketOptions{
				Path: config.Path,
			},
		}
	case "grpc":
		return &option.V2RayTransportOptions{
			Type: constant.V2RayTransportTypeGRPC,
			GRPCOptions: option.V2RayGRPCOptions{
				ServiceName: config.Host,
			},
		}
	case "quic":
		return &option.V2RayTransportOptions{
			Type:        constant.V2RayTransportTypeQUIC,
			QUICOptions: option.V2RayQUICOptions{},
		}
	case "tcp":
		return nil
	default:
		return nil
	}
}
