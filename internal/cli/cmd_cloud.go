package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/cloud"
	c "github.com/jhh3/pyre/internal/contracts"
)

// Cloud nodes (W-gcp): `pyre node create --cloud gcp`, stop/start/delete,
// idle, rdp, image bake and `pyre nodes --cloud`. Provisioning runs here,
// with the owner's own gcloud; see internal/cloud.

// cloudHub adapts Ops to cloud.Hub.
type cloudHub struct {
	ops  *Ops
	opID string
}

func (h cloudHub) CreateEnrollment(ctx context.Context, name string, ttl time.Duration) (c.EnrollmentTicket, error) {
	return h.ops.API.CreateEnrollment(ctx, c.EnrollmentRequest{Mutation: mut(h.opID), Name: name, TTL: int(ttl.Seconds())})
}

func (h cloudHub) FindNode(ctx context.Context, name string) (*c.Node, error) {
	page, err := h.ops.ListNodes(ctx, false)
	if err != nil {
		return nil, err
	}
	for i := range page.Items {
		if page.Items[i].Name == name && page.Items[i].Connection.State != c.ConnRevoked {
			return &page.Items[i], nil
		}
	}
	return nil, nil
}

func (h cloudHub) NodeSessions(ctx context.Context, nodeID string) ([]c.SessionView, error) {
	page, err := h.ops.ListSessions(ctx, SessionFilter{Node: nodeID, IncludeArchived: true})
	if err != nil {
		return nil, err
	}
	return page.Items, nil
}

func (h cloudHub) SessionWorktrees(ctx context.Context, sv c.SessionView, live bool) (c.SessionWorktrees, error) {
	return h.ops.SessionWorktrees(ctx, sv, live)
}

func (h cloudHub) RevokeNode(ctx context.Context, nodeID, reason string) error {
	_, err := h.ops.API.RevokeNode(ctx, c.RevokeRequest{Mutation: mut(""), ID: nodeID, Reason: reason})
	return err
}

type cloudFlags struct {
	project, zone string
}

func (f *cloudFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.project, "project", "", "GCP project (default: cloud config gcp.project, else `gcloud config get-value project`)")
	cmd.Flags().StringVar(&f.zone, "zone", "", "GCP zone (default: cloud config gcp.zone, else `gcloud config get-value compute/zone`)")
}

const toolchainFlagHelp = "developer toolchain to install, added to the cloud config's gcp.toolchains (repeatable): node@lts|<major>|<x.y.z>, pnpm, bun@<x.y.z>, rust, cmake, llvm, python, vs-buildtools, git, gh, parsec (Windows), or a preset (default, electron, media)"

func (a *App) clientConfigPath() string {
	if a.configFlag != "" {
		return a.configFlag
	}
	return DefaultConfigPath(a.Getenv)
}

func (a *App) cloudConfig() (cloud.Config, error) {
	return cloud.LoadConfig(cloud.ConfigPath(a.Getenv, a.clientConfigPath()))
}

func (a *App) cloudCache() *cloud.Cache {
	return &cloud.Cache{Path: filepath.Join(filepath.Dir(a.clientConfigPath()), cloud.CacheFileName)}
}

// releaseSigners reads the release public key the VM verifies with.
func (a *App) releaseSigners(cfg cloud.Config) string {
	p := cfg.Release.AllowedSigners
	if p == "" {
		home := a.Getenv("HOME")
		if home == "" {
			return ""
		}
		p = filepath.Join(home, ".config", "pyre", "release-allowed-signers")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// cloudManager builds the manager; withHub connects to the hub too.
func (a *App) cloudManager(ctx context.Context, f cloudFlags, withHub bool) (*cloud.Manager, error) {
	cfg, err := a.cloudConfig()
	if err != nil {
		return nil, err
	}
	run := a.CloudRunner
	if run == nil {
		run = cloud.ExecRunner{}
	}
	project := firstNonEmpty(f.project, cfg.GCP.Project)
	if project == "" {
		project = cloud.ConfigValue(ctx, run, "project")
	}
	zone := firstNonEmpty(f.zone, cfg.GCP.Zone)
	if zone == "" {
		zone = cloud.ConfigValue(ctx, run, "compute/zone")
	}
	if project == "" || zone == "" {
		return nil, usagef("no GCP project/zone: pass --project/--zone, set gcp.project/gcp.zone in %s, or `gcloud config set project …` / `gcloud config set compute/zone …`", cloud.ConfigPath(a.Getenv, a.clientConfigPath()))
	}
	keys := a.CloudKeys
	if keys == nil {
		if keys, err = cloud.MinterFromConfig(cfg.Tailscale, a.Getenv); err != nil {
			return nil, err
		}
	}
	m := &cloud.Manager{GCP: &cloud.GCP{Run: run, Project: project}, Keys: keys, Cfg: cfg, Zone: zone,
		Out: a.Stderr, Poll: a.CloudPoll, AllowedSigners: a.releaseSigners(cfg), Cache: a.cloudCache()}
	if withHub {
		ops, err := a.connect()
		if err != nil {
			return nil, err
		}
		m.Hub = cloudHub{ops: ops, opID: a.opID}
		m.HubPin = a.pinFlag
		if m.HubPin == "" && a.conn != nil {
			m.HubPin = a.conn.Pin
		}
	}
	return m, nil
}

func randSuffix() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func usd(v float64, known bool) string {
	if !known {
		return "?"
	}
	return fmt.Sprintf("$%.2f", v)
}

func (a *App) printCloudNodes(w io.Writer, ns []cloud.CloudNode) {
	var rows [][]string
	for _, n := range ns {
		in := n.Instance
		mt := in.MachineType
		if in.Spot {
			mt += " (spot)"
		}
		if in.GPU != "" {
			mt += " " + in.GPU
		}
		up := "-"
		if n.UptimeHours > 0 {
			up = short(time.Duration(n.UptimeHours * float64(time.Hour)))
		}
		rows = append(rows, []string{n.Name, n.OS, in.Zone, mt, strings.ToLower(in.Status), orDash(n.Connection), fmt.Sprint(n.ActiveRuns), up,
			usd(n.HourlyUSD, n.CostKnown), usd(n.RunningUSD, n.CostKnown), orDash(n.Note)})
	}
	table(w, "NAME\tOS\tZONE\tTYPE\tVM\tHUB\tRUNS\tUPTIME\tEST $/H NOW\tEST $/H RUNNING\tNOTE", rows)
	if len(ns) > 0 {
		fmt.Fprintln(w, "estimates are list prices (spot ~35% of on-demand); a stopped VM still pays for its disk")
	}
}

func (a *App) cloudList(cmd *cobra.Command, f cloudFlags) error {
	m, err := a.cloudManager(cmd.Context(), f, true)
	if err != nil {
		return err
	}
	ops, _ := a.connect()
	page, err := ops.ListNodes(cmd.Context(), false)
	if err != nil {
		return err
	}
	ns, err := m.List(cmd.Context(), page.Items)
	if err != nil {
		return err
	}
	return a.emit(ns, func(w io.Writer) {
		if len(ns) == 0 {
			fmt.Fprintf(w, "no Pyre cloud nodes in %s (pyre node create --cloud gcp)\n", m.GCP.Project)
			return
		}
		a.printCloudNodes(w, ns)
	})
}

// addCloudNodeCommands adds the cloud subcommands to `pyre nodes`.
func (a *App) addCloudNodeCommands(nodes *cobra.Command) {
	// create
	var cf cloudFlags
	var co cloud.CreateOptions
	var provider, release, releaseVer string
	var spot bool
	var idle int
	create := &cobra.Command{
		Use:   "create --cloud gcp [--name N] [--os linux|windows] [--type T] [--gpu nvidia-l4:1] [--disk GB] [--spot] [--image FAMILY] [--toolchain ID]... [--project P --zone Z]",
		Short: "Create an on-demand cloud machine and enroll it as a node (over Tailscale)",
		Long: `Creates a VM with your gcloud (no service-account keys are stored), labelled
pyre-node=<name> and pyre-managed=true. Its first-boot script joins your
tailnet with a single-use ephemeral key, verifies and installs the signed Pyre
release, enrolls with a one-time hub ticket and starts pyred; both secrets
travel in instance metadata (never argv) and are removed once the node
connects. No inbound port is opened. The node powers itself off after
--idle-stop minutes without live runs (default 30; disk kept).

Settings: ` + cloud.ConfigFileName + ` beside the client config (hub_url = the hub's
tailnet address, release.url/version, tailscale OAuth client, gcp defaults).
Tailscale key: TAILSCALE_OAUTH_CLIENT_ID/TAILSCALE_OAUTH_CLIENT_SECRET (a
fresh key per create/start) or PYRE_TAILSCALE_AUTHKEY (one create).

--os windows makes a Windows Server 2025 node (Git, Node LTS, Python, VS
Build Tools, Claude Code, Codex; auto-logon dev user so pyred can run GUI
apps); --rdp enables Remote Desktop reachable from the tailnet only (pyre
node rdp). With --gpu (nvidia-l4, nvidia-tesla-t4) a Windows node gets the
NVIDIA RTX Virtual Workstation GPU, the GRID driver and hardware graphics
for Remote Desktop; gcp.windows_installers runs your own installers from a
private bucket (SHA-256 checked).

Developer toolchains: gcp.toolchains in the cloud config plus --toolchain
(repeatable; a later entry for the same tool wins), e.g. --toolchain
node@24.21.0 --toolchain pnpm --toolchain bun@1.3.14 --toolchain rust.
Ids: node@lts|<major>|<x.y.z>, pnpm[@x.y.z], bun[@x.y.z], rust[@channel],
cmake[@x.y.z], llvm[@x.y.z], python[@x.y.z], vs-buildtools, git, gh,
parsec (Windows); presets: default, electron, media. Bake an image with the
same list (pyre node image bake --toolchain ...) so creates boot ready.

Codex on the new node needs its own login: pyre accounts login --node <name>.
Claude token accounts reach it automatically.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if provider != "gcp" {
				return usagef("--cloud %q: only gcp is supported", provider)
			}
			if co.Name == "" {
				co.Name = "gcp-" + randSuffix()
			}
			co.TypeExplicit = cmd.Flags().Changed("type")
			if cmd.Flags().Changed("spot") {
				co.Spot = &spot
			}
			if cmd.Flags().Changed("idle-stop") {
				co.IdleMinutes = &idle
			}
			m, err := a.cloudManager(cmd.Context(), cf, true)
			if err != nil {
				return err
			}
			if release != "" {
				m.Cfg.Release.URL = release
			}
			if releaseVer != "" {
				m.Cfg.Release.Version = releaseVer
			}
			res, err := m.Create(cmd.Context(), co)
			if err != nil {
				if c.IsCode(err, c.ErrOutcomeIndeterminate) {
					return withStatus(ExitIndeterminate, err)
				}
				return err
			}
			return a.emit(res, func(w io.Writer) {
				in := res.Instance
				state := "created; booting"
				if res.Connected {
					state = "connected"
				}
				fmt.Fprintf(w, "node %s (%s) %s: gcp %s %s in %s/%s, ~%s/h while running (estimate)\n", res.Name, res.NodeID, state, res.OS, in.MachineType, res.Project, in.Zone, usd(res.RunningUSD, res.CostKnown))
				fmt.Fprintf(w, "tailnet name %s; idle auto-stop per pyre-idle-minutes metadata (pyre node idle %s <minutes>)\n", res.TailnetName, res.Name)
				fmt.Fprintf(w, "Codex there needs its own login: pyre accounts login --node %s (Claude token accounts sync automatically)\n", res.Name)
				if res.OS == cloud.OSWindows && co.RDP {
					fmt.Fprintf(w, "Remote Desktop (tailnet only): pyre node rdp %s --reset-password --open\n", res.Name)
				}
			})
		}),
	}
	fl := create.Flags()
	fl.StringVar(&provider, "cloud", "gcp", "cloud provider (gcp)")
	fl.StringVar(&co.Name, "name", "", "node name (default gcp-<random>); the VM is pyre-<name>")
	fl.StringVar(&co.OS, "os", "linux", "linux or windows")
	fl.StringVar(&co.MachineType, "type", "", "machine type (default: config gcp.machine_type, else "+cloud.DefaultMachineType+")")
	fl.StringVar(&co.GPU, "gpu", "", "GPU as type:count, e.g. nvidia-l4:1 (picks a matching g2/a2/n1 type)")
	fl.IntVar(&co.DiskGB, "disk", 0, "boot disk size in GB (default 200)")
	fl.BoolVar(&spot, "spot", false, "spot VM (much cheaper; may be preempted, which stops it with its disk kept)")
	fl.StringVar(&co.Image, "image", "", "boot image family, or project/family (default: your baked family if present, else Ubuntu 24.04 / Windows Server 2025)")
	fl.StringVar(&co.HubURL, "hub-url", "", "hub URL as reached from the node over the tailnet (default: cloud config hub_url)")
	fl.IntVar(&idle, "idle-stop", 0, "power off after this many minutes without live runs (0 disables; default 30)")
	fl.BoolVar(&co.RDP, "rdp", false, "Windows: enable Remote Desktop, reachable from tailnet addresses only")
	fl.StringSliceVar(&co.Toolchains, "toolchain", nil, toolchainFlagHelp)
	fl.DurationVar(&co.Wait, "wait", 0, "how long to wait for the node to connect (default 15m, Windows 60m)")
	fl.BoolVar(&co.NoWait, "no-wait", false, "return once the VM is created")
	fl.DurationVar(&co.TicketTTL, "ticket-ttl", 0, "enrollment ticket lifetime (default 30m, Windows 60m)")
	fl.StringVar(&release, "release-url", "", "release directory (gs://… or https://…; default: cloud config release.url)")
	fl.StringVar(&releaseVer, "release-version", "", "release version (default: cloud config release.version)")
	cf.add(create)
	nodes.AddCommand(create)

	// stop / start / delete / idle
	var sf cloudFlags
	var stopForce bool
	stop := &cobra.Command{
		Use:   "stop <name>",
		Short: "Stop a cloud node's VM (disk kept; sessions resume after start)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			m, err := a.cloudManager(cmd.Context(), sf, true)
			if err != nil {
				return err
			}
			cn, err := m.Stop(cmd.Context(), args[0], stopForce)
			if err != nil {
				return err
			}
			return a.emit(cn, func(w io.Writer) {
				fmt.Fprintf(w, "stopped %s; its disk still costs ~%s/h (pyre node start %s to resume, pyre node delete %s to remove)\n", cn.Name, usd(cn.HourlyUSD, cn.CostKnown), cn.Name, cn.Name)
			})
		}),
	}
	stop.Flags().BoolVar(&stopForce, "force", false, "stop even with live runs (their processes end)")
	sf.add(stop)
	nodes.AddCommand(stop)

	var stf cloudFlags
	var startWait time.Duration
	var startNoWait bool
	start := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a stopped cloud node (fresh single-use Tailscale key) and wait for it",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			m, err := a.cloudManager(cmd.Context(), stf, true)
			if err != nil {
				return err
			}
			cn, err := m.Start(cmd.Context(), args[0], startWait, startNoWait)
			if err != nil {
				if c.IsCode(err, c.ErrOutcomeIndeterminate) {
					return withStatus(ExitIndeterminate, err)
				}
				return err
			}
			return a.emit(cn, func(w io.Writer) {
				fmt.Fprintf(w, "%s is running (%s), ~%s/h\n", cn.Name, orDash(cn.Connection), usd(cn.RunningUSD, cn.CostKnown))
			})
		}),
	}
	start.Flags().DurationVar(&startWait, "wait", 0, "how long to wait for the node to connect (default 15m)")
	start.Flags().BoolVar(&startNoWait, "no-wait", false, "return once the VM is started")
	stf.add(start)
	nodes.AddCommand(start)

	var df cloudFlags
	var delForce bool
	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a cloud node: VM, disks and hub enrollment (refuses with live runs or unpushed work)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			m, err := a.cloudManager(cmd.Context(), df, true)
			if err != nil {
				return err
			}
			res, err := m.Delete(cmd.Context(), args[0], delForce)
			if err != nil {
				return err
			}
			for _, f := range res.Forced {
				a.warnf("forced past: %s", f)
			}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "deleted %s", res.Name)
				if res.Instance != "" {
					fmt.Fprintf(w, " (instance %s", res.Instance)
					if len(res.Disks) > 0 {
						fmt.Fprintf(w, ", leftover disks %s", strings.Join(res.Disks, ", "))
					}
					fmt.Fprint(w, ")")
				}
				if res.RevokedNode != "" {
					fmt.Fprintf(w, "; revoked hub node %s", res.RevokedNode)
				}
				fmt.Fprintln(w)
			})
		}),
	}
	del.Flags().BoolVar(&delForce, "force", false, "delete despite live runs or unpushed work (it is lost)")
	df.add(del)
	nodes.AddCommand(del)

	var idf cloudFlags
	idleCmd := &cobra.Command{
		Use:   "idle <name> <minutes>",
		Short: "Set a cloud node's idle auto-stop (0 disables); takes effect within a minute",
		Args:  cobra.ExactArgs(2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			var min int
			if _, err := fmt.Sscan(args[1], &min); err != nil || min < 0 {
				return usagef("minutes must be a whole number >= 0")
			}
			m, err := a.cloudManager(cmd.Context(), idf, false)
			if err != nil {
				return err
			}
			if err := m.SetIdle(cmd.Context(), args[0], min); err != nil {
				return err
			}
			return a.emit(map[string]any{"name": args[0], "idle_stop_minutes": min}, func(w io.Writer) {
				if min == 0 {
					fmt.Fprintf(w, "%s: idle auto-stop off (it runs, and costs, until stopped)\n", args[0])
				} else {
					fmt.Fprintf(w, "%s: powers off after %d minutes without live runs\n", args[0], min)
				}
			})
		}),
	}
	idf.add(idleCmd)
	nodes.AddCommand(idleCmd)

	// rdp
	var rf cloudFlags
	var rdpUser, rdpFile string
	var rdpReset, rdpOpen bool
	rdp := &cobra.Command{
		Use:   "rdp <name> [--reset-password] [--open] [--user U]",
		Short: "Remote Desktop to a Windows cloud node over the tailnet",
		Long: `Prints the node's tailnet address for Remote Desktop (Windows App /
Microsoft Remote Desktop on a Mac). This machine must be on the same tailnet;
port 3389 is never opened to the internet.

--reset-password sets a new password for the user with
` + "`gcloud compute reset-windows-password`" + ` and prints it once; Pyre never logs
or stores it. For the node's dev user the VM's auto-logon is updated too.
--open writes a .rdp file (no credentials) and opens it.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			m, err := a.cloudManager(cmd.Context(), rf, false)
			if err != nil {
				return err
			}
			ts := a.TailscaleRunner
			if ts == nil {
				ts = cloud.ExecRunner{Bin: "tailscale"}
			}
			info, err := m.RDP(cmd.Context(), ts, args[0], rdpUser, rdpReset, a.CloudHandoff)
			if err != nil {
				return err
			}
			if info.Password != "" {
				a.warnf("the password is shown once; Pyre does not store or log it")
			}
			path := rdpFile
			if rdpOpen && path == "" {
				dir, err := os.MkdirTemp("", "pyre-rdp-")
				if err != nil {
					return err
				}
				path = filepath.Join(dir, info.Name+".rdp")
			}
			if path != "" {
				if err := os.WriteFile(path, []byte(info.RDPFile), 0o600); err != nil {
					return err
				}
			}
			if err := a.emit(info, func(w io.Writer) {
				fmt.Fprintf(w, "%s: Remote Desktop to %s (%s) as %s\n", info.Name, info.Address, info.TailnetName, info.User)
				if !info.Online {
					fmt.Fprintln(w, "warning: the node is not online on the tailnet right now")
				}
				if info.Password != "" {
					fmt.Fprintf(w, "password: %s\n", info.Password)
				} else {
					fmt.Fprintf(w, "no password shown: pyre node rdp %s --reset-password sets one and prints it once\n", info.Name)
				}
				if path != "" {
					fmt.Fprintf(w, "rdp file: %s\n", path)
				}
			}); err != nil {
				return err
			}
			if rdpOpen {
				switch runtime.GOOS {
				case "darwin":
					return a.Exec([]string{"open", path})
				default:
					a.warnf("open it with your RDP client, e.g. xfreerdp /v:%s /u:%s", info.Address, info.User)
				}
			}
			return nil
		}),
	}
	rdp.Flags().StringVar(&rdpUser, "user", "", "Windows user (default: the node's dev user)")
	rdp.Flags().BoolVar(&rdpReset, "reset-password", false, "set a new password via gcloud and print it once")
	rdp.Flags().BoolVar(&rdpOpen, "open", false, "write a .rdp file and open it (macOS: Windows App)")
	rdp.Flags().StringVar(&rdpFile, "rdp-file", "", "write the .rdp file here")
	rf.add(rdp)
	nodes.AddCommand(rdp)

	// image bake
	var bf cloudFlags
	var bo cloud.BakeOptions
	image := &cobra.Command{Use: "image", Short: "Cloud node images"}
	bake := &cobra.Command{
		Use:   "bake [--os linux|windows] [--family F] [--type T] [--base FAMILY] [--toolchain ID]...",
		Short: "Build a node image (harnesses + verified release, no identity) so creates boot in about a minute",
		Long: `Creates a throwaway builder VM that runs the boot script in bake mode
(installs the toolchain, harnesses and the verified Pyre release; no tailnet
login, no enrollment, no credentials), waits for it to power off, makes an
image in the family (default pyre-node, pyre-node-windows) and deletes the
builder. pyre node create uses the family automatically when it exists.`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			m, err := a.cloudManager(cmd.Context(), bf, false)
			if err != nil {
				return err
			}
			res, err := m.Bake(cmd.Context(), bo)
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "image %s in family %s (%s); builder %s deleted\n", res.Image, res.Family, res.Project, res.Builder)
			})
		}),
	}
	bake.Flags().StringVar(&bo.OS, "os", "linux", "linux or windows")
	bake.Flags().StringVar(&bo.Family, "family", "", "image family (default pyre-node / pyre-node-windows)")
	bake.Flags().StringVar(&bo.MachineType, "type", "", "builder machine type (default e2-standard-4)")
	bake.Flags().StringVar(&bo.Image, "base", "", "base image family, or project/family")
	bake.Flags().DurationVar(&bo.Timeout, "timeout", 0, "how long to wait for the builder (default 45m, Windows 120m)")
	bake.Flags().StringSliceVar(&bo.Toolchains, "toolchain", nil, toolchainFlagHelp)
	bf.add(bake)
	image.AddCommand(bake)
	nodes.AddCommand(image)
}
