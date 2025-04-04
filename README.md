[![Go](https://github.com/getlantern/radiance/actions/workflows/go.yml/badge.svg)](https://github.com/getlantern/radiance/actions/workflows/go.yml)

# Radiance
Radiance is the backend core of the Lantern client that integrates [sing-box](https://github.com/SagerNet/sing-box/) and the [Outline SDK ](https://github.com/Jigsaw-Code/outline-sdk)in addition to Lantern's own protocols and techniques. This is still under development and is not ready or even functional for production use.

What's the "core" idea behind a lantern? _Light_, or synonymously, _radiance_.

## What's it do?
`radiance` runs a TUN device on the user's device via [sing-tun](https://github.com/SagerNet/sing-tun/) and integrates all sing-box protocols (shadowsocks, hysteria2, vmess, anytls, etc) in addition to a proxyless dialer from the Outline SDK and [AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/). Radiance also integrates [application layer Geneva](https://www.youtube.com/watch?v=b9F696-oax0), compatibility with [WATER](https://github.com/refraction-networking/water-rs)/WASM-based transports, and a continuous stream of new Lantern protocols and approaches to stayin unblocked.

## Interoperability
Interoperability is at the core of Lantern and Radiance. Lantern is designed to interoperate with everything from Outline servers to sing-box servers to servers running Lantern's own sing-box extensions. You can similarly run Lantern servers to interoperate with any of those clients. The addition of WATER means that Lantern can deliver new protocols written in any WASM-compatible language at runtime without client-side updates.

## Run

```
go run cmd/main.go 
```
Sudo/Admin privileges are required to run.
