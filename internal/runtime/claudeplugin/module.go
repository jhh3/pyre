package claudeplugin

// Plugin prompt delivery (W-plugdeliver): the per-run plugin carries a
// Claude Code hooks module (hooks/hooks.json {"modules": [...]}, the
// function-hooks API present in Claude Code 2.1.281 through 2.1.292) that
// hands Pyre messages to the session with $.prompt.submit: a prompt queued
// between turns, shown as coming from the pyre-session plugin, never typed
// into the TUI's input box. It works while a human is attached or typing.
//
// Protocol, all inside the run state dir:
//
//   - the adapter writes <run>/plugin-inbox/<seq>-<msg>.msg (atomic rename);
//   - the module polls the dir while the session is idle (no main-agent turn
//     running) and claims the oldest file through `<hook> PyrePluginClaim
//     <run>` (pyred renames .msg -> .claimed: a single-consumer claim that
//     races the adapter's .msg -> .withdrawn safely) which prints the text;
//   - it calls $.prompt.submit({text}); the call resolves once the prompt
//     entered the session (its turn started). It then reports
//     PyrePluginSubmitted (acceptance evidence claude.plugin.prompt_submit),
//     PyrePluginDropped (a hook refused it: not entered) or PyrePluginFailed;
//   - one message per idle period; the next one drains after turn.complete,
//     so queued messages arrive in order, each as a turn of its own;
//   - <run>/plugin-alive.json is rewritten every few seconds: the adapter's
//     capability probe (module loaded, Claude version) and liveness check.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Run-dir names shared with the Claude adapter.
const (
	SpoolDir  = "plugin-inbox"
	AliveFile = "plugin-alive.json"
)

// Events the module reports through the run's hook command (appended to
// events.jsonl by pyred hook claude).
const (
	EvPluginReady     = "PyrePluginReady"
	EvPluginClaim     = "PyrePluginClaim"
	EvPluginSubmitted = "PyrePluginSubmitted"
	EvPluginDropped   = "PyrePluginDropped"
	EvPluginFailed    = "PyrePluginFailed"
)

// AliveEvery is how often the module rewrites AliveFile.
const AliveEvery = 5

// Module configures the hooks module.
type Module struct {
	RunDir   string   // the run's state dir (absolute)
	HookArgv []string // e.g. [pyred, hook, claude]; the module appends <event> <run-dir>
	PollMS   int      // spool poll interval while idle (default 500)
}

// Spec is what Write puts into a plugin dir: the doorbell monitor and/or the
// delivery module. (The agent slash commands are not plugin skills: those
// are only reachable as /pyre-session:<name>; see MaterializeSkills.)
type Spec struct {
	MonitorCommand string
	Module         *Module
}

// Write generates the plugin into dir (created 0700) and returns dir.
func Write(dir string, s Spec) (string, error) {
	if s.MonitorCommand == "" && s.Module == nil {
		return "", fmt.Errorf("claudeplugin: nothing to generate")
	}
	for _, d := range []string{dir, filepath.Join(dir, ".claude-plugin")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
	}
	manifest := map[string]any{
		"name":        Name,
		"version":     "1.3.0",
		"description": "Pyre session plugin: delivers Pyre messages between turns and rings the inbox doorbell (generated per run; do not edit)",
		"author":      map[string]string{"name": "Pyre"},
	}
	if err := writeJSON(filepath.Join(dir, ".claude-plugin", "plugin.json"), manifest); err != nil {
		return "", err
	}
	if s.MonitorCommand != "" {
		if err := os.MkdirAll(filepath.Join(dir, "monitors"), 0o700); err != nil {
			return "", err
		}
		monitors := []map[string]string{{
			"name":        MonitorName,
			"command":     s.MonitorCommand,
			"description": MonitorDescription,
		}}
		if err := writeJSON(filepath.Join(dir, "monitors", "monitors.json"), monitors); err != nil {
			return "", err
		}
	}
	if m := s.Module; m != nil {
		if !filepath.IsAbs(m.RunDir) || len(m.HookArgv) == 0 {
			return "", fmt.Errorf("claudeplugin: module needs an absolute run dir and a hook command")
		}
		if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o700); err != nil {
			return "", err
		}
		if err := writeJSON(filepath.Join(dir, "hooks", "hooks.json"), map[string]any{"modules": []string{"./register.ts"}}); err != nil {
			return "", err
		}
		if err := writeFile(filepath.Join(dir, "hooks", "register.ts"), []byte(ModuleSource(*m))); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// ModuleSource renders the hooks module for one run.
func ModuleSource(m Module) string {
	poll := m.PollMS
	if poll <= 0 {
		poll = 500
	}
	runDir, _ := json.Marshal(m.RunDir)
	hook, _ := json.Marshal(m.HookArgv)
	r := strings.NewReplacer(
		"__RUN_DIR__", string(runDir), "__HOOK__", string(hook), "__POLL_MS__", fmt.Sprint(poll),
		"__ALIVE_MS__", fmt.Sprint(AliveEvery*1000), "__SPOOL__", SpoolDir, "__ALIVE__", AliveFile,
		"__EV_READY__", EvPluginReady, "__EV_CLAIM__", EvPluginClaim, "__EV_SUBMITTED__", EvPluginSubmitted,
		"__EV_DROPPED__", EvPluginDropped, "__EV_FAILED__", EvPluginFailed,
	)
	return r.Replace(moduleTemplate)
}

func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

const moduleTemplate = `// Generated by Pyre for one run; do not edit. Delivers Pyre messages with
// $.prompt.submit between turns (see internal/runtime/claudeplugin/module.go).
import type { EngineInterface, Register } from 'claude-code'

type $ = EngineInterface

const RUN_DIR: string = __RUN_DIR__
const HOOK: readonly string[] = __HOOK__
const SPOOL = RUN_DIR + '/__SPOOL__'
const ALIVE = RUN_DIR + '/__ALIVE__'
const POLL_MS = __POLL_MS__
const ALIVE_MS = __ALIVE_MS__

// Module state (a reload starts it over; the spool and claims live on disk).
const S = { busy: false, draining: false, started: false, version: '' }

type Claimed = { message_id: string; attempt_id?: string; text: string }

/** Reports an event to pyred (appended to the run's events.jsonl); returns its stdout. */
async function emit($: $, event: string, payload: Record<string, unknown>): Promise<string> {
  try {
    const r = await $.process.run([...HOOK, event, RUN_DIR], { stdin: JSON.stringify(payload), timeoutMs: 20_000 })
    return r.exitCode === 0 ? r.stdout : ''
  } catch {
    return ''
  }
}

async function alive($: $): Promise<void> {
  try {
    const at = await $.clock.now()
    await $.fs.write(ALIVE, JSON.stringify({ version: S.version, at, busy: S.busy }) + '\n')
  } catch {
    // the run dir is gone: nothing to deliver
  }
}

async function queued($: $): Promise<string[]> {
  try {
    const entries = await $.fs.list(SPOOL)
    return entries.filter(e => e.kind === 'file' && e.name.endsWith('.msg')).map(e => e.name).sort()
  } catch {
    return []
  }
}

/** Submits the oldest queued message while no turn runs: one per idle period, in order. */
async function drain($: $): Promise<void> {
  if (S.busy || S.draining) return
  S.draining = true
  try {
    for (const name of await queued($)) {
      if (S.busy) return
      const out = await emit($, '__EV_CLAIM__', { name })
      let msg: Claimed | undefined
      try {
        msg = out.trim() ? (JSON.parse(out) as Claimed) : undefined
      } catch {
        msg = undefined
      }
      if (!msg || typeof msg.text !== 'string') continue // withdrawn, or claimed already
      const ids = { name, message_id: msg.message_id, attempt_id: msg.attempt_id ?? '' }
      S.busy = true // the prompt's own turn is about to start
      try {
        const r = await $.prompt.submit({ text: msg.text })
        if (r.drop !== undefined) {
          S.busy = false
          await emit($, '__EV_DROPPED__', { ...ids, reason: String(r.drop).slice(0, 500) })
        } else {
          await emit($, '__EV_SUBMITTED__', ids)
        }
      } catch (err) {
        S.busy = false
        await emit($, '__EV_FAILED__', { ...ids, error: String(err).slice(0, 500) })
      }
      return
    }
  } finally {
    S.draining = false
  }
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    const started = await next(e)
    if (!S.started) {
      S.started = true
      try {
        S.version = (await $.session.version()).version
      } catch {
        S.version = ''
      }
      await alive($)
      await emit($, '__EV_READY__', { version: S.version, interactive: e.isInteractive })
      $.clock.every(ALIVE_MS, () => void alive($))
      $.clock.every(POLL_MS, () => void drain($))
    }
    return started
  })
  on('turn.start', ($, e, next) => {
    S.busy = true
    return next(e)
  })
  on('turn.complete', ($, e, next) => {
    if (e.agentId === undefined) {
      S.busy = false
      $.clock.after(100, () => void drain($))
    }
    return next(e)
  })
}
`
