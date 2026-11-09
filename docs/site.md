# Review site: pages people read and leave notes on

`pyre site` is a small review site. An agent writes a page (one folder: a
self-contained `index.html`, a `page.json`, an `assets/` folder), and people
open it in a browser, read it, and leave notes on individual items. Every new
note is sent to the session that owns the page as a Pyre message, so the agent
sees it on its next turn. Notes are shared: everyone signed in sees everyone's
notes, with who wrote them and when.

```
browser ──https──> ngrok edge (Google sign-in, allowed domains only)
                     │ adds X-Pyre-Email / X-Pyre-Name
                     ▼
     ngrok agent ──> 127.0.0.1:7393  pyre site serve ──> page folders (read on each request)
                                            │            notes.json (state dir)
                                            └──> hub: new note → message to the page's session
```

## When to use it

| You want | Use |
|---|---|
| People to read a proposal, options, a design or a QA pass and comment item by item, on any device | **Review site** (`pyre site`) |
| One decision from the owner with 2–4 options | An ask (`ask_owner`) |
| The owner's view of a project's goal, status and results | The project page (`update_project_page`, [web app](web.md)) |
| A file another agent (or a later session) reads, versioned | `pyre publish` and `pyre comment` |
| A page shared through claude.ai | A Claude Artifact |

The review site is the right tool when the readers are people (several of
them, perhaps from more than one company domain), when the comments belong to
specific items on the page, and when the agent that wrote the page should hear
about each comment.

## Quick start (local)

```sh
pyre site new launch-plan --title "Launch plan" --summary "Pick a launch date and channel"
$EDITOR ~/.local/share/pyre/site/pages/launch-plan/index.html
pyre site serve --dev            # http://127.0.0.1:7393, no sign-in, loopback only
```

`--dev` admits loopback requests without a sign-in header. Never combine it
with the tunnel: ngrok's agent connects from loopback.

## Install and run behind ngrok

Prerequisites: `pyre` installed (`~/.local/bin/pyre`), `pyre login` done for
the owner (`~/.config/pyre/client.json`; used to send notes to sessions),
`ngrok` at `/usr/local/bin/ngrok` with its authtoken configured (`ngrok config
add-authtoken ...`, stored in ngrok's own config) and a reserved ngrok domain
(the units use the placeholder `pyre-example-review.ngrok.app`).

1. Edit the three files for your domains and URL:
   `packaging/systemd/pyre-site.service` (`--allow-domain`, `--origin`),
   `packaging/ngrok/pyre-site-policy.yml` (the `endsWith('@...')` expression)
   and `packaging/systemd/pyre-site-tunnel.service` (`--url`).
2. Install and start:

```sh
install -m 0644 packaging/systemd/pyre-site.service ~/.config/systemd/user/
install -d -m 0700 ~/.config/pyre/ngrok
install -m 0644 packaging/ngrok/pyre-site-policy.yml ~/.config/pyre/ngrok/
install -m 0644 packaging/systemd/pyre-site-tunnel.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now pyre-site pyre-site-tunnel
loginctl enable-linger "$USER"   # keep both running after logout
```

Upgrade: install the new `pyre`, then `systemctl --user restart pyre-site`
(the stylesheet and notes widget are embedded in the binary). Pages and notes
are plain files and survive upgrades and rollbacks. Remove: `systemctl --user
disable --now pyre-site-tunnel pyre-site` and delete the unit files; the pages
and `notes.json` stay until you delete them.

### Commands and flags

| Command | What it does |
|---|---|
| `pyre site new <name> [--title] [--summary] [--owner] [--session ID \| --no-session]` | Creates `<dir>/<name>/` with `page.json`, a starter `index.html` and `assets/`. The session defaults to `$PYRE_SESSION_ID`. |
| `pyre site build [--out DIR]` | Checks every page; exits non-zero and names each problem. `--out` also writes a static copy (no notes). |
| `pyre site serve` | Serves the site (flags below). |
| `pyre site notes [name] [--since 24h]` | Prints notes, oldest first (`--json` for the records). |

Every command takes `--dir` (the page folder; default `$PYRE_SITE_DIR`, else
`$XDG_DATA_HOME/pyre/site/pages`, else `~/.local/share/pyre/site/pages`).

| `serve` flag | Default | Meaning |
|---|---|---|
| `--listen` | `127.0.0.1:7393` | Loopback address; anything else is refused. |
| `--allow-domain` | none | Email domain allowed in (repeatable; exact, not subdomains). |
| `--allow-email` | none | One email address allowed in (repeatable). |
| `--origin` | none | Public origin(s) accepted on writes besides the request's own host; the first is the base of links in delivered notes. |
| `--state-dir` | `$XDG_STATE_HOME/pyre/site`, else `~/.local/state/pyre/site` | `notes.json` (0600). |
| `--title` | `Review` | The site's name in its header. |
| `--no-deliver` | off | Keep notes but never send them to sessions. |
| `--dev` | off | Admit loopback requests without `X-Pyre-Email` (local testing). |

At least one `--allow-domain` or `--allow-email` is required unless `--dev`.
The hub connection uses the usual flags (`--hub`, `--config`, ...); without
one the site still runs and keeps notes, and says at start that it will not
send them.

## How auth works

1. ngrok's traffic policy (`packaging/ngrok/pyre-site-policy.yml`) removes
   any `X-Pyre-Email`/`X-Pyre-Name` the client sent, requires Google sign-in,
   answers 403 unless the email is in an allowed domain, and adds
   `X-Pyre-Email` and `X-Pyre-Name` from the sign-in.
2. The server listens on loopback only and rejects every request (pages,
   assets and API) without that header (401), with more than one, or with an
   email that is not allowed (403). The check is exact: `a@sub.example.org`
   is not `example.org`.
3. Writes (`POST`, `PATCH`, `DELETE`) must carry an `Origin` equal to the
   request's host or an `--origin`, must not be `Sec-Fetch-Site: cross-site`,
   and (except `DELETE`) must be `Content-Type: application/json`.
4. Every response has a Content-Security-Policy (scripts and styles from the
   site itself or inline in the page, images also from https, no framing, no
   plugins), `X-Frame-Options: DENY`, `nosniff` and `no-referrer`; API
   responses are `no-store`.

Pages run on the site's own origin, so a page's script could call the notes
API as the viewer: only publish pages written by your own agents. A process on
the serving machine can reach the loopback port and set the header; the trust
boundary is the machine.

## Writing a page

A page is a folder named by its slug (lowercase letters, digits, hyphens). The
slug is the URL (`/p/<slug>/`) and part of every note's key, so never rename
it.

`page.json`:

```json
{
  "title": "Launch plan",
  "summary": "One line for the index card.",
  "status": "ready",
  "updated": "2026-11-09T10:40:00-08:00",
  "owner": "Launch lead",
  "session": "ses_..."
}
```

- `status`: `draft`, `ready` (shown "Ready for review"), `decided` or
  `archived`; another short word is shown as-is. `archived` pages (or
  `"archived": true`) sit in a collapsed section of the index.
- `updated`: RFC 3339 or `YYYY-MM-DD` (a date is Pacific); default: the
  file's modification time. Shown in Pacific time.
- `order` (optional): lower sorts first; the rest sort newest first.
- `session`: the session that receives each new note (set by `pyre site new`).

`index.html` is a complete, self-contained page: inline CSS, no external
scripts or fonts, images and other files in `assets/` referenced relatively.
When it is served the site adds, in this order:

- in `<head>`: `<meta name="pyre-site-page">` and the shared stylesheet
  (`/_site/site.css`) before the page's own styles, so the page's styles win;
- after `<body>`: a top bar (back to the index, the page title and status, a
  Theme button);
- before `</body>`: the notes widget (`/_site/notes.js`).

The stylesheet has light and dark tokens (`--bg`, `--panel`, `--fg`, `--mut`,
`--line`, `--soft`, `--link`, `--acc`, `--ok`/`--okb`, `--warn`/`--warnb`,
`--bad`/`--badb`) following `prefers-color-scheme` and the Theme button
(`data-theme` on `<html>`), and a few classes: `.wrap`, `.card`, `.card-head`,
`.card-body`, `.card-title`, `.meta`, `.grid`/`.col`, `.lbl`, `.b` with
`b-high|b-medium|b-low|b-ok`, `.chip`, `.pst`, `.prose`. Use the tokens, and
check the page at 390px wide in both themes.

**Notes on items.** Give each element people should comment on a stable
`data-note="<id>"` (letters, digits, `.`, `_`, `-`, at most 80; unique in the
page), for example `<article class="card" data-note="decision-1">`. The widget
adds a collapsed "Notes (n)" block to it and a note count to its `.meta` row.
Keep IDs stable across edits: a note stays on the ID it was left on. A page
that renders items with JavaScript calls `PyreNotes.decorate(element)` after
rendering. Each index card takes notes on the page as a whole (item `_page`).

**Changes are live.** The server reads the folder on every request; there is
nothing to rebuild or restart. `pyre site build` is the check before you share
the link.

## Notes and delivery

- Anyone signed in can add a note (at most 4000 characters) and edit or delete
  their own. Notes are kept in `notes.json` in the state directory, rewritten
  atomically on every change.
- A new note on a page whose `page.json` names a `session` is sent to that
  session as a Pyre message, from the owner's credential, after the note is
  saved:

  ```
  Review note on your page "Launch plan" (launch-plan, item decision-1) from Alice <alice@example.org>, Nov 9, 2026, 10:42 AM PT:

  Prefer the Tuesday date.

  Page: https://pyre-example-review.ngrok.app/p/launch-plan/
  All notes on it: pyre site notes launch-plan
  ```

  The result is recorded on the note (`delivery.message_id`, or
  `delivery.error` if the hub refused or was unreachable; the note is kept
  either way), and the widget shows "sent to the page's agent". Edits and
  deletions are not sent.
- The agent reads every note with `pyre site notes <name>` and answers by
  updating the page and its status (or by message to whoever asked).

## Notes API (JSON)

| Method and path | Body | Response |
|---|---|---|
| `GET /api/me` | | `{email}` |
| `GET /api/notes[?page=<slug>]` | | `{me, notes:[Note]}` oldest first |
| `POST /api/notes` | `{page, item, text}` | `201 Note` (with `delivery` when the page names a session) |
| `PATCH /api/notes/{id}` | `{text}` | `Note` (author only, else 403) |
| `DELETE /api/notes/{id}` | | `{ok: true}` (author only) |

`Note {id, page, item, author, name, text, created, edited?, delivery?: {session, message_id?, error?, at}, mine}`.
Errors are `{"error": "<plain English>"}` with the HTTP status.

## Troubleshooting

| Symptom | Check |
|---|---|
| 401 "Not signed in" | The request did not come through the tunnel (or the policy lost its add-headers rule). `journalctl --user -u pyre-site-tunnel`. |
| 403 after Google sign-in | The account's domain is not in the policy or in `--allow-domain`/`--allow-email`; keep the two lists in step. |
| 403 "origin ... is not this site" on adding a note | The site was opened under another host name; add it with `--origin`. |
| 502 from ngrok | `pyre-site` is not running: `systemctl --user status pyre-site`, `journalctl --user -u pyre-site`. |
| A page is missing from the index | `pyre site build`: its `page.json` is missing a title or does not parse. |
| Notes show "not sent to the agent" | The note's `delivery.error` (`pyre site notes --json`) says why; check the unit can reach the hub (`pyre login` config) and the session ID in `page.json`. |
