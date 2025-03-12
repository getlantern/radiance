[![Go](https://github.com/getlantern/radiance/actions/workflows/go.yml/badge.svg)](https://github.com/getlantern/radiance/actions/workflows/go.yml)

# Radiance
_(WIP)_

Radiance is the backend core of the Lantern client that integrates [sing-box](https://github.com/SagerNet/sing-box/) and the [Outline SDK ](https://github.com/Jigsaw-Code/outline-sdk)in addition to Lantern's own protocols and techniques.

What's the "core" idea behind a lantern? _Light_, or synonymously, _radiance_.

## What's it do?
`radiance` runs a TUN device on the user's device via [sing-tun](https://github.com/SagerNet/sing-tun/) and integrates all sing-box protocols (shadowsocks, vmess, anytls, etc) in addition to a proxyless dialer from the Outline SDK and [AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/). Radiance also integrates [application layer Geneva](https://www.youtube.com/watch?v=b9F696-oax0), with more Lantern protocols coming soon.


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
  - [x] tls w/ frag
  - [x] algeneva
  - [ ] tlsmasq
  - [ ] water
  - [ ] starbridge
  - [x] vmess
  - [ ] broflake?
- [x] Implement VPN TUN 
- [x] Add way to manage multiple proxies.
- [ ] Switch from getlantern/golog to slog
- [ ] Add PacketDialer (UDP) support
- [x] Add support for split tunneling, using [v2ray's routing rule syntax](https://www.v2ray.com/en/configuration/routing.html)
