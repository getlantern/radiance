package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/getlantern/radiance"
	"github.com/getlantern/radiance/common/settings"
	"github.com/getlantern/radiance/config"
	"github.com/getlantern/radiance/events"
	"github.com/getlantern/radiance/vpn"
)

func performLanternPing(urlToHit string, runId string, deviceId string, userId int64, token string, dataDir string, isSticky bool) error {
	if !isSticky {
		os.RemoveAll(dataDir)
	}
	os.MkdirAll(dataDir, 0o755)
	r, err := radiance.NewRadiance(radiance.Options{
		DataDir: dataDir,
		LogDir:  dataDir,
		Locale:  "en-US",
	})
	if err != nil {
		return fmt.Errorf("failed to create radiance instance: %w", err)
	}
	defer r.Close()
	settings.Set(settings.UserIDKey, userId)
	settings.Set(settings.TokenKey, token)
	settings.Set(settings.UserLevelKey, "")
	settings.Set(settings.EmailKey, "pinger@pinger.com")
	settings.Set(settings.DevicesKey, []settings.Device{
		{
			ID:   deviceId,
			Name: deviceId,
		},
	})

	ipcServer, err := vpn.InitIPC(dataDir, "", "trace", nil)
	if err != nil {
		return fmt.Errorf("failed to initialize IPC server: %w", err)
	}
	exit := func() {
		status, _ := vpn.GetStatus()
		if status.TunnelOpen {
			vpn.Disconnect()
		}
		ipcServer.Close()
	}
	defer exit()

	// in sticky mode we assume config exists. otherwise we need to wait for a new one to arrive
	if !isSticky {
		resultCh := make(chan error)
		go events.SubscribeOnce(func(evt config.NewConfigEvent) {
			fmt.Printf("Received new config event: %+v\n", evt)
			resultCh <- nil
		})
		// wait for something to arrive to resultCh or timeout
		select {
		case <-resultCh:
			break
		case <-time.After(30 * time.Second):
			slog.Error("Timeout waiting for config")
			return fmt.Errorf("timeout waiting for config")
		}
	}
	t1 := time.Now()
	if err = vpn.QuickConnect("all", nil); err != nil {
		return fmt.Errorf("quick connect failed: %w", err)
	}
	fmt.Println("Quick connect successful")

	t2 := time.Now()

	proxyAddr := os.Getenv("RADIANCE_SOCKS_ADDRESS")
	if proxyAddr == "" {
	  proxyAddr = "127.0.0.1:6666"
	}
	cmd := exec.Command("curl", "-v", "-x", proxyAddr, "-s", urlToHit)

	// Run the command and capture the output
	outputB, err := cmd.Output()
	if err != nil {
		fmt.Println("Error executing command:", err)
		return err
	}

	body := string(outputB)

	t3 := time.Now()

	fmt.Println("lantern ping completed successfully")
	// create a marker file that will be used by the pinger to determine success
	_ = os.WriteFile(dataDir+"/success", []byte(""), 0o644)
	_ = os.WriteFile(dataDir+"/output.txt", []byte(body), 0o644)
	return os.WriteFile(dataDir+"/timing.txt", []byte(fmt.Sprintf(`
	result: %v
	run-id: %s
	err: %v
	started: %s
	connected: %d
	fetched: %d
	url: %s`,
		true, runId, nil, t1, int32(t2.Sub(t1).Milliseconds()), int32(t3.Sub(t1).Milliseconds()), urlToHit)), 0o644)
}

func main() {
	deviceId := os.Getenv("DEVICE_ID")
	userId := os.Getenv("USER_ID")
	token := os.Getenv("TOKEN")
	runId := os.Getenv("RUN_ID")
	targetUrl := os.Getenv("TARGET_URL")
	data := os.Getenv("DATA")
	isSticky := os.Getenv("STICKY") == "true"

	if deviceId == "" || userId == "" || token == "" || runId == "" || targetUrl == "" || data == "" {
		fmt.Println("missing required environment variable(s)")
		fmt.Println("Required environment variables: DEVICE_ID, USER_ID, TOKEN, RUN_ID, TARGET_URL, DATA")
		os.Exit(1)
	}

	uid, err := strconv.ParseInt(userId, 10, 64)
	if err != nil {
		fmt.Println("failed to parse USER_ID")
		os.Exit(1)
	}

	if err := performLanternPing(targetUrl, runId, deviceId, uid, token, data, isSticky); err != nil {
		fmt.Printf("failed to perform lantern ping: %v\n", err)
		os.Exit(1)
	}
}
