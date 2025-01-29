//go:build !linux

package vpn

import (
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/eycorsican/go-tun2socks/tun"
)

func openTunDevice(rConf *RoutingConfig) (io.ReadWriteCloser, error) {
	ip, ipNet, err := net.ParseCIDR(rConf.Gw)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CIDR %s: %w", rConf.Gw, err)
	}
	mask := net.IP(ipNet.Mask).To4().String()
	dns := strings.Split(rConf.Dns, ",")
	return tun.OpenTunDevice(
		rConf.TunName, rConf.TunIP, ip.To4().String(), mask, dns, false,
	)
}
