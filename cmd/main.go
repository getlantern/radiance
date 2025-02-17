package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		log.Fatalf("failed to create box: %v", err)
	}
	err = b.Start()
	if err != nil {
		log.Fatalf("failed to start box: %v", err)
	}
	log.Debug("box started")
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
		var err error
		log.Debug("stopping box..")
		go func() {
			err = b.Close()
			cancel()
		}()
		<-ctx.Done()
		switch ctx.Err() {
		case context.DeadlineExceeded:
			log.Error("timed out waiting for box to close")
		case context.Canceled:
			if err != nil {
				log.Errorf("failed to close box: %v", err)
			} else {
				log.Debug("box stopped")
			}
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	<-sig
}
