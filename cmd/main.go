package main

import (
	"flag"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance"
)

var log = golog.LoggerFor("main")

func main() {
	addrFlag := flag.String("addr", "localhost:8080", "Address to listen on")
	flag.Parse()

	rad := radiance.Radiance{}
	if err := rad.Run(*addrFlag); err != nil {
		log.Fatalf("Failed to run radiance: %v", err)
	}
}
