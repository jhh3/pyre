package codex

import (
	"context"
	"encoding/json"
	"time"
)

// The Codex app-server keeps every thread it ever loaded in memory — with
// its MCP servers (pyre mcp, outfit mcp, …) and background terminals — until
// the app-server exits: thread/unsubscribe does not unload, even for the last
// subscriber (verified on server, 2026-10-10: 98 loaded threads and ~140 MCP
// processes under one app-server with 6 live runs). thread/archive does
// unload, and thread/unarchive restores the thread to the active list
// without loading it, so archive+unarchive releases a thread while leaving
// it resumable exactly as before.

// unloadThread stops a thread's background terminals and unloads it.
func unloadThread(ctx context.Context, c *rpcClient, threadID string) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_ = c.call(cctx, "thread/backgroundTerminals/clean", threadIDParams{ThreadID: threadID}, nil)
	if err := c.call(cctx, "thread/archive", threadIDParams{ThreadID: threadID}, nil); err != nil {
		return err
	}
	return c.call(cctx, "thread/unarchive", threadIDParams{ThreadID: threadID}, nil)
}

// sweepEvery is how often each app-server's loaded threads are checked.
var sweepEvery = 10 * time.Minute

// sweepLoop unloads threads no live run owns, until ctx ends.
func (a *Adapter) sweepLoop(ctx context.Context) {
	if sweepEvery <= 0 {
		return
	}
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	seen := map[string]bool{} // unowned at the previous sweep
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		seen = a.sweepLoaded(ctx, seen)
	}
}

// sweepLoaded unloads loaded threads that were unowned at the previous sweep
// too (a launching or recovering run registers its thread within seconds,
// so two sweeps apart a thread no run owns is dead) and are not active. It
// returns the threads unowned now.
func (a *Adapter) sweepLoaded(ctx context.Context, prev map[string]bool) map[string]bool {
	a.mu.Lock()
	homes := make([]*homeServer, 0, len(a.homes))
	for _, hs := range a.homes {
		homes = append(homes, hs)
	}
	a.mu.Unlock()
	now := map[string]bool{}
	for _, hs := range homes {
		c := hs.connected()
		if c == nil {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		var r struct {
			Data []string `json:"data"`
		}
		err := c.call(lctx, "thread/loaded/list", map[string]any{}, &r)
		cancel()
		if err != nil {
			continue
		}
		hs.mu.Lock()
		owned := map[string]bool{}
		for id := range hs.threads {
			owned[id] = true
		}
		for sub, parent := range hs.parent {
			if hs.threads[parent] != nil {
				owned[sub] = true
			}
		}
		hs.mu.Unlock()
		for _, id := range r.Data {
			if owned[id] {
				continue
			}
			now[id] = true
			if !prev[id] {
				continue
			}
			// A sub-agent of a live run is owned too (after a pyred restart
			// the parent map is empty until notifications arrive).
			if active, parent := threadInfo(ctx, c, id); active || owned[parent] {
				continue
			}
			_ = unloadThread(ctx, c, id)
		}
	}
	return now
}

// threadInfo reports whether a loaded thread has a turn in progress (an
// unknown status counts as active: never unload what cannot be checked) and
// its parent thread (sub-agents).
func threadInfo(ctx context.Context, c *rpcClient, id string) (bool, string) {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var r struct {
		Thread struct {
			Status         json.RawMessage `json:"status"`
			ParentThreadID string          `json:"parentThreadId"`
		} `json:"thread"`
	}
	if err := c.call(rctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &r); err != nil {
		return true, ""
	}
	var st threadStatus
	if json.Unmarshal(r.Thread.Status, &st) != nil {
		return true, r.Thread.ParentThreadID
	}
	return st.Type == "active", r.Thread.ParentThreadID
}
