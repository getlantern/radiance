package common

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/crypto/ssh"
)

func establishSSH(ip, privateKey, username string) (*ssh.Client, error) {
	signer, err := ssh.ParsePrivateKey([]byte(privateKey))
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}
	config := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}
	tryCounter := 0
	maxTries := 10
	var client *ssh.Client
	for {
		client, err = ssh.Dial("tcp", ip+":22", config)
		if err != nil {
			if tryCounter >= maxTries {
				return nil, fmt.Errorf("failed to dial SSH: %w after %d attempts", err, tryCounter)
			}
			tryCounter++
			slog.Debug("SSH connection failed, retrying...", "attempt", tryCounter, "error", err)
			time.Sleep(5 * time.Second)
			continue
		}
		break
	}
	slog.Debug("SSH connection established", "ip", ip)
	return client, nil
}

func runSSHCommand(client *ssh.Client, cmd string) ([]byte, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()
	output, err := session.CombinedOutput(cmd)
	if err != nil {
		return nil, fmt.Errorf("failed to run command %q: %w", cmd, err)
	}
	return output, nil
}

type ServerConfiguration struct {
	ExternalIp  string `json:"external_ip"`
	Port        int    `json:"port"`
	AccessToken string `json:"access_token"`
}

func InstallServer(ip string, privateSSHKey string, username string) (*ServerConfiguration, error) {
	client, err := establishSSH(ip, privateSSHKey, username)
	if err != nil {
		return nil, fmt.Errorf("failed to establish SSH connection: %w", err)
	}
	defer client.Close()

	// Run the installation commands
	commands := []string{
		"sudo cloud-init status --wait",
		"sudo sh -c 'echo deb [trusted=yes] https://apt.fury.io/getlantern/ / > /etc/apt/sources.list.d/getlantern.list'",
		"sudo apt-get update -y -q",
		"sudo apt-get install -y lantern-server-manager sing-box-extensions fail2ban firewalld",
		"sudo firewall-cmd --add-port 22/tcp --permanent",
		"sudo systemctl restart systemd-journald",
		"sudo systemctl enable --now firewalld lantern-server-manager fail2ban sing-box-extensions",
	}
	for _, cmd := range commands {
		output, err2 := runSSHCommand(client, cmd)
		if err2 != nil {
			return nil, err2
		}
		slog.Debug("Command output", "command", cmd, "output", string(output))
	}
	// TODO: add sysctl configuration
	// Wait for a few seconds to ensure the services are up and running
	time.Sleep(5 * time.Second)
	// Read the contents of /opt/lantern/data/server.json
	output, err := runSSHCommand(client, "sudo cat /opt/lantern/data/server.json")
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	slog.Debug("Config file contents", "config", string(output))

	// Parse the JSON output
	var config ServerConfiguration
	if err := json.Unmarshal(output, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}
	return &config, nil
}
