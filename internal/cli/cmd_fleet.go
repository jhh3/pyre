package cli

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/enrollment"
	"github.com/jhh3/pyre/internal/sysstats"
)

func (a *App) addFleetCommands(root *cobra.Command) {
	var includeRevoked, cloudLs bool
	var lsCloud cloudFlags
	nodesLs := func(cmd *cobra.Command, args []string) error {
		if cloudLs {
			return a.cloudList(cmd, lsCloud) // W-gcp
		}
		ops, err := a.connect()
		if err != nil {
			return err
		}
		page, err := ops.ListNodes(cmd.Context(), includeRevoked)
		if err != nil {
			return err
		}
		return a.emit(page, func(w io.Writer) { printNodes(w, page.Items, page.SnapshotAt) })
	}
	nodes := &cobra.Command{
		Use:     "nodes [ls|show <node>]",
		Aliases: []string{"node"},
		Short:   "List enrolled machines (identity, connectivity, harnesses, load)",
		Args:    cobra.NoArgs,
		RunE:    a.run(nodesLs),
	}
	nodes.PersistentFlags().BoolVar(&includeRevoked, "all", false, "include revoked nodes")
	nodes.Flags().BoolVar(&cloudLs, "cloud", false, "list Pyre cloud VMs (gcloud): type, state, uptime, estimated $/h")
	lsCloud.add(nodes)
	nodesLsCmd := &cobra.Command{Use: "ls", Short: "List nodes", Args: cobra.NoArgs, RunE: a.run(nodesLs)}
	nodesLsCmd.Flags().BoolVar(&cloudLs, "cloud", false, "list Pyre cloud VMs (gcloud): type, state, uptime, estimated $/h")
	lsCloud.add(nodesLsCmd)
	nodes.AddCommand(nodesLsCmd)
	a.addCloudNodeCommands(nodes) // W-gcp
	a.addDrainCommands(nodes)     // node drain / node resume
	nodes.AddCommand(&cobra.Command{
		Use:   "show <node>",
		Short: "Show one node (ID, name, ssh alias or hostname)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			n, err := ops.ResolveNode(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.emit(n, func(w io.Writer) { printNode(w, n) })
		}),
	})
	nodes.AddCommand(&cobra.Command{
		Use:   "command <command_id>",
		Short: "Show a node command's outcome and final result (e.g. a slow node action)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			res, err := ops.API.GetCommandResult(cmd.Context(), c.GetRequest{ID: args[0]})
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "%s: %s\n", res.CommandID, res.Outcome)
				if res.Error != nil {
					fmt.Fprintf(w, "  error: %s\n", res.Error.Error())
				}
				if len(res.Result) > 0 {
					fmt.Fprintf(w, "  result: %s\n", res.Result)
				}
			})
		}),
	})
	root.AddCommand(nodes)

	root.AddCommand(&cobra.Command{
		Use:   "clients",
		Short: "List paired owner clients (CLI, Android)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			page, err := listAll(cmd.Context(), func(ctx contextT, cur string) (c.Page[c.ClientInfo], error) {
				return ops.API.ListClients(ctx, c.PageRequest{Cursor: cur})
			})
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) {
				var rows [][]string
				for _, cl := range page.Items {
					st := "active"
					if cl.Revoked {
						st = "revoked"
					}
					rows = append(rows, []string{cl.ClientID, cl.Name, cl.Kind, st, agoPtr(cl.LastSeenAt)})
				}
				table(w, "ID\tNAME\tKIND\tSTATE\tLAST SEEN", rows)
			})
		}),
	})

	var enName, enSSH, enPyred, enRemoteDir, enHubURL, enTargetOS string
	var enTTL int
	var enBootstrap, enExecute bool
	enroll := &cobra.Command{
		Use:   "enroll-node --name NAME [--hub-url URL] [--ssh ALIAS] [--bootstrap [--execute] [--target-os auto|unix|windows]]",
		Short: "Create a one-time enrollment ticket for a machine (optionally install pyred over ssh)",
		Long: `Create a one-time node enrollment ticket. The secret is shown once.
--hub-url sets the hub URL the ticket carries (the hub as reachable from the
node), with or without --bootstrap.

With --bootstrap --ssh ALIAS, print the exact scp/ssh steps that install pyred
on the host through your existing ssh alias and enroll it (dry run, no ticket
is created). Add --execute to create the ticket and run the steps; the
secret travels on ssh stdin, never in argv. --pyred must be a binary built
for the target platform.

The target's OS is probed before the ticket is created (ssh ALIAS uname -s,
else a PowerShell probe); --target-os unix|windows skips the probe. A dry run
does not connect: it shows the unix steps unless --target-os windows. Windows
targets get PowerShell steps (any OpenSSH login shell) and install
bin\pyred.exe and node-credential.json in %LOCALAPPDATA%\Programs\Pyre, where
install.ps1 -Service node picks the credential up; --pyred must then be a
pyred.exe built for windows.

Run from an agent session, the request is filed for the owner's approval
(exit 8); once he approves, run the same command again to receive the
ticket (its secret never travels in a message).`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if enName == "" {
				return usagef("--name is required")
			}
			if enBootstrap && enSSH == "" {
				return usagef("--bootstrap needs --ssh ALIAS")
			}
			if enExecute && !enBootstrap {
				return usagef("--execute only applies with --bootstrap")
			}
			target := ""
			switch enTargetOS {
			case "", "auto":
			case "unix", "linux", "darwin", "macos":
				target = enrollment.TargetUnix
			case "windows":
				target = enrollment.TargetWindows
			default:
				return usagef("--target-os must be auto, unix (linux, darwin) or windows (got %q)", enTargetOS)
			}
			if enTargetOS != "" && !enBootstrap {
				return usagef("--target-os only applies with --bootstrap")
			}
			if enHubURL != "" {
				if hu, err := url.Parse(enHubURL); err != nil || hu.Scheme != "https" || hu.Host == "" {
					return usagef("--hub-url must be https://host[:port] reachable from the node (got %q)", enHubURL)
				}
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			pin := a.pinFlag
			if pin == "" && a.conn != nil {
				pin = a.conn.Pin // the client config's pin for this hub
			}
			if enBootstrap && !enExecute {
				hub := enHubURL
				if hub == "" && a.conn != nil {
					hub = a.conn.HubURL
				}
				shown := target
				if shown == "" {
					shown = enrollment.TargetUnix // a dry run does not connect to probe
				}
				placeholder := c.EnrollmentTicket{NodeID: "node_PENDING", Secret: "pye_dry-run-placeholder-secret", HubURL: hub}
				plan, err := enrollment.SSHBootstrapPlanFor(shown, enSSH, a.bootstrapPyred(enPyred, shown), hub, pin, placeholder, enRemoteDir)
				if err != nil {
					return err
				}
				steps := append([]enrollment.Step(nil), plan.Steps...)
				for i := range steps {
					if steps[i].Stdin != "" {
						steps[i].Stdin = "<enrollment secret>"
					}
				}
				return a.emit(steps, func(w io.Writer) {
					fmt.Fprint(w, "dry run (no ticket created): "+plan.String())
					if target == "" {
						fmt.Fprintln(w, "the target OS is probed at --execute (uname -s, else PowerShell); --target-os windows previews the Windows steps")
					}
					fmt.Fprintln(w, "re-run with --execute to create the ticket and run these steps")
				})
			}
			if enBootstrap {
				if target == "" {
					// Probe before the ticket exists: an unreachable host wastes nothing.
					if target, err = enrollment.ProbeTarget(cmd.Context(), a.SSHRunner, enSSH); err != nil {
						return err
					}
					fmt.Fprintf(a.Stderr, "==> %s is a %s host\n", enSSH, target)
				}
				if target == enrollment.TargetWindows && enPyred == "" {
					return usagef("%s runs Windows: pass --pyred with a pyred.exe built for windows (amd64 or arm64)", enSSH)
				}
			}
			t, err := ops.API.CreateEnrollment(cmd.Context(), c.EnrollmentRequest{Mutation: mut(a.opID), Name: enName, SSHAlias: enSSH, TTL: enTTL})
			if err != nil {
				return err
			}
			if enHubURL != "" {
				t.HubURL = enHubURL // the hub as reachable from the node; pyred enroll --ticket-stdin uses it
			}
			if !enBootstrap {
				a.warnf("the enrollment secret is shown once; it expires %s", stamp(t.ExpiresAt))
				return a.emit(t, func(w io.Writer) {
					fmt.Fprintf(w, "node %s (%s) enrollment ticket, expires %s\n", t.NodeID, enName, stamp(t.ExpiresAt))
					fmt.Fprintf(w, "secret: %s\n", t.Secret)
					pin := ""
					if t.HubCertSHA256 != "" {
						pin = " --pin " + t.HubCertSHA256
					}
					fmt.Fprintf(w, "on the node: pyred enroll --hub %s%s --node-id %s --secret-stdin   (then paste the secret)\n", t.HubURL, pin, t.NodeID)
				})
			}
			if t.HubCertSHA256 != "" && a.pinFlag == "" {
				pin = "" // the ticket's own pin (SSHBootstrapPlan falls back to it)
			}
			plan, err := enrollment.SSHBootstrapPlanFor(target, enSSH, a.bootstrapPyred(enPyred, target), enHubURL, pin, t, enRemoteDir)
			if err != nil {
				return err
			}
			if err := enrollment.Execute(cmd.Context(), plan, a.SSHRunner, enrollment.ExecuteOptions{Out: a.Stderr}); err != nil {
				return withStatus(ExitIndeterminate, fmt.Errorf("%w; the node may be partially installed: check with `pyre nodes show %s`", err, t.NodeID))
			}
			return a.emit(map[string]string{"node_id": t.NodeID, "alias": enSSH, "target_os": target, "credential_file": plan.CredentialFile}, func(w io.Writer) {
				fmt.Fprintf(w, "%s enrolled as %s; it appears in `pyre nodes` once pyred connects\n", enSSH, t.NodeID)
				if target == enrollment.TargetWindows {
					fmt.Fprintf(w, "next, on the node: install.ps1 -Zip <pyre-windows zip> -Service node -Name %s (it uses %s)\n", enName, plan.CredentialFile)
				}
			})
		}),
	}
	f := enroll.Flags()
	f.StringVar(&enName, "name", "", "node name (mutable alias), e.g. server")
	f.StringVar(&enSSH, "ssh", "", "ssh alias used to reach the machine")
	f.IntVar(&enTTL, "ttl", 0, "ticket lifetime in seconds (hub default if 0)")
	f.BoolVar(&enBootstrap, "bootstrap", false, "install pyred and enroll over ssh (dry run unless --execute)")
	f.BoolVar(&enExecute, "execute", false, "with --bootstrap: create the ticket and run the steps")
	f.StringVar(&enPyred, "pyred", "", "local pyred binary for the target platform (default: next to pyre, or on PATH; required for Windows targets)")
	f.StringVar(&enRemoteDir, "remote-dir", "", "install directory on the node (default "+enrollment.DefaultRemoteDir+"; Windows "+enrollment.DefaultWindowsRemoteDir+")")
	f.StringVar(&enHubURL, "hub-url", "", "hub URL as reachable from the node (default: the hub's own)")
	f.StringVar(&enTargetOS, "target-os", "", "with --bootstrap: auto (probe over ssh), unix or windows")
	root.AddCommand(enroll)

	var pcName, pcKind string
	var pcTTL int
	pairClient := &cobra.Command{
		Use:   "pair-client --name NAME [--kind android|cli]",
		Short: "Create a one-time pairing link for a new client (Android app, another CLI)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if pcName == "" {
				return usagef("--name is required")
			}
			switch pcKind {
			case "cli", "android", "tui":
			default:
				return usagef("--kind must be android, cli or tui")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			t, err := ops.API.CreatePairing(cmd.Context(), c.PairingRequest{Mutation: mut(a.opID), ClientName: pcName, Kind: pcKind, TTL: pcTTL})
			if err != nil {
				return err
			}
			a.warnf("the pairing link is one-time and expires %s; treat it like a password", stamp(t.ExpiresAt))
			return a.emit(t, func(w io.Writer) {
				fmt.Fprintf(w, "%s\n", t.URI)
				fmt.Fprintf(w, "pairing %s for %s (%s), expires %s\n", t.PairingID, pcName, pcKind, stamp(t.ExpiresAt))
				if pcKind == "cli" {
					fmt.Fprintln(w, "on the other machine: run `pyre pair` and paste the link (never pass it as an argument)")
				} else {
					fmt.Fprintln(w, "open the link on the device or paste it into the app's pairing screen")
				}
			})
		}),
	}
	pairClient.Flags().StringVar(&pcName, "name", "", "client name")
	pairClient.Flags().StringVar(&pcKind, "kind", "android", "client kind: android, cli or tui")
	pairClient.Flags().IntVar(&pcTTL, "ttl", 0, "link lifetime in seconds (hub default if 0)")
	root.AddCommand(pairClient)

	var revokeReason string
	revoke := &cobra.Command{
		Use:   "revoke node|client <id>",
		Short: "Revoke a node's or client's credential",
		Long: `Revoke a node's or client's credential. From an agent session, revoking a
node is filed for the owner's approval (exit 8); Pyre revokes it when he
approves and messages the session the outcome.`,
		Args: cobra.ExactArgs(2),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			switch args[0] {
			case "node":
				n, err := ops.ResolveNode(cmd.Context(), args[1])
				if err != nil {
					return err
				}
				out, err := ops.API.RevokeNode(cmd.Context(), c.RevokeRequest{Mutation: mut(a.opID), ID: n.NodeID, Reason: revokeReason})
				if err != nil {
					return err
				}
				return a.emit(out, func(w io.Writer) { fmt.Fprintf(w, "revoked node %s (%s)\n", out.NodeID, out.Name) })
			case "client":
				out, err := ops.API.RevokeClient(cmd.Context(), c.RevokeRequest{Mutation: mut(a.opID), ID: args[1], Reason: revokeReason})
				if err != nil {
					return err
				}
				return a.emit(out, func(w io.Writer) { fmt.Fprintf(w, "revoked client %s (%s)\n", out.ClientID, out.Name) })
			}
			return usagef("revoke takes `node` or `client`")
		}),
	}
	revoke.Flags().StringVar(&revokeReason, "reason", "", "reason recorded with the revocation")
	root.AddCommand(revoke)
}

// bootstrapPyred is the local binary a bootstrap copies: --pyred, else (for
// a unix target) pyredPath's default. A Windows target needs --pyred (the
// command refuses earlier); the placeholder only shapes a dry run.
func (a *App) bootstrapPyred(flag, target string) string {
	if flag == "" && target == enrollment.TargetWindows {
		return "pyred.exe"
	}
	return a.pyredPath(flag)
}

func (a *App) pyredPath(flag string) string {
	if flag != "" {
		return flag
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "pyred")
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	if p, err := exec.LookPath("pyred"); err == nil {
		return p
	}
	return "pyred"
}

// connText describes a node's connection. ref is the hub's clock at read
// time (page snapshot_at) when known, so "seen 2m ago" is measured on the
// hub clock that stamped last_seen_at, not on this client's clock.
func connText(n c.Node, ref time.Time) string {
	switch n.Connection.State {
	case c.ConnConnected:
		return "connected"
	case c.ConnDisconnected:
		seen := "-"
		if t := n.Connection.LastSeenAt; t != nil {
			seen = agoFrom(ref, *t)
		}
		return "disconnected (seen " + seen + ")"
	case c.ConnNever:
		return "never connected"
	}
	return string(n.Connection.State)
}

func harnessText(hs []c.HarnessInfo) string {
	var parts []string
	for _, h := range hs {
		s := string(h.Harness)
		if h.Version != "" {
			s += " " + h.Version
		}
		if !h.Available {
			s += " (unavailable)"
		}
		parts = append(parts, s)
	}
	return orDash(strings.Join(parts, ", "))
}

func printNodes(w io.Writer, ns []c.Node, ref time.Time) {
	sort.SliceStable(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	var rows [][]string
	for _, n := range ns {
		runs := fmt.Sprintf("%d", n.Resources.ActiveRuns)
		if n.Resources.MaxRuns > 0 {
			runs += fmt.Sprintf("/%d", n.Resources.MaxRuns)
		}
		cpu, mem, swap, disk := machineCells(n.Resources)
		rows = append(rows, []string{n.Name, n.NodeID, n.Platform + "/" + n.Arch, connText(n, ref), harnessText(n.Harnesses), runs, cpu, mem, swap, disk})
	}
	table(w, "NAME\tID\tPLATFORM\tCONNECTION\tHARNESSES\tRUNS\tCPU\tMEM\tSWAP\tDISK FREE", rows)
	for _, n := range ns {
		if ws := sysstats.Warnings(n.Resources); len(ws) > 0 {
			fmt.Fprintf(w, "! %s: %s\n", n.Name, strings.Join(ws, "; "))
		}
	}
}

// machineCells formats the machine stats columns; "-" when not reported.
// A trailing "!" marks a warning (low disk, memory pressure, heavy swap).
func machineCells(r c.NodeResources) (cpu, mem, swap, disk string) {
	cpu, mem, swap, disk = "-", "-", "-", "-"
	switch {
	case r.CPUPct > 0:
		cpu = fmt.Sprintf("%.0f%% %.1f/%d", r.CPUPct, r.Load1, r.CPUCount)
	case r.Load1 > 0:
		cpu = fmt.Sprintf("%.1f/%d", r.Load1, r.CPUCount)
	}
	if r.MemTotalMB > 0 {
		mem = sysstats.HumanMB(float64(r.MemTotalMB-r.MemAvailMB)) + "/" + sysstats.HumanMB(float64(r.MemTotalMB))
		if sysstats.PressureHigh(r) {
			mem += " " + r.MemPressure + "!"
		}
	}
	if r.SwapTotalMB > 0 {
		swap = sysstats.HumanMB(float64(r.SwapUsedMB)) + "/" + sysstats.HumanMB(float64(r.SwapTotalMB))
		if sysstats.SwapHeavy(r) {
			swap += "!"
		}
	}
	if d, ok := lowestDisk(r.Disks); ok {
		disk = fmt.Sprintf("%s %.0f%%", sysstats.HumanMB(float64(d.FreeMB)), 100*float64(d.FreeMB)/float64(d.TotalMB))
		if sysstats.DiskLow(d) {
			disk += "!"
		}
	}
	return cpu, mem, swap, disk
}

func lowestDisk(ds []c.DiskUsage) (c.DiskUsage, bool) {
	var best c.DiskUsage
	ok := false
	for _, d := range ds {
		if d.TotalMB <= 0 {
			continue
		}
		if !ok || float64(d.FreeMB)/float64(d.TotalMB) < float64(best.FreeMB)/float64(best.TotalMB) {
			best, ok = d, true
		}
	}
	return best, ok
}

func printNode(w io.Writer, n c.Node) {
	fmt.Fprintf(w, "%s  %s\n", n.Name, n.NodeID)
	fmt.Fprintf(w, "  platform:    %s/%s  hostname %s  ssh %s\n", n.Platform, n.Arch, orDash(n.Hostname), orDash(n.SSHAlias))
	fmt.Fprintf(w, "  connection:  %s\n", connText(n, time.Time{}))
	if n.Connection.State != c.ConnConnected {
		fmt.Fprintln(w, "               (offline does not mean idle: runs may still be executing)")
	}
	fmt.Fprintf(w, "  harnesses:   %s\n", harnessText(n.Harnesses))
	for _, h := range n.Harnesses {
		if !h.Available && h.Reason != "" {
			fmt.Fprintf(w, "               %s: %s\n", h.Harness, h.Reason)
		}
	}
	r := n.Resources
	fmt.Fprintf(w, "  resources:   %d cpu, load %.2f, mem %d/%d MB free, runs %d (max %d), observed %s\n", r.CPUCount, r.Load1, r.MemAvailMB, r.MemTotalMB, r.ActiveRuns, r.MaxRuns, ago(r.ObservedAt))
	if r.CPUPct > 0 || r.Load5 > 0 {
		fmt.Fprintf(w, "  cpu:         %.1f%% busy, load %.2f %.2f %.2f\n", r.CPUPct, r.Load1, r.Load5, r.Load15)
	}
	if r.MemPressure != "" {
		p := r.MemPressure
		if r.MemPressureSome10 > 0 {
			p += fmt.Sprintf(" (psi some avg10 %.1f%%)", r.MemPressureSome10)
		}
		fmt.Fprintf(w, "  pressure:    %s\n", p)
	}
	if r.SwapTotalMB > 0 {
		fmt.Fprintf(w, "  swap:        %d/%d MB used\n", r.SwapUsedMB, r.SwapTotalMB)
	}
	for _, d := range r.Disks {
		fmt.Fprintf(w, "  disk:        %s (%s) %s free of %s\n", d.Path, strings.Join(d.Roles, "+"), sysstats.HumanMB(float64(d.FreeMB)), sysstats.HumanMB(float64(d.TotalMB)))
	}
	for _, u := range r.Runs {
		fmt.Fprintf(w, "  run usage:   %s %s\n", u.RunID, sysstats.Detail(u))
	}
	for _, x := range sysstats.Warnings(r) {
		fmt.Fprintf(w, "  WARNING:     %s\n", x)
	}
	if len(n.Versions) > 0 {
		var vs []string
		for _, k := range sortedKeys(n.Versions) {
			vs = append(vs, k+" "+n.Versions[k])
		}
		fmt.Fprintf(w, "  versions:    %s\n", strings.Join(vs, ", "))
	}
	if len(n.Capabilities) > 0 {
		fmt.Fprintf(w, "  capabilities: %s\n", strings.Join(n.Capabilities, ", "))
	}
	fmt.Fprintf(w, "  enrolled:    %s\n", stamp(n.EnrolledAt))
}
