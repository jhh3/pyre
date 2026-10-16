package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// RDPInfo is how to reach a Windows node's desktop. Password is set only
// right after --reset-password and is shown once; Pyre never stores or
// logs it.
type RDPInfo struct {
	Name        string `json:"name"`
	Address     string `json:"address"`      // tailnet IP (or MagicDNS name)
	TailnetName string `json:"tailnet_name"` // MagicDNS name when known
	User        string `json:"user"`
	Password    string `json:"password,omitempty"`
	RDPFile     string `json:"rdp_file"` // .rdp file contents (no credentials)
	Online      bool   `json:"online"`
}

// TailnetPeer finds a device by hostname in the local `tailscale status
// --json` (this machine must be on the tailnet).
func TailnetPeer(ctx context.Context, ts Runner, hostname string) (addr, dnsName string, online bool, err error) {
	out, err := ts.Run(ctx, "status", "--json")
	if err != nil {
		return "", "", false, fmt.Errorf("tailscale status on this machine: %w", err)
	}
	var st struct {
		Peer map[string]struct {
			HostName     string   `json:"HostName"`
			DNSName      string   `json:"DNSName"`
			TailscaleIPs []string `json:"TailscaleIPs"`
			Online       bool     `json:"Online"`
		} `json:"Peer"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return "", "", false, fmt.Errorf("tailscale status: %w", err)
	}
	for _, p := range st.Peer {
		if !strings.EqualFold(p.HostName, hostname) && !strings.HasPrefix(strings.ToLower(p.DNSName), strings.ToLower(hostname)+".") {
			continue
		}
		dns := strings.TrimSuffix(p.DNSName, ".")
		for _, ip := range p.TailscaleIPs {
			if !strings.Contains(ip, ":") {
				return ip, dns, p.Online, nil
			}
		}
		return dns, dns, p.Online, nil
	}
	return "", "", false, contracts.Errorf(contracts.ErrNotFound, "%s is not on this machine's tailnet (is the node running, and this machine logged in to the same tailnet?)", hostname)
}

// RDPFileContent is a minimal .rdp file without credentials.
func RDPFileContent(addr, user string) string {
	return strings.Join([]string{
		"full address:s:" + addr,
		"username:s:" + user,
		"prompt for credentials:i:1",
		"screen mode id:i:2",
		"dynamic resolution:i:1",
		"audiomode:i:2",
		"redirectclipboard:i:1",
		"",
	}, "\r\n")
}

// RDP returns the connection details of a Windows node; with reset it
// also sets a new password for user through `gcloud compute
// reset-windows-password` (no inbound port needed) and, when user is the
// node's auto-logon user, hands the new password to the VM's auto-logon
// refresher through a metadata key removed again right after.
func (m *Manager) RDP(ctx context.Context, ts Runner, name, user string, reset bool, handoff time.Duration) (RDPInfo, error) {
	in, err := m.describe(ctx, name)
	if err != nil {
		return RDPInfo{}, err
	}
	if in.Labels[LabelOS] != OSWindows {
		return RDPInfo{}, contracts.Errorf(contracts.ErrUnsupported, "%s is a Linux node: Remote Desktop is for Windows nodes (use ssh %s@%s over Tailscale SSH)", name, m.Cfg.GCP.user(), InstanceName(name))
	}
	if in.Status != "RUNNING" {
		return RDPInfo{}, contracts.Errorf(contracts.ErrUnavailable, "%s is %s: `pyre node start %s` first", name, in.Status, name)
	}
	if user == "" {
		user = m.Cfg.GCP.user()
	}
	info := RDPInfo{Name: name, User: user, TailnetName: InstanceName(name)}
	if ts != nil {
		addr, dns, online, err := TailnetPeer(ctx, ts, InstanceName(name))
		if err != nil {
			return RDPInfo{}, err
		}
		info.Address, info.Online = addr, online
		if dns != "" {
			info.TailnetName = dns
		}
	} else {
		info.Address = InstanceName(name)
	}
	if reset {
		out, err := m.GCP.run(ctx, "compute", "reset-windows-password", in.Name, "--zone="+m.Zone, "--user="+user, "--format=json")
		if err != nil {
			return RDPInfo{}, err
		}
		var pw struct {
			Password string `json:"password"`
			Username string `json:"username"`
		}
		if err := json.Unmarshal(out, &pw); err != nil || pw.Password == "" {
			return RDPInfo{}, fmt.Errorf("gcloud reset-windows-password returned no password")
		}
		info.Password = pw.Password
		if pw.Username != "" {
			info.User = pw.Username
		}
		if strings.EqualFold(info.User, m.Cfg.GCP.user()) {
			if err := m.handAutologon(ctx, in.Name, pw.Password, handoff); err != nil {
				m.logf("warning: auto-logon may use the old password after the next reboot: %v", err)
			}
		}
	}
	info.RDPFile = RDPFileContent(info.Address, info.User)
	return info, nil
}

func (m *Manager) handAutologon(ctx context.Context, inst, password string, handoff time.Duration) error {
	dir, cleanup, err := m.privateDir()
	if err != nil {
		return err
	}
	defer cleanup()
	f, err := writePrivate(dir, MetaAutologonPassword, []byte(password))
	if err != nil {
		return err
	}
	if err := m.GCP.AddMetadataFiles(ctx, m.Zone, inst, map[string]string{MetaAutologonPassword: f}); err != nil {
		return err
	}
	if handoff <= 0 {
		handoff = 20 * time.Second // the VM checks every 5 s
	}
	serr := sleep(ctx, handoff)
	rctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.GCP.RemoveMetadata(rctx, m.Zone, inst, []string{MetaAutologonPassword}); err != nil {
		return fmt.Errorf("remove %s from metadata: %w (remove it by hand)", MetaAutologonPassword, err)
	}
	return serr
}
