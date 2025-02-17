package main

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/getlantern/golog"

	"github.com/getlantern/radiance/client"
)

var log = golog.LoggerFor("main")

func main() {
	// addrFlag := flag.String("addr", "localhost:8080", "Address to listen on")
	// flag.Parse()
	//
	// rad := radiance.NewRadiance()
	// if err := rad.Run(*addrFlag); err != nil {
	// 	log.Fatalf("Failed to run radiance: %v", err)
	// }

	b, err := client.NewBox()
	if err != nil {
		log.Fatalf("Failed to create box: %v", err)
	}
	b.Close()
	err = b.Start()
	if err != nil {
		log.Fatalf("Failed to start box: %v", err)
	}
	log.Debug("Box started")
	defer func() {
		log.Debug("Box stopping")
		err := b.Close()
		if err != nil {
			log.Errorf("Failed to close box: %v", err)
		}
		log.Debug("Box stopped")
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	<-sig
}
