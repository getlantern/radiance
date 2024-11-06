package main

import (
	"flag"
	"os"
	"os/signal"

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

	// Wait for interrupt signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	log.Debug("Shutting down radiance")
	if err := rad.Shutdown(); err != nil {
		log.Fatalf("Failed to shutdown radiance: %v", err)
	}
}
