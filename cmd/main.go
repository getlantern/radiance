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
	rad, err := radiance.NewRadiance(nil)
	if err != nil {
		log.Fatalf("unable to create radiance: %v", err)
	}
	rad.StartVPN()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	<-sig

	rad.StopVPN()
}
