package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jhh3/pyre/internal/daemon"
)

func defaultState() string { return daemon.DefaultStateDir() }

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	state := fs.String("state", defaultState(), "state directory")
	socket := fs.String("socket", "", "local socket (default <state>/pyred.sock)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sock := *socket
	if sock == "" {
		sock = filepath.Join(*state, "pyred.sock")
	}
	hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	resp, err := hc.Get("http://pyred" + daemon.PathLocalStatus)
	if err != nil {
		return fmt.Errorf("pyred is not running (no socket at %s): %w", sock, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status: HTTP %d: %s", resp.StatusCode, body)
	}
	if *asJSON {
		os.Stdout.Write(body)
		return nil
	}
	var st daemon.LocalStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return err
	}
	dg := st.Diagnostics
	fmt.Printf("node      %s %s\n", st.NodeID, st.NodeName)
	fmt.Printf("version   pyred %s (protocol %s)\n", st.Versions["pyred"], st.Versions["protocol"])
	fmt.Printf("hub link  %s\n", dg.Link)
	if dg.HubClockAheadMS != nil {
		fmt.Printf("hub clock %+.1fs vs this node (commands and leases expire on the hub clock)\n", float64(*dg.HubClockAheadMS)/1000)
	}
	fmt.Printf("state     %s\nsocket    %s\n", st.StateDir, st.Socket)
	fmt.Printf("runs      %d live\n", st.LiveRuns)
	fmt.Printf("overhead  pyred %.1f MB RSS, %.1f%% CPU; agents %d procs %.1f MB RSS, %.1f%% CPU\n",
		dg.Resources.PyreRSSMB, dg.Resources.PyreCPUPct, dg.Agents.Processes, dg.Agents.RSSMB, dg.Agents.CPUPct)
	fmt.Printf("runtime   %d goroutines, %.1f MB heap, %d open fds\n", dg.Runtime.Goroutines, dg.Runtime.HeapAllocMB, dg.Runtime.OpenFDs)
	keys := make([]string, 0, len(dg.Queues))
	for k := range dg.Queues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("queue     %-22s %d\n", k, dg.Queues[k])
	}
	for _, e := range dg.Errors {
		fmt.Printf("error     %s\n", e)
	}
	return nil
}
