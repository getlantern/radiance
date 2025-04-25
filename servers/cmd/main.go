package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"

	"golang.org/x/crypto/ssh"

	"github.com/getlantern/radiance/servers/cloud/common"
	"github.com/getlantern/radiance/servers/cloud/digitalocean"
	"github.com/getlantern/radiance/servers/cloud/gcp"
)

// Start tries to open the URL in a browser.
func Start(url string) error {
	var args []string
	switch runtime.GOOS {
	case "darwin":
		args = []string{"open"}
	case "windows":
		args = []string{"cmd", "/c", "start"}
	default:
		args = []string{"xdg-open"}
	}
	cmd := exec.Command(args[0], append(args[1:], url)...)
	return cmd.Start()
}

// MakeSSHKeyPair make a pair of public and private keys for SSH access.
func MakeSSHKeyPair() (string, string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return "", "", err
	}
	pub, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return "", "", err
	}
	privateKeyPEM := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}
	private := pem.EncodeToMemory(privateKeyPEM)
	public := ssh.MarshalAuthorizedKey(pub)

	return string(public), string(private), nil
}

func readCachedConnectInfo(filePath string) (string, string, string, error) {
	if d, err := os.ReadFile(filePath); err == nil {
		slog.Debug("Read token from file", "token", string(d))
		var data map[string]string
		if err := json.Unmarshal(d, &data); err != nil {
			slog.Error("Failed to unmarshal token", "err", err)
			return "", "", "", err
		}
		return data["token"], data["publicSSHKey"], data["privateSSHKey"], nil
	}
	return "", "", "", nil
}

func testDO() {
	ctx := context.Background()
	tok := ""
	publicSSHKey := ""
	privateSSHKey := ""
	var err error
	if tok, publicSSHKey, privateSSHKey, err = readCachedConnectInfo(tokenFileDO); tok == "" || err != nil {
		session := digitalocean.RunOauth(Start)
		publicSSHKey, privateSSHKey, err = MakeSSHKeyPair()
		if err != nil {
			slog.Error("Failed to create SSH key pair", "err", err)
			return
		}
		slog.Debug("Created SSH key pair", "publicSSHKey", publicSSHKey)

		// Wait for the result
		result := <-session.Result

		if result.Err != nil {
			slog.Error("OAuth failed", "err", result.Err)
		}
		tok = result.Token
		data := map[string]string{"token": tok, "publicSSHKey": publicSSHKey, "privateSSHKey": privateSSHKey}
		bdata, _ := json.Marshal(data)
		os.WriteFile(tokenFileDO, bdata, 0644)
	}

	slog.Debug("Successfully obtained OAuth token", "token", tok)

	// Use the token...
	account, err := digitalocean.GetAccount(tok)
	if err != nil {
		slog.Error("Failed to get account info with token", "err", err)
	}
	slog.Debug("Verified token", "email", account.Email)

	s := digitalocean.NewRestApiSession(tok)
	dropletID := 0
	var ip string
	if dis, err := s.GetDroplets(ctx, "", "tt1"); err == nil && len(dis) > 0 {
		dropletID = dis[0].ID
		ip = dis[0].Networks.V4[0].IPAddress
	} else {
		if di, err := s.CreateDroplet(ctx, "tt1", "nyc1", publicSSHKey, digitalocean.DropletSpecification{
			Size:  "s-1vcpu-1gb",
			Image: "ubuntu-22-04-x64",
		}); err != nil {
			slog.Error("Failed to create droplet", "err", err)
			return
		} else {
			slog.Debug("Created droplet", "dropletID", di.ID)
			dropletID = di.ID
			ip = di.Networks.V4[0].IPAddress
		}
	}
	slog.Debug("Droplet", "id", dropletID)
	if conf, err := common.InstallServer(ip, privateSSHKey, "root"); err != nil {
		slog.Error("Failed to install server", "err", err)
		return
	} else {
		slog.Debug("Installed server on droplet", "id", dropletID, "conf", conf)
	}
}

const tokenFileGCP = "/tmp/tmptokenGCP.json"
const tokenFileDO = "/tmp/tmptokenDO.json"

func testGCP() {
	tok := ""
	publicSSHKey := ""
	privateSSHKey := ""
	var err error
	if tok, publicSSHKey, privateSSHKey, err = readCachedConnectInfo(tokenFileGCP); tok == "" || err != nil {
		session := gcp.RunOauth(Start)
		publicSSHKey, privateSSHKey, err = MakeSSHKeyPair()
		if err != nil {
			slog.Error("Failed to create SSH key pair", "err", err)
			return
		}

		// Wait for the result
		result := <-session.Result

		if result.Err != nil {
			slog.Error("OAuth failed", "err", result.Err)
		}
		tok = result.Token
		data := map[string]string{"token": tok, "publicSSHKey": publicSSHKey, "privateSSHKey": privateSSHKey}
		bdata, _ := json.Marshal(data)
		os.WriteFile(tokenFileGCP, bdata, 0644)
	}

	slog.Debug("Successfully obtained OAuth token", "token", tok) // Print prefix only
	ctx := context.Background()
	client, err := gcp.NewAPIClient(ctx, tok)
	if err != nil {
		slog.Error("Failed to create GCP client", "err", err)
		return
	}
	projects, err := client.ListProjects(ctx, "")
	if err != nil {
		slog.Error("Failed to list projects", "err", err)
		return
	}

	// this is a hack to get the project id, in real app, show the list to the user
	projectIdx := slices.IndexFunc(projects, func(p gcp.Project) bool {
		return p.Name == "lantern-vpn"
	})
	if projectIdx == -1 {
		slog.Error("Project not found")
		return
	}
	project := projects[projectIdx]
	if !project.IsHealthy(ctx, client) {
		slog.Error("Project is not healthy", "err", err)
		return
	}
	if err := project.CreateFirewallIfNeeded(ctx, client); err != nil {
		slog.Error("Failed to create firewall", "err", err)
		return
	}
	locs, err := client.ListZones(ctx, project.ProjectID)
	if err != nil {
		slog.Error("Failed to list zones", "err", err)
		return
	}
	slog.Debug("got ", "locs", locs)

	zoneID := locs[0].Name
	instances, err := client.ListInstances(ctx, gcp.Locator{ProjectID: project.ProjectID, ZoneID: zoneID}, "")
	if err != nil {
		slog.Error("Failed to list instances", "err", err)
		return
	}

	instanceName := "lantern-20250521-142616"
	var instanceID string
	if idx := slices.IndexFunc(instances, func(i gcp.Instance) bool {
		return i.Name == instanceName
	}); idx != -1 {
		instanceID = instances[idx].ID
	} else {
		instanceName, instanceID, err = project.CreateInstance(ctx, client, zoneID, publicSSHKey)
		if err != nil {
			slog.Error("Failed to create instance", "err", err)
			return
		}
		slog.Debug("Created instance", "instanceName", instanceName, "instanceID", instanceID)
	}

	instance, err := client.GetInstance(ctx, gcp.Locator{ProjectID: project.ProjectID, ZoneID: zoneID, InstanceID: instanceID})
	if err != nil {
		slog.Error("Failed to get instance", "err", err)
		return
	}
	slog.Debug("Got instance", "instance", instance)
	zone := gcp.Zone{ID: zoneID}
	regionLocator := gcp.RegionLocator{ProjectID: project.ProjectID, RegionID: zone.GetRegionID()}
	var ip string
	if sip, err := client.GetStaticIP(ctx, regionLocator, instanceName); err != nil {
		natIP := ""
		if len(instance.NetworkInterfaces[0].AccessConfigs) > 0 {
			natIP = instance.NetworkInterfaces[0].AccessConfigs[0].NatIP
		}
		createData := gcp.StaticIpCreate{
			Address:     natIP,
			Name:        instanceName,
			Description: instance.Description,
		}
		if sip, err = client.CreateStaticIP(ctx, regionLocator, createData); err != nil {
			slog.Error("Failed to create static IP", "err", err)
			return
		}
		slog.Debug("Created static IP", "ip", sip.Address)
		ip = sip.Address
	} else {
		slog.Debug("Got static IP", "ip", sip)
		ip = sip.Address
	}
	if sc, err := common.InstallServer(ip, privateSSHKey, "ubuntu"); err != nil {
		slog.Error("Failed to create SSH key", "err", err)
		return
	} else {
		slog.Debug("Installed server on instance", "instance", instance, "conf", sc)
	}
}

// Example Usage
func main() {
	slog.SetLogLoggerLevel(slog.LevelDebug)
	//slog.Debug("Starting DigitalOcean OAuth flow...")
	testDO()
	//testGCP()
}
