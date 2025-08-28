package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getlantern/radiance/vpn/ipc"
)

func main() {
	svr := ipc.NewServer(nil)
	if err := svr.Start(""); err != nil {
		log.Fatal(err)
	}
	defer svr.Close()

	time.Sleep(100 * time.Millisecond) // wait for server to start
	metrics, err := ipc.GetMetrics()
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Memory: %d bytes", metrics.Memory)
	log.Printf("Goroutines: %d", metrics.Goroutines)
	log.Printf("Connections: %d", metrics.Connections)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
