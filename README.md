[![Go](https://github.com/getlantern/radiance/actions/workflows/go.yml/badge.svg)](https://github.com/getlantern/radiance/actions/workflows/go.yml)

# Radiance
_(WIP)_

Radiance is a pilot PoC of Lantern core SDK (flashlight) utilizing the [outline-sdk](github.com/Jigsaw-code/outline-sdk).
What's the "core" idea behind a lantern, and I guess a flashlight? _Light_, or synonymously, _radiance_.

## Current State
`radiance` runs a local server that proxies requests to a remote Lantern proxy. Requests are proxied over a `transport.StreamDialer`, which uses a specific protocol to communicate with the remote proxy, _i.e._ shadowsocks. `radiance` will automatically fetch the proxy and protocol information and configure the dialer. Currently, not all protocols are supported.

##### supported protocols
- shadowsocks
- multiplexing
- algeneva

### Add transports
New transports/protocols can be added by implementing [transport.StreamDialer](https://pkg.go.dev/github.com/Jigsaw-Code/outline-sdk@v0.0.17/transport#StreamDialer) and creating a [BuilderFn](https://github.com/getlantern/radiance/blob/main/transport/transport.go#L21). Create a new package in `transport` (_e.g._ `transport/myTransport`) and add the necessary code to run the transport here, including the `StreamDialer` and `BuilderFn`. Then add `registerDialerBuilder("myTransportName", myTransport.MyBuilderFn)` to `init` in [register.go](https://github.com/getlantern/radiance/blob/main/transport/register.go) to enable it. `myTransportName` must match [protocol](https://github.com/getlantern/radiance/blob/main/config/config.go#L16) in the proxy config as this is what's used to configure the dialer.


> [!NOTE]
> You should not need to make any other modifications to `register.go` or modify any other file.


## Run

```
go run cmd/main.go 
```
Sudo/Admin privileges are required to run.

## TODO
- [x] Create an Outline transport StreamDialer using a proxy config.
- [x] Connect to and route requests to backend proxy using a StreamDialer.
- [x] Retrieve proxy config from backend.
- [ ] Implement remaining protocols
  - [ ] tls w/ frag
  - [x] algeneva
  - [ ] tlsmasq
  - [ ] water
  - [ ] starbridge
  - [ ] vmess
  - [ ] broflake?
- [ ] Add socks5 support
- [ ] Implement VPN TUN 
- [ ] Add way to manage multiple proxies. MAB?
- [ ] Switch from getlantern/golog to slog
- [ ] Add PacketDialer (UDP) support
- [ ] Add support for split tunneling, using [v2ray's routing rule syntax](https://www.v2ray.com/en/configuration/routing.html)
