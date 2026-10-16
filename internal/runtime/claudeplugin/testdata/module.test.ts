// Runs Pyre's generated delivery module under Claude Code's own plugin test
// kit (`claude plugin test <plugin dir>`): the engine's real hook chain and
// $ API, with the world beneath (fs, process, prompt, turns) answered here.
// Copied next to the generated module by TestModuleUnderClaudePluginTest;
// the module's run dir is /synthetic/pyre-run. All data is synthetic.
import { expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

const RUN = '/synthetic/pyre-run'
const SPOOL = RUN + '/plugin-inbox'

type Msg = { name: string; message_id: string; text: string }

function world(on: On, queue: Msg[], opts: { drop?: string } = {}) {
  const w = {
    queue,
    claimed: [] as string[],
    events: [] as string[],
    submitted: [] as string[],
    alive: 0,
  }
  on('session.start', ($, e) => ({ cwd: e.cwd }))
  on('turn.start', ($, e) => ({ turnId: e.turnId }))
  on('turn.complete', ($, e) => ({ text: e.answer }))
  on('session.version', () => ({ value: { version: '2.1.291' } }))
  on('fs.write', ($, e) => {
    if (e.path === RUN + '/plugin-alive.json') w.alive++
    return { value: undefined }
  })
  on('fs.list', ($, e) => {
    expect(e.path).toBe(SPOOL)
    return { value: w.queue.map(m => ({ name: m.name, kind: 'file' as const, size: 1, mtimeMs: 0, isLink: false })) }
  })
  on('process.run', ($, e) => {
    const event = e.argv[e.argv.length - 2]
    expect(e.argv[e.argv.length - 1]).toBe(RUN)
    w.events.push(event)
    const input = JSON.parse(e.init?.stdin ?? '{}')
    let stdout = ''
    if (event === 'PyrePluginClaim') {
      const i = w.queue.findIndex(m => m.name === input.name)
      if (i >= 0) {
        const [m] = w.queue.splice(i, 1)
        w.claimed.push(m.message_id)
        stdout = JSON.stringify({ message_id: m.message_id, attempt_id: 'att_synthetic', text: m.text })
      }
    }
    return { value: { exitCode: 0, stdout, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('prompt.submit', ($, e) => {
    if (opts.drop) return { drop: opts.drop }
    w.submitted.push(e.text)
    return { text: e.text }
  })
  return w
}

const msgs = (): Msg[] => [
  { name: '00000000000000000002-msg_b.msg', message_id: 'msg_b', text: '[pyre-msg:msg_b] from session:synthetic-peer:\nsecond' },
  { name: '00000000000000000001-msg_a.msg', message_id: 'msg_a', text: '[pyre-msg:msg_a] from session:synthetic-peer:\nfirst' },
]

const done = (agentId?: string) => ({ answer: '', durationMs: 1, isAborted: false, turnId: 't1', reason: 'answer', ...(agentId ? { agentId } : {}) }) as never

test('submits queued messages in order, one per idle period, and reports each', async ($, on) => {
  const clock = mock.clock(on)
  const w = world(on, msgs())
  await $.session.start({ cwd: '/synthetic', surface: 'terminal', isInteractive: true })
  expect(w.alive).toBeGreaterThan(0)
  expect(w.events).toEqual(['PyrePluginReady'])
  await clock.advance(600)
  expect(w.submitted).toEqual(['[pyre-msg:msg_a] from session:synthetic-peer:\nfirst'])
  expect(w.events).toEqual(['PyrePluginReady', 'PyrePluginClaim', 'PyrePluginSubmitted'])
  // Its turn runs: nothing else is claimed until the main turn completes.
  await $.turn.start({ text: 'x', turnId: 't1' })
  await clock.advance(3000)
  expect(w.claimed).toEqual(['msg_a'])
  // A subagent's turn ending does not free the session.
  await $.turn.complete(done('agent_synthetic'))
  await clock.advance(1000)
  expect(w.claimed).toEqual(['msg_a'])
  await $.turn.complete(done())
  await clock.advance(1000)
  expect(w.claimed).toEqual(['msg_a', 'msg_b'])
  expect(w.submitted.length).toBe(2)
  expect(w.submitted[1]).toContain('second')
})

test('waits while a turn runs (a person typing or attached never blocks it)', async ($, on) => {
  const clock = mock.clock(on)
  const w = world(on, msgs().slice(1))
  await $.session.start({ cwd: '/synthetic', surface: 'terminal', isInteractive: true })
  await $.turn.start({ text: 'owner prompt', turnId: 't0' })
  await clock.advance(5000)
  expect(w.claimed).toEqual([])
  await $.turn.complete(done())
  await clock.advance(1000)
  expect(w.claimed).toEqual(['msg_a'])
  expect(w.submitted.length).toBe(1)
})

test('a dropped prompt is reported, not retried by the module', async ($, on) => {
  const clock = mock.clock(on)
  const w = world(on, msgs().slice(1), { drop: 'synthetic policy refusal' })
  await $.session.start({ cwd: '/synthetic', surface: 'terminal', isInteractive: true })
  await clock.advance(600)
  expect(w.events).toEqual(['PyrePluginReady', 'PyrePluginClaim', 'PyrePluginDropped'])
  expect(w.submitted).toEqual([])
})
