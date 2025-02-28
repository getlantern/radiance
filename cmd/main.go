package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance"
)

var log = golog.LoggerFor("main")

func main() {
	// addrFlag := flag.String("addr", "localhost:8080", "Address to listen on")
	// flag.Parse()
	//
	rad, err := radiance.NewRadiance()
	if err != nil {
		log.Fatalf("unable to create radiance: %v", err)
	}
	rad.StartVPN()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	<-sig

	rad.StopVPN()
}
