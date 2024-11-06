# Radiance
_(WIP)_

Radiance is a pilot PoC of Lantern core SDK (flashlight) utilizing the Outline SDK. 
What's the "core" idea behind a lantern, and I guess a flashlight? _Light_, or synonymously, _radiance_.

## Current State
Currently, `radiance` reads a proxy config from a file, `config/proxy.conf`.
##### protocols
- shadowsocks
- multiplexing

## Run

#### Retrieve proxy config
A new config will be needed each time the IP is rotated.

You need to be connected to `tailscale` (refer to lantern-cloud to setup and configure if needed).

The following commands assume you are in the `lantern-cloud` root directory:
Get an IP for one of the routes for `omanyte`. This is a shadowsocks test track. 
```
bin/lc routes list --track omanyte
```
then dump the config to a file as JSON (don't add the `--legacy` flag)
```
bin/lc route dump-config <ip> > ../radiance/config/proxy.conf
```

From the `radiance` directory, run 
```
go run cmd/main.go -addr localhost:8080
```

## TODO
- [x] Create an Outline transport StreamDialer using a proxy config. (shadowsocks w/ multiplex)
- [x] Connect to and route requests to backend proxy using a StreamDialer.
- [ ] Retrieve proxy config from backend.
- [ ] Implement remaining protocols
- [ ] Add socks5 support
- [ ] Implement VPN TUN 
- [ ] Add way to manage multiple proxies. MAB?
