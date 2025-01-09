package main

import (
	"flag"
	"net"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance"
)

var log = golog.LoggerFor("main")

func main() {
	addrFlag := flag.String("addr", "localhost:8080",
		"The address the proxy should listen on as host:port when running in proxy mode. In VPN mode, "+
			"this is the IP address that will be assigned to the TUN network interface. Example: '10.1.0.10'.",
	)
	asVPN := flag.Bool("vpn", false,
		"Run as VPN client. Must run as sudo on Linux/MacOS or as admin on Windows. Windows users "+
			"must have an OpenVPN client installed.",
	)
	flag.Parse()

	typ := radiance.Proxy_Router
	addr := *addrFlag
	if *asVPN {
		typ = radiance.VPN_Router
		ip := net.ParseIP(addr)
		if ip == nil || !ip.IsPrivate() {
			log.Fatalf("Invalid IP address: %v", addr)
		}
		addr = ip.String()
	}
	rad := radiance.New(radiance.RouterType(typ))
	if err := rad.Run(addr); err != nil {
		log.Fatalf("Failed to run radiance: %v", err)
	}
}
