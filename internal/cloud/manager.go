package cloud

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Hub is what the manager needs from the Pyre hub (the CLI adapts Ops).
type Hub interface {
	CreateEnrollment(ctx context.Context, name string, ttl time.Duration) (contracts.EnrollmentTicket, error)
	// FindNode returns the non-revoked node named name, or nil.
	FindNode(ctx context.Context, name string) (*contracts.Node, error)
	NodeSessions(ctx context.Context, nodeID string) ([]contracts.SessionView, error)
	// SessionWorktrees scans live when the node answers, else returns the
	// last report.
	SessionWorktrees(ctx context.Context, sv contracts.SessionView, live bool) (contracts.SessionWorktrees, error)
	RevokeNode(ctx context.Context, nodeID, reason string) error
}

// Manager provisions and manages cloud nodes.
type Manager struct {
	GCP    *GCP
	Hub    Hub
	Keys   KeyMinter // nil: no Tailscale key source configured
	Cfg    Config
	HubPin string // the client's pin for the hub (pins ignore hostnames)
	// AllowedSigners is the release public key file content.
	AllowedSigners string
	Zone           string
	Out            io.Writer // progress (stderr)
	Poll           time.Duration
	Now            func() time.Time
	TempDir        string // parent for the private secret dir (default os.TempDir)
	Cache          *Cache // optional local snapshot for the TUI
}

var nameRe = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,40}[a-z0-9])?$`)

// InstanceName is the VM name of node name.
func InstanceName(name string) string { return InstancePrefix + name }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) logf(format string, a ...any) {
	if m.Out != nil {
		fmt.Fprintf(m.Out, format+"\n", a...)
	}
}

func (m *Manager) poll() time.Duration {
	if m.Poll > 0 {
		return m.Poll
	}
	return 5 * time.Second
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// CreateOptions are the flags of `pyre node create --cloud gcp`.
type CreateOptions struct {
	Name         string
	OS           string // linux (default) | windows
	MachineType  string
	GPU          string // "nvidia-l4:1"
	DiskGB       int
	Spot         *bool
	Image        string // family, or project/family
	HubURL       string
	IdleMinutes  *int
	RDP          bool // Windows: Remote Desktop over the tailnet
	Wait         time.Duration
	NoWait       bool
	TicketTTL    time.Duration
	TypeExplicit bool
	// Toolchains are added to gcp.toolchains (or the default preset).
	Toolchains []string
}

// CloudNode is one cloud machine as `pyre node ls --cloud` shows it.
type CloudNode struct {
	Name        string    `json:"name"`
	Cloud       string    `json:"cloud"`
	Project     string    `json:"project"`
	OS          string    `json:"os"`
	Instance    *Instance `json:"instance,omitempty"`
	NodeID      string    `json:"node_id,omitempty"`
	Connection  string    `json:"connection,omitempty"` // hub view
	ActiveRuns  int       `json:"active_runs"`
	HourlyUSD   float64   `json:"est_hourly_usd"`  // in the current state
	RunningUSD  float64   `json:"est_running_usd"` // while running
	UptimeHours float64   `json:"uptime_hours"`    // since the last start (running only)
	CostKnown   bool      `json:"est_known"`       // false: machine type not in the price table
	TailnetName string    `json:"tailnet_name"`    // Tailscale hostname
	Note        string    `json:"note,omitempty"`
}

// CreateResult is printed by create.
type CreateResult struct {
	CloudNode
	Connected bool `json:"connected"`
}

// resolveShape applies the GPU rules: L4 lives on G2 (bundled), A100 on
// A2 (bundled), other GPUs attach to N1.
func resolveShape(o CreateOptions, cfgType string) (machineType, accel string, err error) {
	mt := o.MachineType
	if mt == "" {
		mt = cfgType
	}
	g, n := splitGPU(o.GPU)
	if g == "" {
		return mt, "", nil
	}
	switch {
	case g == "nvidia-l4":
		if !strings.HasPrefix(mt, "g2-") {
			if o.TypeExplicit {
				return "", "", contracts.Errorf(contracts.ErrInvalid, "nvidia-l4 GPUs come with g2-* machine types (e.g. --type g2-standard-8), not %s", mt)
			}
			mt = map[int]string{1: "g2-standard-8", 2: "g2-standard-24", 4: "g2-standard-48", 8: "g2-standard-96"}[n]
			if mt == "" {
				return "", "", contracts.Errorf(contracts.ErrInvalid, "nvidia-l4 count must be 1, 2, 4 or 8")
			}
		}
		return mt, "", nil
	case strings.HasPrefix(g, "nvidia-tesla-a100") || g == "nvidia-a100-80gb":
		if !strings.HasPrefix(mt, "a2-") {
			if o.TypeExplicit {
				return "", "", contracts.Errorf(contracts.ErrInvalid, "A100 GPUs come with a2-* machine types, not %s", mt)
			}
			mt = map[int]string{1: "a2-highgpu-1g", 2: "a2-highgpu-2g", 4: "a2-highgpu-4g"}[n]
			if mt == "" {
				return "", "", contracts.Errorf(contracts.ErrInvalid, "A100 count must be 1, 2 or 4")
			}
		}
		return mt, "", nil
	default:
		if !strings.HasPrefix(mt, "n1-") {
			if o.TypeExplicit {
				return "", "", contracts.Errorf(contracts.ErrInvalid, "%s attaches to n1-* machine types, not %s", g, mt)
			}
			mt = "n1-standard-8"
		}
		return mt, fmt.Sprintf("type=%s,count=%d", g, n), nil
	}
}

// image resolves the boot image: --image (family or project/family), else
// the baked family when it exists, else the base family.
func (m *Manager) image(ctx context.Context, flag, osName, machineType string) (family, project string, err error) {
	if flag != "" {
		if p, f, ok := strings.Cut(flag, "/"); ok {
			return f, p, nil
		}
		if strings.HasPrefix(flag, "ubuntu-") {
			return flag, DefaultImageProject, nil
		}
		if strings.HasPrefix(flag, "windows-") {
			return flag, DefaultWindowsImageProject, nil
		}
		return flag, m.GCP.Project, nil
	}
	c := m.Cfg.GCP
	if baked := c.bakedFamily(); baked != "none" {
		if osName == OSWindows {
			baked += "-windows"
		}
		ok, err := m.GCP.ImageFamilyExists(ctx, m.GCP.Project, baked)
		if err != nil {
			return "", "", err
		}
		if ok {
			return baked, m.GCP.Project, nil
		}
	}
	if osName == OSWindows {
		return DefaultWindowsImage, DefaultWindowsImageProject, nil
	}
	if c.Image != "" {
		return c.Image, or(c.ImageProject, DefaultImageProject), nil
	}
	if isARM(machineType) {
		return DefaultBaseImageARM, DefaultImageProject, nil
	}
	return DefaultBaseImage, DefaultImageProject, nil
}

// Windows images.
const (
	// Windows Server 2025 Datacenter, as GitHub's windows-2025 runners.
	DefaultWindowsImage        = "windows-2025"
	DefaultWindowsImageProject = "windows-cloud"
)

func isARM(mt string) bool {
	return strings.HasPrefix(mt, "t2a-") || strings.HasPrefix(mt, "c4a-")
}

func (m *Manager) releaseConfig() (ReleaseConfig, error) {
	r := m.Cfg.Release
	if r.URL == "" || r.Version == "" {
		return r, contracts.Errorf(contracts.ErrInvalid, "the release location is not configured: set release.url (gs://bucket/prefix/ or https://…, holding the tarballs, SHA256SUMS and SHA256SUMS.sig) and release.version in the cloud config, or pass --release-url/--release-version")
	}
	if !strings.HasPrefix(r.URL, "gs://") && !strings.HasPrefix(r.URL, "https://") {
		return r, contracts.Errorf(contracts.ErrInvalid, "release.url must be gs:// or https://")
	}
	if strings.TrimSpace(m.AllowedSigners) == "" && !r.AllowUnsigned {
		return r, contracts.Errorf(contracts.ErrInvalid, "no release key to verify the release on the VM: pin one (~/.config/pyre/release-allowed-signers, written by install.sh) or set release.allowed_signers")
	}
	return r, nil
}

func (m *Manager) bootConfig(mode, osName, name, nodeID, hubURL string, rdp bool, toolchains []string) (BootConfig, error) {
	r, err := m.releaseConfig()
	if err != nil {
		return BootConfig{}, err
	}
	c := m.Cfg.GCP
	tcs, err := ResolveToolchains(osName, c.Toolchains, toolchains)
	if err != nil {
		return BootConfig{}, err
	}
	bc := BootConfig{Mode: mode, OS: osName, Name: name, User: c.user(), HubURL: hubURL, HubPin: m.HubPin, NodeID: nodeID,
		ReleaseURL: r.URL, ReleaseVersion: r.Version, AllowedSigners: m.AllowedSigners, AllowUnsigned: r.AllowUnsigned,
		WithTools: r.WithTools, Harnesses: c.harnesses(), Toolchains: withHarnessToolchains(tcs, c.harnesses()),
		PyredArgs: c.PyredArgs, RDP: rdp}
	if osName == OSWindows {
		if bc.Installers, err = validateInstallers(c.WindowsInstallers); err != nil {
			return BootConfig{}, err
		}
		if err := validateGPUDriver(c.WindowsGPUDriver); err != nil {
			return BootConfig{}, err
		}
		bc.GPUDriver = c.WindowsGPUDriver
	}
	if name != "" {
		bc.TSHostname = InstanceName(name)
		if osName != OSWindows {
			bc.TSSSH = m.Cfg.Tailscale.ssh()
			if bc.TSSSH {
				bc.SSHAlias = c.user() + "@" + bc.TSHostname
			}
		}
	}
	return bc, nil
}

func (m *Manager) privateDir() (string, func(), error) {
	dir, err := os.MkdirTemp(m.TempDir, "pyre-cloud-")
	if err != nil {
		return "", nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

func (m *Manager) usesGCS() bool { return strings.HasPrefix(m.Cfg.Release.URL, "gs://") }

// storageScope: the VM reads the release or the owner's Windows installers
// from a private bucket (read-only scope); otherwise it gets no identity.
func (m *Manager) storageScope(osName string) bool {
	return m.usesGCS() || (osName == OSWindows && len(m.Cfg.GCP.WindowsInstallers) > 0)
}

// Create provisions a VM and waits until its pyred is connected.
func (m *Manager) Create(ctx context.Context, o CreateOptions) (CreateResult, error) {
	var res CreateResult
	if o.OS == "" {
		o.OS = OSLinux
	}
	if o.OS != OSLinux && o.OS != OSWindows {
		return res, contracts.Errorf(contracts.ErrInvalid, "--os must be linux or windows")
	}
	if o.RDP && o.OS != OSWindows {
		return res, contracts.Errorf(contracts.ErrInvalid, "--rdp applies to --os windows (Linux nodes are reached with Tailscale SSH)")
	}
	if !nameRe.MatchString(o.Name) {
		return res, contracts.Errorf(contracts.ErrInvalid, "node name %q: use lowercase letters, digits and dashes, starting with a letter (max 42)", o.Name)
	}
	hubURL := or(o.HubURL, m.Cfg.HubURL)
	if hubURL == "" {
		return res, contracts.Errorf(contracts.ErrInvalid, "the hub address for cloud nodes is not set: set hub_url in the cloud config (or --hub-url) to the hub's tailnet address, e.g. https://<hub>.<tailnet>.ts.net:7443")
	}
	if !strings.HasPrefix(hubURL, "https://") {
		return res, contracts.Errorf(contracts.ErrInvalid, "--hub-url must be https://")
	}
	if m.Keys == nil {
		return res, contracts.Errorf(contracts.ErrInvalid, "no Tailscale auth key source: set %s/%s (OAuth client, mints a single-use ephemeral key per boot) or %s / tailscale.auth_key_file", EnvTailscaleClientID, EnvTailscaleClientSecret, EnvTailscaleAuthKey)
	}
	c := m.Cfg.GCP
	cfgType := c.machineType()
	vwsGPU, vwsAccel := "", ""
	if o.OS == OSWindows && o.GPU != "" {
		// NVIDIA RTX Virtual Workstation: the -vws accelerator carries the
		// GRID license the boot script's Windows graphics driver needs. The
		// machine shape follows the plain GPU (L4 -> G2, T4 -> N1).
		base, gpu, accel, err := windowsGPU(o.GPU)
		if err != nil {
			return res, err
		}
		o.GPU, vwsGPU, vwsAccel = base, gpu, accel
	}
	mt, accel, err := resolveShape(o, cfgType)
	if err != nil {
		return res, err
	}
	if vwsGPU != "" {
		o.GPU, accel = vwsGPU, vwsAccel
	}
	if o.OS == OSWindows && isARM(mt) {
		return res, contracts.Errorf(contracts.ErrInvalid, "Windows nodes need an x86 machine type")
	}
	disk := o.DiskGB
	if disk <= 0 {
		disk = c.diskGB()
	}
	spot := c.Spot
	if o.Spot != nil {
		spot = *o.Spot
	}
	idle := c.idleMinutes()
	if o.IdleMinutes != nil {
		idle = *o.IdleMinutes
	}
	inst := InstanceName(o.Name)

	// Refuse a name in use, on the hub or in the project.
	if n, err := m.Hub.FindNode(ctx, o.Name); err != nil {
		return res, err
	} else if n != nil {
		return res, contracts.Errorf(contracts.ErrConflict, "a node named %s already exists (%s)", o.Name, n.NodeID)
	}
	if in, err := m.GCP.Describe(ctx, m.Zone, inst); err != nil {
		return res, err
	} else if in != nil {
		return res, contracts.Errorf(contracts.ErrConflict, "instance %s already exists in %s (%s)", inst, m.Zone, in.Status)
	}
	if _, err := m.bootConfig("node", o.OS, o.Name, "", hubURL, o.RDP, o.Toolchains); err != nil {
		return res, err // release/key problems before anything is created
	}
	family, imgProject, err := m.image(ctx, o.Image, o.OS, mt)
	if err != nil {
		return res, err
	}
	if c.firewall() {
		created, err := m.GCP.EnsureFirewall(ctx, c.network())
		if err != nil {
			return res, err
		}
		if created {
			m.logf("created firewall rule %s (deny all inbound to tag %s; nodes are reached over Tailscale only)", FirewallRule, NetworkTag)
		}
	}

	ttl := o.TicketTTL
	if ttl <= 0 {
		ttl = DefaultTicketTTLMin * time.Minute
		if o.OS == OSWindows {
			// Enrollment happens at the dev user's logon, after the toolchain
			// (Build Tools, Rust, LLVM ...) and a reboot.
			ttl *= 2
		}
	}
	ticket, err := m.Hub.CreateEnrollment(ctx, o.Name, ttl)
	if err != nil {
		return res, err
	}
	key, err := m.Keys.Mint(ctx, "pyre node "+o.Name)
	if err != nil {
		return res, err
	}
	joined := false
	defer func() {
		if !joined {
			m.revokeKey(key)
		}
	}()
	bc, err := m.bootConfig("node", o.OS, o.Name, ticket.NodeID, hubURL, o.RDP, o.Toolchains)
	if err != nil {
		return res, err
	}
	if o.OS == OSWindows {
		bc.GPU = o.GPU
	}
	dir, cleanup, err := m.privateDir()
	if err != nil {
		return res, err
	}
	defer cleanup()
	files, err := bootFiles(dir, bc, map[string]string{MetaEnrollSecret: ticket.Secret, MetaTSAuthKey: key.Key})
	if err != nil {
		return res, err
	}
	labels := map[string]string{LabelNode: o.Name, LabelManaged: "true", LabelOS: o.OS}
	spec := CreateSpec{Name: inst, Zone: m.Zone, MachineType: mt, ImageFamily: family, ImageProject: imgProject,
		DiskGB: disk, DiskType: c.diskType(), Spot: spot, Accelerator: accel, GPU: o.GPU != "",
		Labels: labels, Metadata: map[string]string{MetaNodeID: ticket.NodeID, MetaIdleMinutes: strconv.Itoa(idle)},
		MetadataFiles: files, ServiceAccount: c.ServiceAccount, StorageReadScope: m.storageScope(o.OS),
		NetworkTags: []string{NetworkTag}, Network: c.network()}
	m.logf("creating %s (%s, %s, %d GB%s) in %s/%s from %s/%s", inst, o.OS, mt, disk, map[bool]string{true: ", spot"}[spot], m.GCP.Project, m.Zone, imgProject, family)
	if err := m.GCP.Create(ctx, spec); err != nil {
		return res, fmt.Errorf("%w (the enrollment ticket for %s expires unused)", err, ticket.NodeID)
	}
	if err := m.GCP.AddDiskLabels(ctx, m.Zone, inst, labels); err != nil {
		m.logf("warning: could not label boot disk %s: %v", inst, err)
	}
	est := EstimateCost(mt, o.GPU, spot, disk, c.diskType(), m.Cfg.Prices)
	if o.OS == OSWindows {
		est = withWindowsLicense(est, mt, spot)
	}
	res.CloudNode = CloudNode{Name: o.Name, Cloud: "gcp", Project: m.GCP.Project, OS: o.OS, NodeID: ticket.NodeID,
		HourlyUSD: est.Hourly(true), RunningUSD: est.Hourly(true), CostKnown: est.Known, TailnetName: InstanceName(o.Name),
		Instance: &Instance{Name: inst, Node: o.Name, Zone: m.Zone, MachineType: mt, Status: "PROVISIONING", Spot: spot, GPU: o.GPU, DiskGB: disk, NodeID: ticket.NodeID}}
	m.cachePut(res.CloudNode)
	if o.NoWait {
		m.logf("not waiting: the boot script still holds the one-time secrets in metadata; remove them later with `pyre node start %s` or `gcloud compute instances remove-metadata %s --zone %s --keys %s`", o.Name, inst, m.Zone, strings.Join(SecretMetadataKeys, ","))
		return res, nil
	}
	wait := o.Wait
	if wait <= 0 {
		wait = time.Duration(or0(c.WaitMinutes, DefaultWaitMinutes)) * time.Minute
		if o.OS == OSWindows {
			wait *= 4 // the toolchain (Build Tools ...) and a reboot
		}
	}
	connected, werr := m.waitConnected(ctx, o.Name, inst, ticket.NodeID, wait)
	joined = connected
	m.removeSecrets(inst)
	if werr != nil {
		return res, werr
	}
	res.Connected = connected
	res.Connection = string(contracts.ConnConnected)
	res.Instance.Status = "RUNNING"
	m.cachePut(res.CloudNode)
	return res, nil
}

func or0(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// revokeKey invalidates a minted key the node never used. Never call it
// after the node joined: revoking the key of an ephemeral, pre-authorized
// device logs that device out of the tailnet (first real GCP node,
// 2026-10-09), and a used single-use key is spent and expires anyway.
func (m *Manager) revokeKey(k AuthKey) {
	if k.ID == "" || m.Keys == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = m.Keys.Revoke(ctx, k.ID) // used keys are spent anyway
}

// removeSecrets drops the secret metadata keys (best effort, never fatal:
// both are single-use and expire).
func (m *Manager) removeSecrets(inst string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := m.GCP.RemoveMetadata(ctx, m.Zone, inst, SecretMetadataKeys); err != nil {
		m.logf("warning: could not remove the one-time secrets from %s metadata (both are spent or expire): %v", inst, err)
	}
}

// waitConnected polls the hub until the node is connected.
func (m *Manager) waitConnected(ctx context.Context, name, inst, nodeID string, wait time.Duration) (bool, error) {
	deadline := m.now().Add(wait)
	lastNote := m.now()
	m.logf("waiting up to %s for %s to boot, join the tailnet, enroll and connect (serial console: gcloud compute instances get-serial-port-output %s --zone %s --project %s)", wait.Round(time.Second), name, inst, m.Zone, m.GCP.Project)
	for i := 0; ; i++ {
		n, err := m.Hub.FindNode(ctx, name)
		if err == nil && n != nil && (nodeID == "" || n.NodeID == nodeID) && n.Connection.State == contracts.ConnConnected {
			m.logf("%s connected (%s)", name, n.NodeID)
			return true, nil
		}
		if i%6 == 5 {
			if in, err := m.GCP.Describe(ctx, m.Zone, inst); err == nil && in != nil && (in.Status == "TERMINATED" || in.Status == "STOPPED") {
				return false, contracts.Errorf(contracts.ErrUnavailable, "%s stopped while booting (spot preemption or a boot failure): see the serial console", inst)
			}
		}
		if m.now().After(deadline) {
			return false, contracts.Errorf(contracts.ErrOutcomeIndeterminate, "%s did not connect within %s; the VM is still there (check the serial console, then `pyre node delete %s --force` or wait and `pyre nodes`)", name, wait, name)
		}
		if m.now().Sub(lastNote) >= time.Minute {
			m.logf("still waiting for %s …", name)
			lastNote = m.now()
		}
		if err := sleep(ctx, m.poll()); err != nil {
			return false, err
		}
	}
}

func (m *Manager) describe(ctx context.Context, name string) (*Instance, error) {
	in, err := m.GCP.Describe(ctx, m.Zone, InstanceName(name))
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, contracts.Errorf(contracts.ErrNotFound, "no instance %s in %s/%s", InstanceName(name), m.GCP.Project, m.Zone)
	}
	if in.Labels[LabelManaged] != "true" {
		return nil, contracts.Errorf(contracts.ErrConflict, "instance %s is not managed by Pyre (no %s=true label); leaving it alone", in.Name, LabelManaged)
	}
	return in, nil
}

// liveRuns lists the sessions on node whose run may still be executing.
func liveRuns(ss []contracts.SessionView, nodeID string) []contracts.SessionView {
	var out []contracts.SessionView
	for _, s := range ss {
		if s.Run != nil && s.Run.NodeID == nodeID && !s.Run.State.Terminal() {
			out = append(out, s)
		}
	}
	return out
}

// Stop powers the VM off (disk kept; sessions resume after start).
func (m *Manager) Stop(ctx context.Context, name string, force bool) (CloudNode, error) {
	in, err := m.describe(ctx, name)
	if err != nil {
		return CloudNode{}, err
	}
	if !force {
		if n, _ := m.Hub.FindNode(ctx, name); n != nil {
			ss, err := m.Hub.NodeSessions(ctx, n.NodeID)
			if err != nil {
				return CloudNode{}, err
			}
			if live := liveRuns(ss, n.NodeID); len(live) > 0 {
				return CloudNode{}, contracts.Errorf(contracts.ErrConflict, "%s has %d live run(s) (%s); stopping kills their processes (transcripts stay resumable): stop them first or pass --force", name, len(live), sessionList(live))
			}
		}
	}
	if in.Status != "TERMINATED" && in.Status != "STOPPED" {
		m.logf("stopping %s (disk kept)", in.Name)
		if err := m.GCP.Stop(ctx, m.Zone, in.Name); err != nil {
			return CloudNode{}, err
		}
	}
	in.Status = "TERMINATED"
	in.StartedAt = nil
	cn := m.cloudNode(*in, nil)
	m.cachePut(cn)
	return cn, nil
}

func sessionList(ss []contracts.SessionView) string {
	var ids []string
	for i, s := range ss {
		if i == 5 {
			ids = append(ids, fmt.Sprintf("+%d more", len(ss)-5))
			break
		}
		ids = append(ids, s.Session.SessionID)
	}
	return strings.Join(ids, ", ")
}

// Start boots a stopped VM with a fresh Tailscale key (the ephemeral
// device left the tailnet at power-off) and waits for the node.
func (m *Manager) Start(ctx context.Context, name string, wait time.Duration, noWait bool) (CloudNode, error) {
	in, err := m.describe(ctx, name)
	if err != nil {
		return CloudNode{}, err
	}
	if in.Status == "RUNNING" {
		if in.SecretsPending {
			m.removeSecrets(in.Name)
		}
		return m.cloudNode(*in, nil), nil
	}
	if m.Keys == nil {
		return CloudNode{}, contracts.Errorf(contracts.ErrInvalid, "starting needs a fresh Tailscale key: configure the Tailscale OAuth client (%s/%s) or %s", EnvTailscaleClientID, EnvTailscaleClientSecret, EnvTailscaleAuthKey)
	}
	key, err := m.Keys.Mint(ctx, "pyre node "+name)
	if err != nil {
		return CloudNode{}, err
	}
	joined := false
	defer func() {
		if !joined {
			m.revokeKey(key)
		}
	}()
	dir, cleanup, err := m.privateDir()
	if err != nil {
		return CloudNode{}, err
	}
	defer cleanup()
	kf, err := writePrivate(dir, MetaTSAuthKey, []byte(key.Key))
	if err != nil {
		return CloudNode{}, err
	}
	if err := m.GCP.AddMetadataFiles(ctx, m.Zone, in.Name, map[string]string{MetaTSAuthKey: kf}); err != nil {
		return CloudNode{}, err
	}
	m.logf("starting %s", in.Name)
	if err := m.GCP.Start(ctx, m.Zone, in.Name); err != nil {
		m.removeSecrets(in.Name)
		return CloudNode{}, err
	}
	in.Status = "RUNNING"
	t := m.now()
	in.StartedAt = &t
	if noWait {
		m.logf("not waiting: remove the key from metadata later with `pyre node start %s` once it runs", name)
		return m.cloudNode(*in, nil), nil
	}
	if wait <= 0 {
		wait = time.Duration(or0(m.Cfg.GCP.WaitMinutes, DefaultWaitMinutes)) * time.Minute
	}
	ok, werr := m.waitConnected(ctx, name, in.Name, in.NodeID, wait)
	joined = ok
	m.removeSecrets(in.Name)
	n, _ := m.Hub.FindNode(ctx, name)
	cn := m.cloudNode(*in, n)
	m.cachePut(cn)
	return cn, werr
}

// DeleteResult reports what delete removed.
type DeleteResult struct {
	Name        string   `json:"name"`
	Instance    string   `json:"instance,omitempty"`
	Disks       []string `json:"disks_deleted,omitempty"`
	RevokedNode string   `json:"revoked_node,omitempty"`
	Forced      []string `json:"forced_past,omitempty"` // safety findings overridden by --force
}

// Unsafe lists what deleting node would lose: live runs, and worktrees
// with uncommitted changes or branches neither merged nor pushed (the
// rules of the worktree removal code).
func (m *Manager) Unsafe(ctx context.Context, n *contracts.Node) ([]string, error) {
	if n == nil {
		return nil, nil
	}
	ss, err := m.Hub.NodeSessions(ctx, n.NodeID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range liveRuns(ss, n.NodeID) {
		out = append(out, fmt.Sprintf("live run: session %s %q (%s)", s.Session.SessionID, s.Session.Title, s.Run.State))
	}
	online := n.Connection.State == contracts.ConnConnected
	seen := map[string]bool{}
	for _, s := range ss {
		if s.Run == nil || s.Run.NodeID != n.NodeID {
			continue
		}
		sw, err := m.Hub.SessionWorktrees(ctx, s, online)
		if err != nil {
			if contracts.IsCode(err, contracts.ErrNotFound) {
				continue // never reported: not in a repository
			}
			out = append(out, fmt.Sprintf("worktrees of session %s unknown: %v", s.Session.SessionID, err))
			continue
		}
		for _, r := range sw.Repos {
			for _, w := range r.Worktrees {
				if seen[w.Path] {
					continue
				}
				seen[w.Path] = true
				switch {
				case w.Main:
					// Not checked by the scan (only session-created worktrees are).
				case !w.CreatedInSession:
				case w.Dirty:
					out = append(out, fmt.Sprintf("uncommitted changes: %s", w.Path))
				case w.Branch != "" && !w.Merged && !w.Pushed:
					out = append(out, fmt.Sprintf("branch %s neither merged nor pushed: %s", w.Branch, w.Path))
				}
			}
		}
	}
	if len(out) > 0 && !online {
		out = append(out, "(the node is offline: worktrees were checked from their last report; `pyre node start` for a fresh check)")
	}
	sort.Strings(out[:countReal(out)])
	return out, nil
}

func countReal(out []string) int {
	if n := len(out); n > 0 && strings.HasPrefix(out[n-1], "(") {
		return n - 1
	}
	return len(out)
}

// Delete removes the VM, its disks and its hub enrollment. It refuses while
// runs are live or work is unpushed unless force.
func (m *Manager) Delete(ctx context.Context, name string, force bool) (DeleteResult, error) {
	res := DeleteResult{Name: name}
	in, err := m.GCP.Describe(ctx, m.Zone, InstanceName(name))
	if err != nil {
		return res, err
	}
	if in != nil && in.Labels[LabelManaged] != "true" {
		return res, contracts.Errorf(contracts.ErrConflict, "instance %s is not managed by Pyre; leaving it alone", in.Name)
	}
	n, err := m.Hub.FindNode(ctx, name)
	if err != nil {
		return res, err
	}
	disks, err := m.GCP.DisksLabeled(ctx, LabelNode, name)
	if err != nil {
		return res, err
	}
	if in == nil && n == nil && len(disks) == 0 {
		return res, contracts.Errorf(contracts.ErrNotFound, "no cloud node %s (no instance %s, no labelled disks, no hub node)", name, InstanceName(name))
	}
	unsafe, err := m.Unsafe(ctx, n)
	if err != nil {
		return res, err
	}
	if len(unsafe) > 0 {
		if !force {
			return res, contracts.Errorf(contracts.ErrConflict, "refusing to delete %s:\n  %s\npush or remove the work (or stop the runs), or pass --force to lose it", name, strings.Join(unsafe, "\n  "))
		}
		res.Forced = unsafe
	}
	if in != nil {
		m.logf("deleting %s and its disks", in.Name)
		if err := m.GCP.Delete(ctx, m.Zone, in.Name); err != nil {
			return res, err
		}
		res.Instance = in.Name
		// The instance delete removed its attached disks; re-list for leftovers.
		if disks, err = m.GCP.DisksLabeled(ctx, LabelNode, name); err != nil {
			return res, err
		}
	}
	for _, d := range disks {
		if len(d.Users) > 0 {
			m.logf("warning: disk %s is still attached to %v; not deleted", d.Name, d.Users)
			continue
		}
		if err := m.GCP.DeleteDisk(ctx, d.Zone, d.Name); err != nil && !NotFound(err) {
			return res, err
		}
		res.Disks = append(res.Disks, d.Name)
	}
	if n != nil {
		if err := m.Hub.RevokeNode(ctx, n.NodeID, "pyre node delete: cloud VM deleted"); err != nil {
			return res, fmt.Errorf("VM deleted but revoking hub node %s failed: %w (run `pyre revoke node %s`)", n.NodeID, err, n.NodeID)
		}
		res.RevokedNode = n.NodeID
	}
	if m.Cache != nil {
		m.Cache.Delete(name)
	}
	return res, nil
}

// List returns every Pyre-managed VM in the project joined with the hub's
// node view.
func (m *Manager) List(ctx context.Context, nodes []contracts.Node) ([]CloudNode, error) {
	ins, err := m.GCP.List(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]*contracts.Node{}
	for i := range nodes {
		byName[nodes[i].Name] = &nodes[i]
	}
	out := make([]CloudNode, 0, len(ins))
	for _, in := range ins {
		if in.Node == "" {
			continue // builder VMs of image bake
		}
		out = append(out, m.cloudNode(in, byName[in.Node]))
	}
	if m.Cache != nil {
		m.Cache.Replace(out)
	}
	return out, nil
}

func (m *Manager) cloudNode(in Instance, n *contracts.Node) CloudNode {
	running := in.Status == "RUNNING"
	osName := OSLinux
	if in.Labels[LabelOS] == OSWindows {
		osName = OSWindows
	}
	est := EstimateCost(in.MachineType, in.GPU, in.Spot, in.DiskGB, m.Cfg.GCP.diskType(), m.Cfg.Prices)
	if osName == OSWindows {
		est = withWindowsLicense(est, in.MachineType, in.Spot)
	}
	cn := CloudNode{Name: in.Node, Cloud: "gcp", Project: m.GCP.Project, OS: osName, Instance: &in, NodeID: in.NodeID,
		HourlyUSD: est.Hourly(running), RunningUSD: est.Hourly(true), CostKnown: est.Known, TailnetName: InstanceName(in.Node)}
	if running && in.StartedAt != nil {
		cn.UptimeHours = m.now().Sub(*in.StartedAt).Hours()
	}
	if n != nil {
		cn.NodeID = n.NodeID
		cn.Connection = string(n.Connection.State)
		cn.ActiveRuns = n.Resources.ActiveRuns
	} else {
		cn.Connection = "not enrolled"
	}
	if in.SecretsPending {
		cn.Note = "one-time secrets still in metadata (pyre node start removes them)"
	}
	return cn
}

// windows Server license, per vCPU-hour (on-demand and spot alike).
const windowsLicensePerVCPU = 0.046

func withWindowsLicense(e Estimate, mt string, spot bool) Estimate {
	vcpu, _, _, ok := shape(mt)
	if !ok {
		return e
	}
	e.ComputeHourly += vcpu * windowsLicensePerVCPU
	return e
}

// SetIdle changes the idle auto-stop minutes (read by the VM every minute).
func (m *Manager) SetIdle(ctx context.Context, name string, minutes int) error {
	in, err := m.describe(ctx, name)
	if err != nil {
		return err
	}
	return m.GCP.AddMetadata(ctx, m.Zone, in.Name, map[string]string{MetaIdleMinutes: strconv.Itoa(minutes)})
}

func (m *Manager) cachePut(cn CloudNode) {
	if m.Cache != nil {
		m.Cache.Put(cn)
	}
}

// BakeOptions are the flags of `pyre node image bake`.
type BakeOptions struct {
	OS          string
	Family      string
	MachineType string
	Image       string // base image family
	Timeout     time.Duration
	// Toolchains are added to gcp.toolchains (or the default preset).
	Toolchains []string
}

// BakeResult names the new image.
type BakeResult struct {
	Image   string `json:"image"`
	Family  string `json:"family"`
	Project string `json:"project"`
	Builder string `json:"builder"`
}

// Bake builds an image family from a throwaway builder VM that runs the
// boot script in bake mode (harnesses + verified release, no tailnet
// identity, no enrollment) and powers itself off; its disk becomes the
// image and the builder is deleted. Later creates boot from it in about a
// minute. A builder (not a used node) is the source so no node credential,
// tailnet key or session data can end up in the image.
func (m *Manager) Bake(ctx context.Context, o BakeOptions) (BakeResult, error) {
	if o.OS == "" {
		o.OS = OSLinux
	}
	family := o.Family
	if family == "" {
		family = m.Cfg.GCP.bakedFamily()
		if family == "none" {
			family = DefaultBakedFamily
		}
		if o.OS == OSWindows {
			family += "-windows"
		}
	}
	mt := or(o.MachineType, "e2-standard-4")
	bc, err := m.bootConfig("bake", o.OS, "", "", "", false, o.Toolchains)
	if err != nil {
		return BakeResult{}, err
	}
	base, baseProject := DefaultBaseImage, DefaultImageProject
	if o.OS == OSWindows {
		base, baseProject = DefaultWindowsImage, DefaultWindowsImageProject
	} else if m.Cfg.GCP.Image != "" {
		base, baseProject = m.Cfg.GCP.Image, or(m.Cfg.GCP.ImageProject, DefaultImageProject)
	}
	if o.Image != "" {
		if p, f, ok := strings.Cut(o.Image, "/"); ok {
			base, baseProject = f, p
		} else {
			base = o.Image
		}
	}
	stamp := m.now().UTC().Format("20060102-150405")
	builder := "pyre-bake-" + stamp
	dir, cleanup, err := m.privateDir()
	if err != nil {
		return BakeResult{}, err
	}
	defer cleanup()
	files, err := bootFiles(dir, bc, nil)
	if err != nil {
		return BakeResult{}, err
	}
	labels := map[string]string{LabelManaged: "true", LabelRole: "bake"}
	spec := CreateSpec{Name: builder, Zone: m.Zone, MachineType: mt, ImageFamily: base, ImageProject: baseProject,
		DiskGB: m.Cfg.GCP.diskGB(), DiskType: m.Cfg.GCP.diskType(), Labels: labels, MetadataFiles: files,
		ServiceAccount: m.Cfg.GCP.ServiceAccount, StorageReadScope: m.storageScope(o.OS), NetworkTags: []string{NetworkTag}, Network: m.Cfg.GCP.network()}
	if m.Cfg.GCP.firewall() {
		if _, err := m.GCP.EnsureFirewall(ctx, m.Cfg.GCP.network()); err != nil {
			return BakeResult{}, err
		}
	}
	m.logf("creating builder %s (%s from %s/%s)", builder, mt, baseProject, base)
	if err := m.GCP.Create(ctx, spec); err != nil {
		return BakeResult{}, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 45 * time.Minute
		if o.OS == OSWindows {
			timeout = 120 * time.Minute
		}
	}
	deadline := m.now().Add(timeout)
	m.logf("waiting up to %s for the builder to install everything and power off (serial console: gcloud compute instances get-serial-port-output %s --zone %s --project %s)", timeout, builder, m.Zone, m.GCP.Project)
	for {
		in, err := m.GCP.Describe(ctx, m.Zone, builder)
		if err != nil {
			return BakeResult{}, err
		}
		if in != nil && in.Status == "TERMINATED" {
			break
		}
		if m.now().After(deadline) {
			return BakeResult{}, contracts.Errorf(contracts.ErrUnavailable, "builder %s did not finish within %s; it is left for inspection (delete it with gcloud compute instances delete %s --zone %s)", builder, timeout, builder, m.Zone)
		}
		if err := sleep(ctx, m.poll()); err != nil {
			return BakeResult{}, err
		}
	}
	image := "pyre-node-" + stamp
	if o.OS == OSWindows {
		image = "pyre-node-windows-" + stamp
	}
	m.logf("creating image %s in family %s", image, family)
	if err := m.GCP.CreateImage(ctx, image, family, builder, m.Zone, map[string]string{LabelManaged: "true", LabelRole: "image", LabelOS: o.OS}); err != nil {
		return BakeResult{}, err
	}
	if err := m.GCP.Delete(ctx, m.Zone, builder); err != nil {
		m.logf("warning: could not delete builder %s: %v", builder, err)
	}
	return BakeResult{Image: image, Family: family, Project: m.GCP.Project, Builder: builder}, nil
}
