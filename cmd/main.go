package main

import (
	"flag"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance"
)

var log = golog.LoggerFor("main")

func main() {
	addrFlag := flag.String("addr", "localhost:8080", "Address to listen on")
	proxylessConfigFlag := flag.String("proxyless-config", "disorder:0|split:123", "Proxyless config for used for proxyless mode. You can find examples here: https://pkg.go.dev/github.com/Jigsaw-Code/outline-sdk/x/configurl#hdr-Examples")
	flag.Parse()

	rad := radiance.NewRadiance()
	if err := rad.Run(*addrFlag, proxylessConfigFlag); err != nil {
		log.Fatalf("Failed to run radiance: %v", err)
	}
}
