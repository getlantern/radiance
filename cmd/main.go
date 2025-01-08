package main

import (
	"flag"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance"
)

var log = golog.LoggerFor("main")

func main() {
	addrFlag := flag.String("addr", "localhost:8080", "Address to listen on")
	typ := flag.String("type", "proxy", "Type of router to use (proxy or vpn)")
	flag.Parse()

	rad := radiance.New(radiance.RouterType(*typ))
	if err := rad.Run(*addrFlag); err != nil {
		log.Fatalf("Failed to run radiance: %v", err)
	}
}
