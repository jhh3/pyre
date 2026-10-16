# Generates web/public/fixtures/*.json (synthetic data for ?fixtures=1).
# Usage: python3 web/scripts/gen-fixtures.py web/public/fixtures
import json, datetime, sys, os
OUT = sys.argv[1]
NOW = datetime.datetime(2026, 10, 15, 16, 40, 0, tzinfo=datetime.timezone.utc)
def t(minutes): return (NOW + datetime.timedelta(minutes=minutes)).strftime('%Y-%m-%dT%H:%M:%SZ')
def dump(name, obj):
    obj = {"_now": t(0), **obj}
    with open(os.path.join(OUT, name), 'w') as f:
        json.dump(obj, f, indent=1, ensure_ascii=False); f.write('\n')

P = {
 'studio': {'id': 'prj_fx_studio', 'name': 'Studio billing v2'},
 'web': {'id': 'prj_fx_pyreweb', 'name': 'Pyre web inbox'},
 'voy': {'id': 'prj_fx_vortex', 'name': 'Vortex onboarding'},
 'back': {'id': 'prj_fx_backrooms', 'name': 'Backrooms video'},
}
S = {
 'studio_lead': dict(id='ses_fx_studio_lead', title='Studio lead', state='working', node='server'),
 'billing': dict(id='ses_fx_billing', title='Billing migration', state='needs_you', node='server'),
 'webhooks': dict(id='ses_fx_webhooks', title='Stripe webhooks', state='working', node='mbp'),
 'web_lead': dict(id='ses_fx_web_lead', title='Pyre web lead', state='working', node='server'),
 'web_server': dict(id='ses_fx_web_server', title='web-server', state='needs_you', node='server'),
 'web_ui': dict(id='ses_fx_web_ui', title='web-ui', state='working', node='server'),
 'voy_lead': dict(id='ses_fx_voy_lead', title='Vortex lead', state='idle', node='mbp'),
 'funnel': dict(id='ses_fx_funnel', title='Trial funnel analysis', state='needs_you', node='mbp'),
 'render': dict(id='ses_fx_render', title='Render farm', state='needs_you', node='server'),
}
OWNER = {'kind': 'owner', 'id': 'owner', 'name': 'You'}
def sfrom(s): return {'kind': 'session', 'id': S[s]['id'], 'label': S[s]['title']}
def ofrom(): return {'kind': 'owner', 'id': 'owner', 'label': 'You'}
def deliv(state, s, at):
    name = S[s]['title']
    d = datetime.datetime.strptime(at, '%Y-%m-%dT%H:%M:%SZ').replace(tzinfo=datetime.timezone.utc) - datetime.timedelta(hours=7)
    clk = d.strftime('%I:%M %p').lstrip('0')
    return {'state': state, 'summary': {
        'delivered': f'Delivered to {name} at {clk}', 'read': f'Read by {name} at {clk}',
        'replied': f'{name} replied at {clk}', 'queued': f'Queued for {name}'}[state]}
mid = [0]
def msg(frm, text, at, delivery=None, ask_id=None, reply_to=''):
    mid[0] += 1
    m = {'message_id': f'msg_fx_{mid[0]:03d}', 'from': frm, 'text': text, 'at': at, 'reply_to': reply_to,
         'delivery': delivery or {'state': 'delivered', 'summary': 'Delivered to you'}}
    if ask_id: m['ask_id'] = ask_id
    return m

def ask(aid, proj, ses, title, context, options, default, created, deadline, state='open', answer=None, updates=None, rule='1h', urgent=False, decision=None):
    a = {'ask_id': aid, 'topic_key': aid.replace('ask_fx_', '').replace('_', '-'), 'project_id': P[proj]['id'],
         'session_id': S[ses]['id'], 'title': title, 'context': context, 'options': options,
         'default_option': default, 'urgent': urgent, 'deadline_at': deadline, 'deadline_rule': rule,
         'state': state, 'count': 1, 'message_id': 'msg_ask_' + aid, 'thread_id': 'thr_' + aid,
         'created_at': created, 'updated_at': created}
    if answer: a['answer'] = answer
    if updates: a['updates'] = updates
    if decision: a['decision_message_id'] = decision
    if not default: del a['default_option']
    return a

def thread(kind, tid, title, preview, proj, ses, a, last, unread, msgs, needs_reply=False):
    return {'thread': {'thread_id': tid, 'kind': kind, 'title': title, 'preview': preview,
                       'project': P[proj] if proj else None, 'session': S[ses] if ses else None,
                       'ask': a, 'held': None, 'last_at': last, 'unread': unread,
                       'message_count': len(msgs), 'needs_reply': needs_reply},
            'messages': msgs}

threads = []

# 1. Billing migration: open, deadline soon, back-and-forth, follow-up update.
ctx1 = """**What's being decided:** whether the billing v2 migration (PR #2214) merges *before* Friday's deploy freeze or waits until Monday.

### Why it matters now
The freeze starts **Thursday 6 PM PT**. Three enterprise customers (Halcyon, Meridian Labs, Okapi) are still on legacy invoices, and finance is crediting them by hand each week — about **2 hours of manual work** per cycle. Merging before the freeze ends that this week; waiting means one more manual cycle.

### What I already know
- The PR is rebased on `main`, all 412 tests pass, and the migration ran cleanly against last night's prod snapshot (11 min, no lock waits over 200 ms).
- The risky step is the **cutover**: new invoices start using `billing_v2.line_items`. If the totals are wrong, customers see it on Monday's invoices.
- Rollback is a feature flag (`billing.v2_invoices`) — flipping it reverts invoice generation in under 10 minutes. The schema change itself is additive and safe to leave in place.

| Option | Risk | Manual credits this week |
|---|---|---|
| Merge today | Medium — cutover lands right before the freeze | None |
| Wait until Monday | Low | One more cycle (~2 h) |
| Split | Low — schema now, cutover Monday | One more cycle (~2 h) |

### What happens if nobody answers
At the deadline I take the default (**wait until Monday**), keep the branch rebased, and post a reminder Monday 9 AM."""
opts1 = [
 {'id': 'merge', 'label': 'Merge today, before the freeze', 'detail': 'Ships Thursday 4 PM. I watch the first 2 hours of invoices and flip the flag back if totals drift.'},
 {'id': 'wait', 'label': 'Wait until Monday', 'detail': 'Nothing changes this week; finance does one more round of manual credits.'},
 {'id': 'split', 'label': 'Split it: schema now, cutover Monday', 'detail': 'About half a day of extra work. The risky part moves after the freeze.'},
]
a1 = ask('ask_fx_billing', 'studio', 'billing', "Merge the billing migration before Friday's freeze?", ctx1, opts1, 'wait', t(-38), t(23),
         updates=[{'seq': 1, 'session_id': S['billing']['id'], 'text': 'Update: CI is green on the rebased branch (run 48213) and the prod-snapshot dry run finished in 11 min.', 'at': t(-9)}])
m1 = [
 msg(sfrom('billing'), "**Merge the billing migration before Friday's freeze?**", t(-38), ask_id='ask_fx_billing'),
]
m1[0]['message_id'] = 'msg_ask_ask_fx_billing'
q = msg(ofrom(), "What's the rollback plan if Monday's invoices come out wrong? And who notices first — us or the customer?", t(-24))
q['delivery'] = deliv('replied', 'billing', t(-24)); m1.append(q)
m1.append(msg(sfrom('billing'), """We should notice first. Two layers:

1. **Automatic check** — a nightly job compares v1 and v2 totals for every invoice draft and pages the Studio channel on any difference above $0.50. It has run against the snapshot for 5 nights with zero differences.
2. **Rollback** — flip `billing.v2_invoices` off. Drafts regenerate from v1 within 10 minutes; nothing already sent changes.

Invoices go out Monday 6 AM PT, so even with a merge today there are three nightly checks before any customer sees one.""", t(-17), reply_to=q['message_id']))
threads.append(thread('ask', 'thr_ask_fx_billing', a1['title'], 'We should notice first. Two layers: an automatic nightly check and a feature-flag rollback…', 'studio', 'billing', a1, t(-9), True, m1))

# 2. Hostname: open, held answer.
ctx2 = """**What's being decided:** the public hostname for the Pyre web inbox.

`pyre web serve` is running on server behind ngrok with the Google OAuth policy (only `@example.org` addresses get through). I need one hostname to put in the traffic policy and the Android deep links.

- **pyre-example.ngrok.app** is already reserved on the ngrok account and works today.
- **inbox.example.org** looks nicer but needs a CNAME in Cloudflare and a paid ngrok custom-domain slot (we have one unused).

Either can be changed later; links in old notifications would break.

If nobody answers, I use the ngrok hostname."""
opts2 = [
 {'id': 'ngrok', 'label': 'pyre-example.ngrok.app', 'detail': 'Works now, no DNS changes.'},
 {'id': 'custom', 'label': 'inbox.example.org', 'detail': 'Needs a Cloudflare CNAME; about 30 minutes of setup.'},
]
a2 = ask('ask_fx_hostname', 'web', 'web_server', 'Which hostname for the web inbox?', ctx2, opts2, 'ngrok', t(-12), t(55))
m2 = [msg(sfrom('web_server'), '**Which hostname for the web inbox?**', t(-12), ask_id='ask_fx_hostname')]
m2[0]['message_id'] = 'msg_ask_ask_fx_hostname'
threads.append(thread('ask', 'thr_ask_fx_hostname', a2['title'], 'pyre web serve is running on server behind ngrok with the Google OAuth policy…', 'web', 'web_server', a2, t(-12), False, m2))

# 3. Trial: urgent, no default.
ctx3 = """**What's being decided:** whether to extend the Vortex free trial from **7 to 14 days** starting with today's signups.

### Why now
Paid conversion dropped from 9.1% to 6.4% over the last three weeks. The funnel shows people *activate* fine but don't reach their second project before the trial ends:

| Step | 7-day trial (last 3 weeks) |
|---|---|
| Signed up | 1,284 |
| First generation | 1,027 (80%) |
| Second project | 391 (30%) |
| Converted | 82 (6.4%) |

Users who reach a second project convert at 19%. Median time to a second project is **8.5 days** — just past the trial.

### What each option does
- **Extend to 14 days:** a config change (`trial.days`), no deploy. Revenue from this week's cohort shifts one week later.
- **Keep 7 days, add a day-5 nudge email:** about a day of work; doesn't address the 8.5-day median.

There's **no default**: pricing changes need your call. If this expires, nothing changes."""
opts3 = [
 {'id': 'extend', 'label': 'Extend to 14 days', 'detail': "Config-only; applies to today's signups onward."},
 {'id': 'nudge', 'label': 'Keep 7 days, add a day-5 nudge', 'detail': 'About a day of work; ships next week.'},
 {'id': 'test', 'label': 'A/B test 7 vs 14 days', 'detail': 'Two weeks to a readable result at current volume.'},
]
a3 = ask('ask_fx_trial', 'voy', 'funnel', 'Extend the Vortex free trial to 14 days?', ctx3, opts3, '', t(-55), t(130), urgent=True, rule='custom')
m3 = [msg(sfrom('funnel'), '**Extend the Vortex free trial to 14 days?**', t(-55), ask_id='ask_fx_trial')]
m3[0]['message_id'] = 'msg_ask_ask_fx_trial'
threads.append(thread('ask', 'thr_ask_fx_trial', a3['title'], 'Paid conversion dropped from 9.1% to 6.4% over the last three weeks…', 'voy', 'funnel', a3, t(-55), True, m3))

# 4. Message thread with back-and-forth, needs reply.
m4 = []
r1 = msg(sfrom('render'), """Benchmarks are in for the new renderer on the server farm (same 40-shot test reel, three runs each):

| | Old | New |
|---|---|---|
| Median frame | 2.31 s | 1.27 s |
| Peak VRAM | 21.4 GB | 14.8 GB |
| Failed frames | 3 | 0 |

That's **1.8× faster** with a third less memory. Full report is published as an artifact on the project.""", t(-185))
m4.append(r1)
o1 = msg(ofrom(), 'Nice. Is the output identical, or did anything change visually?', t(-172), reply_to=r1['message_id'])
o1['delivery'] = deliv('replied', 'render', t(-172)); m4.append(o1)
m4.append(msg(sfrom('render'), 'Pixel-identical on 38 of 40 shots. The other two differ only in film grain (seeded differently); side-by-sides are in the report, section 3.', t(-160), reply_to=o1['message_id']))
m4.append(msg(sfrom('render'), 'Want me to switch the farm over tonight? The overnight batch (chapter 4, ~900 frames) would finish around 3 AM instead of 6 AM.', t(-41)))
threads.append(thread('message', 'thr_fx_bench', 'Benchmarks: new renderer is 1.8× faster', 'Want me to switch the farm over tonight? The overnight batch would finish around 3 AM instead of 6 AM.', 'back', 'render', None, t(-41), True, m4, needs_reply=True))

# 5. Yesterday's open ask (Earlier group), morning deadline.
ctx5 = """**What's being decided:** how long to keep the old Stripe webhook endpoint (`/hooks/stripe`) running after the new one (`/hooks/stripe/v2`) takes over.

Both endpoints are live and verify signatures. Stripe retries failed deliveries for up to **3 days**, so anything less than that risks losing events sent to the old URL during the switch. Keeping it longer costs nothing but leaves two code paths to maintain.

If nobody answers by this morning, I keep it for 30 days and add a removal reminder."""
opts5 = [
 {'id': '30d', 'label': 'Keep it for 30 days', 'detail': 'Safe; I add a calendar reminder to remove it on Nov 14.'},
 {'id': '7d', 'label': 'Keep it for 7 days', 'detail': 'Covers Stripe retries with margin; removes the old path sooner.'},
]
a5 = ask('ask_fx_webhook', 'studio', 'webhooks', 'How long to keep the old Stripe webhook endpoint?', ctx5, opts5, '30d', t(-990), t(50), rule='morning')
m5 = [msg(sfrom('webhooks'), '**How long to keep the old Stripe webhook endpoint?**', t(-990), ask_id='ask_fx_webhook')]
m5[0]['message_id'] = 'msg_ask_ask_fx_webhook'
threads.append(thread('ask', 'thr_ask_fx_webhook', a5['title'], 'Both endpoints are live and verify signatures. Stripe retries failed deliveries for up to 3 days…', 'studio', 'webhooks', a5, t(-990), False, m5))

# 5b. Older unanswered status reports (open, more than a day old): the
# "Archive all older than 1 day" link offers to file them away.
m5b = [msg(sfrom('funnel'), 'Weekly funnel export is done: signups +4%, activation flat, trial-to-paid 6.4%. The CSV is published on the project. Want a breakdown by acquisition channel next?', t(-1700))]
threads.append(thread('message', 'thr_fx_funnel_export', 'Weekly funnel export is done', 'Want a breakdown by acquisition channel next?', 'voy', 'funnel', None, t(-1700), False, m5b, needs_reply=True))
m5c = [msg(sfrom('webhooks'), 'Heads-up: Stripe sandbox was flaky for ~20 minutes (5xx on test-mode charges). Retries covered it; nothing lost. Should I add an alert for this?', t(-2600))]
threads.append(thread('message', 'thr_fx_sandbox', 'Heads-up: Stripe sandbox was flaky for ~20 minutes', 'Retries covered it; nothing lost. Should I add an alert for this?', 'studio', 'webhooks', None, t(-2600), False, m5c, needs_reply=True))

# Done: answered by you.
ctx6 = """**What's being decided:** live updates for the web inbox — Server-Sent Events or WebSockets.

SSE is one-way (server → browser), works through ngrok unchanged and reconnects on its own. WebSockets would only matter if the browser had to stream data *to* the server, which it doesn't: answers and replies are plain POSTs.

Default: SSE."""
opts6 = [{'id': 'sse', 'label': 'Server-Sent Events', 'detail': 'Simpler; automatic reconnects; enough for one-way updates.'},
         {'id': 'ws', 'label': 'WebSockets', 'detail': 'Two-way; needs a reconnect layer we would write ourselves.'}]
m6 = [msg(sfrom('web_lead'), '**Server-Sent Events or WebSockets for live updates?**', t(-200), ask_id='ask_fx_sse')]
m6[0]['message_id'] = 'msg_ask_ask_fx_sse'
dm = msg(ofrom(), 'Decision: **Server-Sent Events**', t(-180), ask_id='ask_fx_sse')
dm['delivery'] = deliv('read', 'web_lead', t(-179)); m6.append(dm)
m6.append(msg(sfrom('web_lead'), 'Thanks — going with SSE. web-server owns `/api/events`.', t(-176), reply_to=dm['message_id']))
a6 = ask('ask_fx_sse', 'web', 'web_lead', 'Server-Sent Events or WebSockets for live updates?', ctx6, opts6, 'sse', t(-200), t(-140), state='answered',
         answer={'option_id': 'sse', 'by': OWNER, 'at': t(-180)}, decision=dm['message_id'])
threads.append(thread('ask', 'thr_ask_fx_sse', a6['title'], 'You chose: Server-Sent Events', 'web', 'web_lead', a6, t(-176), False, m6))

# Done: defaulted.
ctx7 = """**What's being decided:** who receives the Vortex onboarding survey — every trial user, or only people who signed up after the new onboarding shipped (Oct 8).

Older users saw a different flow, so their answers would mix two experiences. Default: only new signups."""
opts7 = [{'id': 'all', 'label': 'All trial users', 'detail': 'More responses (~1,200), mixed onboarding versions.'},
         {'id': 'new', 'label': 'Only signups since Oct 8', 'detail': 'About 400 responses, all on the new flow.'}]
dm7 = msg(ofrom(), 'Decision (default at deadline): **Only signups since Oct 8**', t(-1260), ask_id='ask_fx_survey')
dm7['from'] = {'kind': 'owner', 'id': 'owner', 'label': 'Pyre (default)'}
dm7['delivery'] = deliv('delivered', 'voy_lead', t(-1260))
m7 = [msg(sfrom('voy_lead'), '**Who gets the onboarding survey?**', t(-1320), ask_id='ask_fx_survey'), dm7]
m7[0]['message_id'] = 'msg_ask_ask_fx_survey'
a7 = ask('ask_fx_survey', 'voy', 'voy_lead', 'Who gets the onboarding survey?', ctx7, opts7, 'new', t(-1320), t(-1260), state='defaulted',
         answer={'option_id': 'new', 'by': {'kind': 'system', 'id': 'hub', 'name': 'Pyre'}, 'by_default': True, 'at': t(-1260)}, decision=dm7['message_id'])
threads.append(thread('ask', 'thr_ask_fx_survey', a7['title'], 'Defaulted to: Only signups since Oct 8', 'voy', 'voy_lead', a7, t(-1260), False, m7))

# Done: expired.
ctx8 = """**What's being decided:** whether to re-render chapter 3 at 4K before the festival cut. It takes about 14 hours of farm time and blocks chapter 4 renders. No default: this spends the farm for most of a day."""
opts8 = [{'id': 'yes', 'label': 'Re-render at 4K', 'detail': '~14 h of farm time; chapter 4 waits.'},
         {'id': 'no', 'label': 'Keep the 1080p master', 'detail': 'Upscale only if the festival asks.'}]
m8 = [msg(sfrom('render'), '**Re-render chapter 3 at 4K?**', t(-3100), ask_id='ask_fx_4k')]
m8[0]['message_id'] = 'msg_ask_ask_fx_4k'
a8 = ask('ask_fx_4k', 'back', 'render', 'Re-render chapter 3 at 4K?', ctx8, opts8, '', t(-3100), t(-2980), state='expired')
threads.append(thread('ask', 'thr_ask_fx_4k', a8['title'], 'Expired with no answer', 'back', 'render', a8, t(-2980), False, m8))

# Done: withdrawn.
ctx9 = """**What's being decided:** rotate the staging Stripe webhook secret now (staging is down ~5 minutes) or wait for the scheduled rotation on Oct 20."""
opts9 = [{'id': 'now', 'label': 'Rotate now', 'detail': 'Staging is down ~5 minutes.'}, {'id': 'later', 'label': 'Wait for Oct 20', 'detail': 'No downtime now.'}]
m9 = [msg(sfrom('webhooks'), '**Rotate the staging webhook secret now?**', t(-1500), ask_id='ask_fx_rotate'),
      msg(sfrom('webhooks'), 'Withdrawn: not needed — the deploy already rotated the secret.', t(-1440))]
m9[0]['message_id'] = 'msg_ask_ask_fx_rotate'
a9 = ask('ask_fx_rotate', 'studio', 'webhooks', 'Rotate the staging webhook secret now?', ctx9, opts9, 'later', t(-1500), t(-1400), state='withdrawn',
         updates=[{'seq': 1, 'session_id': S['webhooks']['id'], 'text': 'Not needed — the deploy already rotated the secret.', 'at': t(-1440)}])
threads.append(thread('ask', 'thr_ask_fx_rotate', a9['title'], 'Withdrawn: not needed — the deploy already rotated the secret.', 'studio', 'webhooks', a9, t(-1440), False, m9))

# Done: handled message thread.
m10 = [msg(sfrom('studio_lead'), 'Nightly summary: 3 PRs merged (billing fixtures, invoice PDF fonts, webhook retries), 0 incidents. Nothing needs you today.', t(-400))]
ack = msg(ofrom(), 'Thanks!', t(-380), reply_to=m10[0]['message_id']); ack['delivery'] = deliv('read', 'studio_lead', t(-379)); m10.append(ack)
threads.append(thread('message', 'thr_fx_nightly', 'Nightly summary: 3 PRs merged, nothing needs you', 'You: Thanks!', 'studio', 'studio_lead', None, t(-380), False, m10))

held = {'items': [{'ask_id': 'ask_fx_hostname', 'option_id': 'ngrok', 'held_at': t(-1)}], 'sends_at': t(4)}
dump('threads.json', {'held': held, 'threads': threads})
dump('me.json', {'email': 'owner@example.com'})

def card(s, lead=False, line=None, at=None):
    c = dict(S[s]); c['is_lead'] = lead
    c['last_activity'] = {'line': line, 'at': at} if line else None
    return c

pages = {
 'studio': ("""# Studio billing v2

**Goal:** move every customer to usage-based invoices generated from `billing_v2`, and retire the hand-built credits finance does each week.

## Status — on track, one decision pending
- Schema and backfill are done and verified against prod snapshots (5 nights, zero differences).
- **Waiting on you:** merge the migration before Friday's freeze, or wait until Monday. See the open question.
- New Stripe webhook endpoint is live; the old one stays up while retries drain.

## Results so far
| Metric | Before | Now |
|---|---|---|
| Manual credits per week | 11 | 3 |
| Invoice generation time | 41 min | 6 min |
| Invoice disputes (30 days) | 4 | 1 |

## Next
1. Cutover for the last three enterprise customers (Halcyon, Meridian Labs, Okapi).
2. Remove the v1 invoice path two weeks after cutover.
3. Hand the dashboards to finance.

## Links
- PR [#2214 — billing v2 migration](https://example.com/pr/2214)
- Runbook: *Billing v2 rollback* (artifact)
""", 'studio_lead', -125),
 'web': ("""# Pyre web inbox

**Goal:** answer sessions from any device in seconds, with an unmistakable confirmation that the answer went out.

## Status — building
- Server (`pyre web serve`) runs on server behind ngrok, Google sign-in limited to example.org.
- Web app: inbox, threads with options, held answers, project pages. Live updates over SSE.
- **Open:** final hostname (held answer: the ngrok hostname, sending in a few minutes).

## Decisions
- Live updates use **Server-Sent Events** (decided today).
- Held answers wait **5 minutes** after the last hold, then send together.

## Next
- Android deep links into threads.
- Read receipts in the conversation view.
""", 'web_lead', -18),
 'voy': ("""# Vortex onboarding

**Goal:** lift trial → paid conversion back above 9%.

## Status — needs a decision
Conversion slipped to **6.4%**. Users activate but don't reach a second project before the 7-day trial ends (median 8.5 days). The trial-length question is with you.

## What we've tried
- New onboarding flow (Oct 8): activation up from 71% to 80%.
- Onboarding survey to signups since Oct 8 (defaulted yesterday): 112 responses so far.

## Next
- Decide trial length.
- Read the survey at 300 responses.
""", 'voy_lead', -300),
}
cards, details = [], {}
leads = {'studio': 'studio_lead', 'web': 'web_lead', 'voy': 'voy_lead', 'back': None}
sessions = {
 'studio': [card('studio_lead', True, 'Reviewing invoice PDF font fix', t(-6)), card('billing', False, 'Waiting for your answer on the merge window', t(-9)), card('webhooks', False, 'Replaying 14 retried events against v2', t(-3))],
 'web': [card('web_lead', True, 'Integrating web-server and web-ui branches', t(-2)), card('web_server', False, 'Waiting for the hostname answer', t(-12)), card('web_ui', False, 'Taking screenshots for review', t(-1))],
 'voy': [card('voy_lead', True, 'Drafted the survey summary', t(-48)), card('funnel', False, 'Waiting for your answer on trial length', t(-55))],
 'back': [card('render', False, 'Asked whether to switch the farm tonight', t(-41))],
}
summ = {
 'studio': 'Status — on track, one decision pending. Schema and backfill are done and verified against prod snapshots. Waiting on you: merge before Friday’s freeze, or wait until Monday.',
 'web': 'Status — building. Server runs on server behind ngrok, Google sign-in limited to example.org. Web app: inbox, threads with options, held answers, project pages.',
 'voy': 'Status — needs a decision. Conversion slipped to 6.4%. Users activate but don’t reach a second project before the 7-day trial ends.',
 'back': '',
}
desc = {'studio': 'Usage-based invoices on billing_v2 for every customer.', 'web': 'Email-like inbox and project pages for Pyre, on the web.',
        'voy': 'Trial-to-paid conversion for Vortex.', 'back': 'Feature-length render pipeline and festival cut.'}
asks_by = {k: [] for k in P}
for th in threads:
    a = th['thread']['ask']
    if a:
        k = [k for k in P if P[k]['id'] == a['project_id']][0]
        asks_by[k].append(a)
open_counts = {k: sum(1 for a in v if a['state'] == 'open') for k, v in asks_by.items()}
open_counts['back'] += 0
waiting = {k: open_counts[k] > 0 for k in P}
waiting['back'] = True  # the render farm message needs a reply
activity = {'studio': t(-3), 'web': t(-1), 'voy': t(-48), 'back': t(-41)}
arts = {
 'studio': [{'id': 'art_fx_1', 'title': 'Billing v2 rollback runbook', 'url': '/api/artifacts/art_fx_1/content', 'updated_at': t(-200)},
            {'id': 'art_fx_2', 'title': 'Prod snapshot dry-run report (Oct 14)', 'url': '/api/artifacts/art_fx_2/content', 'updated_at': t(-610)}],
 'web': [{'id': 'art_fx_3', 'title': 'Web inbox screenshots — review round 1', 'url': '/api/artifacts/art_fx_3/content', 'updated_at': t(-5)}],
 'voy': [{'id': 'art_fx_4', 'title': 'Trial funnel, last 3 weeks', 'url': '/api/artifacts/art_fx_4/content', 'updated_at': t(-60)}],
 'back': [{'id': 'art_fx_5', 'title': 'Renderer benchmark report', 'url': '/api/artifacts/art_fx_5/content', 'updated_at': t(-185)}],
}
live = {k: sum(1 for c in sessions[k] if c['state'] in ('working', 'needs_you', 'idle', 'starting')) for k in P}
for k, p in P.items():
    lead = leads[k]
    pg = pages.get(k)
    ldr = [c for c in sessions[k] if c['is_lead']]
    cards.append({'project_id': p['id'], 'name': p['name'], 'description': desc[k], 'open_asks': open_counts[k], 'waiting': waiting[k],
                  'leads': ldr, 'page_updated_at': t(pg[2]) if pg else None, 'page_summary': summ[k], 'last_activity_at': activity[k],
                  'live_sessions': live[k], 'active': live[k] > 0 or open_counts[k] > 0})
    proj = {'project_id': p['id'], 'name': p['name'], 'description': desc[k], 'repos': [], 'checkouts': [], 'archived': False, 'version': 3,
            'created_by': OWNER, 'created_at': t(-20000), 'updated_at': activity[k]}
    if pg:
        rev = {'revision_id': 'rev_fx_' + k, 'document_id': 'doc_fx_' + k, 'seq': 7, 'digest': 'sha256:fixture', 'size': len(pg[0]),
               'author': {'kind': 'session', 'id': S[pg[1]]['id'], 'name': S[pg[1]]['title']}, 'created_at': t(pg[2]), 'summary': 'Status update'}
        page = {'project_id': p['id'], 'document': {'document_id': 'doc_fx_' + k, 'project_id': p['id'], 'title': 'Project page', 'slug': 'page',
                'media_type': 'text/markdown', 'head_revision_id': rev['revision_id'], 'head_seq': 7, 'created_by': rev['author'],
                'created_at': t(-9000), 'updated_at': t(pg[2])}, 'revision': rev, 'content': pg[0]}
    else:
        page = {'project_id': p['id'], 'content': ''}
    asks = sorted(asks_by[k], key=lambda a: (a['state'] != 'open', a['created_at']))
    details[p['id']] = {'project': proj, 'page': page, 'sessions': sessions[k], 'asks': asks, 'artifacts': arts[k]}
cards.sort(key=lambda c: (c['name'].lower(), c['project_id']))
dump('projects.json', {'projects': cards})
dump('project-details.json', {'details': details})
