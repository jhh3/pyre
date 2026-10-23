# Pyre web: inbox and project pages

`pyre web serve` is the owner's web app: an email-like inbox (asks with
their options, plain message threads you can reply to, answers sent now or
held for a 5-minute batch) and one page per project. It runs on one always-on
machine (server) and covers every machine's sessions, because it reads the hub
with the owner's normal client credentials, exactly like `pyre tui`.

```
browser ──https──> ngrok edge (Google sign-in, @example.org only)
                     │ adds X-Pyre-Email: <email>
                     ▼
            ngrok agent on server ──> 127.0.0.1:7390  pyre web serve ──> hub
```

## Install and run (server)

Prerequisites: `pyre` installed (`~/.local/bin/pyre`), `pyre login` done for
the owner (`~/.config/pyre/client.json`), `ngrok` at `/usr/local/bin/ngrok`
with its authtoken configured (`ngrok config add-authtoken ...`, stored in
ngrok's own config) and the reserved domain `pyre-example.ngrok.app` on that
ngrok account.

```sh
install -m 0644 packaging/systemd/pyre-web.service ~/.config/systemd/user/
install -d -m 0700 ~/.config/pyre/ngrok
install -m 0644 packaging/ngrok/pyre-web-policy.yml ~/.config/pyre/ngrok/
install -m 0644 packaging/systemd/pyre-web-tunnel.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now pyre-web pyre-web-tunnel
loginctl enable-linger "$USER"   # keep both running after logout
```

Open https://pyre-example.ngrok.app and sign in with an @example.org Google
account.

Upgrade: install the new `pyre`, then `systemctl --user restart pyre-web`
(the app's files are embedded in the binary). Rollback: reinstall the
previous `pyre` and restart; the server keeps no data that a version change
affects (read marks only). Remove: `systemctl --user disable --now
pyre-web-tunnel pyre-web` and delete the two unit files.

### Flags

| Flag | Default | Meaning |
|---|---|---|
| `--listen` | `127.0.0.1:7390` | Loopback address; anything else is refused. |
| `--state-dir` | `$XDG_STATE_HOME/pyre/web`, else `~/.local/state/pyre/web` | Read marks (`read.json`, 0600). |
| `--origin` | `https://pyre-example.ngrok.app` | Public origins accepted on POSTs (besides the request's own host). |
| `--dev` | off | Admit loopback requests without `X-Pyre-Email` (local testing). |

The connection flags are the usual ones (`--hub`, `--config`, `PYRE_HUB`,
...). The server refuses to start unless the credential is the owner's (an
agent session's `PYRE_SESSION_TOKEN` is not); the unit unsets the session and
proxy variables.

Local testing without the tunnel:
`pyre web serve --dev --listen 127.0.0.1:7391`, then open
http://127.0.0.1:7391. Never combine `--dev` with the tunnel: ngrok's agent
connects from loopback, so `--dev` would admit unauthenticated tunnel traffic
if the policy were ever removed.

## How auth works

1. ngrok's traffic policy (`packaging/ngrok/pyre-web-policy.yml`) first
   removes any `X-Pyre-Email` the client sent, then requires Google OAuth,
   answers 403 unless the email ends with `@example.org`, and adds
   `X-Pyre-Email: <email>` from the OAuth identity.
2. The server listens on loopback only and rejects every request (API and
   app files) without that header (401) or with another domain, a
   subdomain, or more than one header (403). `GET /api/me` returns the email.
3. POSTs must be `Content-Type: application/json`, carry an `Origin` equal
   to the request's host or a `--origin`, and not be `Sec-Fetch-Site:
   cross-site` (CSRF).
4. Every response has a Content-Security-Policy allowing only the app's own
   files (no inline scripts, no third-party hosts, no framing),
   `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`,
   `Referrer-Policy: no-referrer`. API responses are `no-store`.
   Published artifacts are served with `Content-Security-Policy: sandbox` so
   an HTML artifact cannot run script as the app.

A process on server itself can reach 127.0.0.1:7390 and send the header; the
trust boundary is the machine (the hub credential lives there anyway).

## Behaviour

- **Inbox (open)**: open asks, plus message threads where a session wrote to
  you after your last message and you have not answered, acknowledged or
  opened (an update, mail that asked nothing of you, is stored acknowledged
  and only ever appears in Done; docs/experience.md),
  plus session items (thread kind `attention`, thread ID = the attention
  ID): a lead's "waiting on you" (it ended its turn with a question in its
  own chat) and a session's own "needs you" flag. Their thread shows the
  excerpt and the owner's messages to the session between the item's
  creation and the session's next item of that kind (a new question is a
  new item, so a new conversation; a reply typed in a thread stays in it,
  remembered in the server's `read.json`); a reply goes to
  the session as chat (no thread, so it also answers the session's open
  asks), and Archive dismisses the item (`attention.update` handled).
  **Done**: decided asks and answered threads from the last 14 days.
- **Threads**: a thread is the hub thread (`thread_id`). An ask's thread is
  `Ask.ThreadID` (or `ask:<ask_id>` on hubs that do not thread asks). A
  message a session sent without a thread is keyed by its own message ID;
  replies from the web carry that key as their thread ID, so the agent's
  `send_message reply_to` answer lands in the same thread.
- **Reply** sends a queue-mode message from you to the thread's session,
  replying to the newest message that session wrote in the thread (the ask's
  carrying message if it has not written since). A reply in an ask's thread
  does not answer the ask (it may be a question back); **Answer with this**
  in the composer of an open ask sends the text as the answer
  (`POST /api/asks/{id}/answer` with `text`). Writing to the session itself
  (its terminal, `pyre send`) answers its open asks (docs/experience.md,
  Asks).
- **Answers**: `mode: "now"` answers at once (AnswerAsks); `mode: "hold"`
  puts it in the hub's held queue, which sends after 5 quiet minutes (each
  new hold restarts the wait), on "send now", or before the ask's deadline.
  Held answers live in the hub, so the TUI and phone see the same queue.
- **Archive** (email-style): archiving a message thread acknowledges, as
  the owner, every session message in it nobody acknowledged (the hub's
  `messages.ack`, as `pyre ack` does) and marks its attention items read,
  so it moves from Open to Done. There is no unarchive; a new message from
  the session opens the thread again. An open ask is archived only by
  dismissing it: `{"dismiss": true}` withdraws it as the owner (reason
  `contracts.AskDismissReason`: no option, its default never applies, the
  session is told); without it the request is refused (409), and bulk
  archive never dismisses. The app asks to confirm ("Dismiss this
  question?"). A decided ask's thread is already in Done
  (only unanswered follow-ups in it are acknowledged). In the app: the
  Archive button in the thread header, `e`, a per-row archive button on
  open message threads (on hover; always shown on touch screens), and
  "Archive all older than 1 day" under the Open list (asks for
  confirmation with the count). After archiving the open thread the view
  moves to the next one in the list.
- **Unread**: the server remembers when you opened each thread here
  (`read.json`); opening a thread also marks its attention items read at the
  hub (a message's item handled: reading it is all it asks) and acknowledges
  the session messages in it (not an open ask's carrier, which closes with
  its ask), so a message thread moves to Done once read; an item another
  surface marked read is not shown unread. The app sends the read for an
  unread thread and for a message thread still waiting on you.
- **Delivery** lines are plain English from the message's stored delivery
  facts: "Sent 9:41 AM · waiting: <session> is working (...)", "Delivered to
  <session> at 9:41 AM", "Read by the agent at 9:42 AM", "<session> replied
  at 9:50 AM", or "Not delivered: <why>". Times are America/Los_Angeles.
- **Projects**: one card per project with its leads (top-level sessions in
  the project), open asks, whether something waits on you, the lead's
  progress and phase, a "Stale" chip, and the start of the project page; the
  project view opens with a live status card, then the page (markdown,
  maintained by the lead), sessions (leads first), asks (open, then the last
  10 decided) and the 20 most recent artifacts.
- **Status card**: the lead writes progress (0-100%), phase, ETA, in-flight
  items, items ready for the owner (PRs to review), shipped items (merged,
  deployed, released) and blockers with `update_project_page` `status` (or
  `pyre project page status`); the server adds working sessions, sessions
  that need you, idle workers (not the lead), stuck sessions (working with no
  activity for 20 minutes, or failed) and open asks. The card is stale when
  sessions are working (stuck-but-working included, failed not) and the lead
  last updated the page or card more than 30 minutes ago; a project whose
  lead never wrote either is not flagged.
- **Standing go-aheads**: the project view lists the project's active
  standing grants (action → option, the owner's own words or "approved a
  proposal", note, expiry, uses), each with a Revoke button. A proposal ask
  ("Remember this go-ahead?" / "Add to the fact sheet?") is answered like any
  ask: Approve records it with the typed answer as the owner's words.
- **Live updates**: the server keeps one hub event subscription for all
  browsers and fans out Server-Sent Events; browsers refetch on each.

## API (JSON, under /api)

Errors: HTTP status and `{"error":{"code","message"}}` (message is plain
English). Mutations accept an optional `operation_id` so a retried request is
not applied twice.

| Method and path | Body | Response |
|---|---|---|
| `GET /api/me` | | `{email}` |
| `GET /api/inbox?box=open\|done[&project=<id>][&session=<id>]` | | `{threads:[Thread], held: HeldAnswers}`; `session` keeps that session and the sessions under it |
| `GET /api/threads/{thread_id}` | | `{thread: Thread, messages:[Msg]}` |
| `POST /api/threads/{thread_id}/reply` | `{text}` | `{message: Msg}` |
| `POST /api/threads/{thread_id}/read` | `{operation_id?}` | `{}` (acknowledges the thread's session messages, as archive does) |
| `POST /api/threads/{thread_id}/archive` | `{dismiss?: bool}` | `{thread: Thread}` (open ask without `dismiss`: 409 `conflict`; with it the ask is dismissed; a session item is marked handled) |
| `POST /api/inbox/archive` | `{thread_ids:[...]}` (at most 200) | `{results:[{thread_id, thread?, error?:{code,message}}]}` in request order |
| `POST /api/asks/{ask_id}/answer` | `{option_id?, items?: {item_id: option_id}, text?, mode: "now"\|"hold"}` (items: batched asks, per-item answers) | `{ask: Ask, held: HeldAnswers}` (hold on a hub without it: 501 `unsupported`) |
| `POST /api/held/{ask_id}/undo` | `{}` | `{held}` (already sent: 404) |
| `POST /api/held/send` | `{}` | `{results, held}` |
| `GET /api/projects` | | `{projects:[ProjectCard]}` |
| `GET /api/projects/{project_id}` | | `{project, page, sessions:[SessionCard], asks:[Ask], artifacts:[{id,title,url,updated_at}], status:StatusCard, grants:[StandingGrant]}` (grants: the project's active standing grants) |
| `POST /api/grants/{grant_id}/revoke` | `{reason?}` | `{grant: StandingGrant}` (the project page's Revoke) |
| `GET /api/artifacts/{artifact_id}/content` | | the latest version's bytes (sandboxed) |
| `GET /api/events` | | SSE: `inbox` `{}`, `held` HeldAnswers, `project` `{project_id}`, `thread` `{thread_id}`; `: ping` every 20s |

Types:

- `Thread {thread_id, kind:"ask"|"message"|"attention", title, preview, project:{id,name}|null, session:{id,title,state,node}|null, ask|null, held|null, last_at, unread, message_count, needs_reply, attention?: AttentionItem (kind "attention": the session item)}`
- `Msg {message_id, from:{kind:"owner"|"session",id,label}, text, at, reply_to, delivery:{state, summary}, ask_id?}`;
  delivery state is `stored|queued|delivered|read|replied|failed`; `ask_id`
  marks an ask's carrying message and its decision.
- `ProjectCard {project_id, name, description, open_asks, waiting, leads:[SessionCard], page_updated_at|null, page_summary, last_activity_at}`
- `SessionCard {id, title, state, node, is_lead, last_activity:{line,at}|null, watching?:[string]}` (watching: the checks of the live monitors that wake the session, shown as a "watching N" badge; monitor sessions themselves are never cards, nor counted in live sessions or the status buckets)
- Session `state`: `working`, `needs_you`, `idle`, `starting`, `ended`,
  `failed`, `unknown`, `archived`.

## Frontend

The app's source is `web/` (React, TypeScript, Vite, Tailwind, shadcn/ui);
`npm run build` there writes `internal/web/static/`, which is committed and
embedded, so building `pyre` needs no Node. See `web/README.md`. Opening the
app with `?fixtures=1` runs it against an in-browser fake API with synthetic
data (no hub calls), for design work and screenshots.

Routes: `#/inbox`, `#/done` (both take `?project=<id>` or `?session=<lead id>`,
shown as a removable filter chip), `#/thread/<thread_id>`, `#/projects`,
`#/projects/<project_id>`. Every route works on a fresh load, so other
surfaces (the TUI, notifications) can link straight into a thread.

Keyboard: `j`/`k` move, `↵` opens, `e` archives (the open thread, or the
highlighted row in the Open list), `Esc` goes back to the list, `g i` /
`g d` / `g p` switch views, `1`-`4` answer, `h` toggles the answer mode,
`u` undoes a held answer, `r` replies, `?` lists them.

Answer mode ("Send now" or "Hold & batch") is remembered per browser. Every
mutation carries a fresh `operation_id`; Retry after a failure reuses it.

## Troubleshooting

| Symptom | Check |
|---|---|
| 401 "Not signed in" | The request did not come through the tunnel (or the policy lost its add-headers rule). `journalctl --user -u pyre-web-tunnel`. |
| 403 after Google sign-in | The account is not @example.org. Sign out of Google for the ngrok domain and use the right account. |
| 403 "origin ... is not this app" on sending | The page was opened under another host name; add it with `--origin`. |
| 502 from ngrok | `pyre-web` is not running: `systemctl --user status pyre-web`, `journalctl --user -u pyre-web`. |
| "needs the owner's client credentials" at start | The unit or shell has `PYRE_SESSION_TOKEN`/`PYRE_TOKEN`; use the owner's `pyre login` config. |
| Inbox stale | The log says "hub event stream: ..." while the hub is unreachable; pages still refetch at most a minute old, and the server re-reads everything when the stream reconnects. |
| Hold answers fails with 501 | The hub predates held answers; upgrade the hub or send now. |
