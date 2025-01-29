package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	otransport "github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/getlantern/eventual/v2"
	"github.com/getlantern/golog"

	"github.com/getlantern/radiance"

	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/proxy"
	"github.com/getlantern/radiance/transport"
	"github.com/getlantern/radiance/transport/logger"
	"github.com/getlantern/radiance/vpn"
)

var log = golog.LoggerFor("main")

func main() {
	proxyAddr := flag.String("proxyAddr", "127.0.0.1:8080", "Address to listen on when running as a proxy")
	asVPN := flag.Bool("vpn", false, "Run as a vpn client")
	testVPN := flag.Bool("testvpn", false,
		"Start a proxy server, on 'proxyAddr', to test the VPN client. Only traffic from the proxy server "+
			"will be routed through the VPN client. The proxy server will be stopped when the VPN client is stopped.",
	)
	flag.Parse()
	log.Debugf("flag values: proxyAddr=%s, asVPN=%s, testVPN=%s", *proxyAddr, *asVPN, *testVPN)

	if !*asVPN {
		log.Debugf("Starting radiance on %s", *proxyAddr)

		rad := radiance.New()
		if err := rad.Run(*proxyAddr); err != nil {
			log.Fatalf("Failed to run radiance: %v", err)
		}
	} else {
		ch := config.NewConfigHandler(5 * time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		proxyConf, err := waitForConfig(ctx, ch)
		if err != nil {
			log.Fatalf("Failed to get config: %v", err)
		}

		sd, err := transport.DialerFrom(proxyConf)
		if err != nil {
			log.Fatalf("Failed to create dialer: %v", err)
		}

		var (
			dialer = sd
			addr   = proxyConf.Addr
			auth   = proxyConf.AuthToken
		)
		if *testVPN {
			// Start a local proxy server to test the VPN
			log.Debug("Starting test proxy server")
			p := proxy.New(sd, proxyConf.Addr, proxyConf.AuthToken, *proxyAddr)
			go p.Start()

			ep := otransport.StreamDialerEndpoint{
				Dialer:  &otransport.TCPDialer{},
				Address: *proxyAddr,
			}
			dialer = otransport.FuncStreamDialer(func(ctx context.Context, addr string) (otransport.StreamConn, error) {
				return ep.ConnectStream(ctx)
			})
			dialer, _ = logger.NewStreamDialer(dialer, proxyConf)
			addr = *proxyAddr
			auth = "testToken"
		}
		conf := vpn.RoutingConfig{
			TunName:      "radtun",
			TunIP:        "10.10.1.2",
			Gw:           "10.10.1.1/32",
			Dns:          "8.8.8.8",
			StartRouting: true,
		}
		client, err := vpn.NewClient(dialer, conf, addr, auth)
		if err != nil {
			log.Fatalf("Failed to create VPN client: %v", err)
		}

		if err := client.Start(); err != nil {
			log.Fatalf("Failed to start VPN client: %v", err)
		}
		defer client.Stop()
		log.Debug("VPN client started")
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	<-sig
}

func waitForConfig(ctx context.Context, ch *config.ConfigHandler) (*config.Config, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
			log.Debug("waiting for config")
		case <-time.After(400 * time.Millisecond):
			proxies, _ := ch.GetConfig(eventual.DontWait)
			if proxies != nil {
				return proxies, nil
			}
		}
	}
}
