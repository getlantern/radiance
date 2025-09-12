package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/servers"
	"github.com/getlantern/radiance/vpn"
)

func printUsage() {
	fmt.Println("Usage:")
	fmt.Println("  quickconnect [group]    - Connects to auto group (lantern, user, or all). If no group is specified, it defaults to 'all'.")
	fmt.Println("  connect <group> <tag>   - Connects to a specific server")
	fmt.Println("  disconnect              - Disconnects the VPN")
	fmt.Println("  status                  - Displays VPN status")
	fmt.Println("  servers                 - List available servers")
	fmt.Println("  help                    - Displays this help message")
	fmt.Println("  exit                    - Exits the CLI")
}

func main() {
	// rad, err := radiance.NewRadiance(radiance.Options{
	// 	DataDir:  filepath.Join("radiance", "data"),
	// 	LogDir:   "radiance",
	// 	LogLevel: "debug",
	// })
	// if err != nil {
	// 	slog.Error("creating radiance", "error", err)
	// 	os.Exit(1)
	// }
	//

	dataPath := filepath.Join("radiance", "data")
	common.Init(dataPath, "radiance", "debug", "some-device-id")
	exit := func() {
		status, _ := vpn.GetStatus()
		if status.TunnelOpen {
			vpn.Disconnect()
		}
		// rad.Close()
		os.Exit(0)
	}

	// Handle shutdown gracefully
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		exit()
	}()

	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("Radiance CLI started.")
	printUsage()

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) == 0 {
			fmt.Print("> ")
			continue
		}

		command := parts[0]
		args := parts[1:]

		switch command {
		case "help":
			printUsage()
		case "quickconnect":
			if len(args) > 1 {
				fmt.Println("Usage: quickconnect [group]")
				continue
			}
			var group string
			if len(args) == 1 {
				group = args[0]
			}
			if err := vpn.QuickConnect(group, nil); err != nil {
				fmt.Println("Error quick connecting:", err)
			} else {
				fmt.Println("Quick connect successful")
			}
		case "connect":
			if len(args) != 2 {
				fmt.Println("Usage: connect <group> <tag>")
				continue
			}
			group := args[0]
			tag := args[1]
			if err := vpn.ConnectToServer(group, tag, nil); err != nil {
				fmt.Println("Error connecting to server:", err)
			} else {
				fmt.Println("Successfully connected to server")
			}
		case "disconnect":
			if err := vpn.Disconnect(); err != nil {
				fmt.Println("Error disconnecting:", err)
			} else {
				fmt.Println("Disconnected successfully")
			}
		case "status":
			status, err := vpn.GetStatus()
			if err != nil {
				fmt.Println("Error getting status:", err)
			} else {
				fmt.Println("Tunnel Open:", status.TunnelOpen)
				fmt.Println("Selected Server:", status.SelectedServer)
				fmt.Println("Active Server:", status.ActiveServer)
			}
		case "servers":
			sMgr, _ := servers.NewManager(dataPath)
			servers := sMgr.Servers()
			for group, svrs := range servers {
				if len(svrs.Outbounds) > 0 || len(svrs.Endpoints) > 0 {
					fmt.Println(group)
				}
				for _, svr := range svrs.Outbounds {
					fmt.Println("  Type:", svr.Type)
					fmt.Println("  Tag:", svr.Tag)
					if loc, ok := svrs.Locations[svr.Tag]; ok {
						fmt.Printf("  Location: %s, %s\n", loc.City, loc.Country)
					}
				}
				for _, svr := range svrs.Endpoints {
					fmt.Println("  Type:", svr.Type)
					fmt.Println("  Tag:", svr.Tag)
					if loc, ok := svrs.Locations[svr.Tag]; ok {
						fmt.Printf("  Location: %s, %s\n", loc.City, loc.Country)
					}
				}
			}
		case "exit":
			exit()
		default:
			fmt.Println("Unknown command:", command)
		}
	}
}
