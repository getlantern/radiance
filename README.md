[![Go](https://github.com/getlantern/radiance/actions/workflows/go.yml/badge.svg)](https://github.com/getlantern/radiance/actions/workflows/go.yml)

# Radiance
Radiance is the backend core of the [Lantern client](https://github.com/getlantern/lantern-outline) that integrates [sing-box](https://github.com/SagerNet/sing-box/) and the [Outline SDK ](https://github.com/Jigsaw-Code/outline-sdk)in addition to Lantern's own protocols and techniques. This is still under development and is not ready or even functional for production use.

See [Lantern client](https://github.com/getlantern/lantern-outline) for build instructions.

What's the "core" idea behind a lantern? _Light_, or synonymously, _radiance_.

## What's it do?
`radiance` runs a TUN device on the user's device via [sing-tun](https://github.com/SagerNet/sing-tun/) and integrates all sing-box protocols (shadowsocks, hysteria2, vmess, anytls, etc) in addition to a proxyless dialer from the Outline SDK and [AmneziaWG](https://docs.amnezia.org/documentation/amnezia-wg/). Radiance also integrates [application layer Geneva](https://www.youtube.com/watch?v=b9F696-oax0), compatibility with [WATER](https://github.com/refraction-networking/water-rs)/WASM-based transports, and a continuous stream of new Lantern protocols and approaches to stayin unblocked.

## Interoperability
Interoperability is at the core of Lantern and Radiance. Lantern is designed to interoperate with everything from Outline servers to sing-box servers to servers running Lantern's own sing-box extensions. You can similarly run Lantern servers to interoperate with any of those clients. The addition of WATER means that Lantern can deliver new protocols written in any WASM-compatible language at runtime without client-side updates.

## Environment Variables
Configuration can be controlled via a `.env` file in the root of the project directory or by setting environment variables. The order of precedence for setting these is as follows:

1.  Environment variables (highest precedence)
2.  `.env` file (lowest precedence)

The following variables are available:

*   `RADIANCE_LOG_LEVEL`: Sets the log level (e.g., `trace`, `debug`, `info`, `warn`, `error`, `fatal`).
*   `RADIANCE_LOG_PATH`: Sets the absolute path to the log file.
*   `RADIANCE_DATA_PATH`: Sets the absolute path to the data directory.
*   `RADIANCE_DISABLE_FETCH_CONFIG`: If set to `true`, disables fetching the remote config.

## Packages

Use `common.Init` to setup directories and configure loggers. 
> [!note]
> This isn't necessary if `NewRadiance` was called as it will call `Init` for you.

### `vpn`

The `vpn` package provides high-level functions for controlling the VPN tunnel. 

To connect to the best available server, you can use the `QuickConnect` function. This function takes a server group (`servers.SGLantern`, `servers.SGUser`, or `"all"`) and a `PlatformInterface` as input. For example:

```go
err := vpn.QuickConnect(servers.SGLantern, platIfce)
```

will connect to the best Lantern server, while:

```go
err := vpn.QuickConnect("all", platIfce)
```

will connect to the best overall.

You can also connect to a specific server using `ConnectToServer`. This function requires a server group, a server tag, and a `PlatformInterface`. For example:

```go
err := vpn.ConnectToServer(servers.SGUser, "my-server", platIfce)
```

Both `QuickConnect` and `ConnectToServer` can be called without disconnecting first, allowing you to seamlessly switch between servers or connection modes.

Once connected, you can check the `GetStatus` or view `ActiveConnections`. To stop the VPN, simply call `Disconnect`. The package also supports reconnecting to the last used server with `Reconnect`.

This package also includes split tunneling capabilities, allowing you to include or exclude specific applications, domains, or IP addresses from the VPN tunnel. You can manage split tunneling by creating a `SplitTunnel` handler with `NewSplitTunnelHandler`. This handler allows you to `Enable` or `Disable` split tunneling, `AddItem` or `RemoveItem` from the filter, and view the current `Filters`.

### `servers`

The `servers` package is responsible for managing all VPN server configurations, separating them into two groups: `lantern` (official Lantern servers) and `user` (user-provided servers).

The `Manager` allows you to `AddServers` and `RemoveServer` configurations. You can retrieve the config for a specific server with `GetServerByTag` or use `Servers` to retrieve all configs.

> [!caution]
> While you can get a new `Manager` instance with `NewManager`, it is recommended to use `Radiance.ServerManager`. This will return the shared manager instance. `NewManager` can be useful for retrieving server information if you don't have access to the shared instance, but the new instance should not be kept as it won't stay in sync and adding server configs to it will overwrite existing configs if both manager instances are pointed to the same server file.

A key feature of this package is the ability to add private servers from a server manager via an access token using `AddPrivateServer`. This process uses Trust-on-first-use (TOFU) to securely add the server. Once a private server is added, you can use the manager to invite other users to it with `InviteToPrivateServer` and revoke access with `RevokePrivateServerInvite`.

