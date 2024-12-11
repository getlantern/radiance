# Radiance
_(WIP)_

Radiance is a pilot PoC of Lantern core SDK (flashlight) utilizing the Outline SDK. 
What's the "core" idea behind a lantern, and I guess a flashlight? _Light_, or synonymously, _radiance_.

## Current State
`radiance` runs a local server that proxies requests to a remote Lantern proxy. `radiance` will fetch a config, containing the proxy and protocol information, from the backend server, which it will then use to configure the dialer. Currently, not all protocols are supported.

##### supported protocols
- shadowsocks
- multiplexing

## Run

```
go run cmd/main.go -addr localhost:8080
```

## TODO
- [x] Create an Outline transport StreamDialer using a proxy config. (shadowsocks w/ multiplex)
- [x] Connect to and route requests to backend proxy using a StreamDialer.
- [x] Retrieve proxy config from backend.
- [ ] Implement remaining protocols
- [ ] Add socks5 support
- [ ] Implement VPN TUN 
- [ ] Add way to manage multiple proxies. MAB?
- [ ] Switch from getlantern/golog to rs/zerolog w/ local log pkg
