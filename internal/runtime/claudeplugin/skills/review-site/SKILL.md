---
name: review-site
description: Publish a page on the review site (pyre site) for people to read and leave notes on item by item, then read their notes back. Use on /review-site or when work needs a decision or feedback on several items (a proposal, options, a design or QA pass) from the owner or teammates.
argument-hint: "[page name]"
---

# /review-site — a page people review item by item

Page: $ARGUMENTS (default: a short kebab name for what you want reviewed).

**When.** Use it when several people (or the owner, later, on a phone) should read
something and comment on individual items: a proposal with decisions, options to
choose between, a design or QA pass with findings. One question needing one answer
is an ask, not a page; the project's status goes on its project page; a file only
another agent reads goes through `pyre publish`.

1. **Where.** Pages live on the machine that runs `pyre site serve`
   (`pyre site build` there lists them; the folder is `$PYRE_SITE_DIR`, default
   `~/.local/share/pyre/site/pages`). On another machine, ask your parent or the
   owner which machine serves the site and start the work there.
2. **Create.** `pyre site new <name> --title "..." --summary "one line for the
   index card"`. It writes `<name>/page.json` (your session ID as `session`, so
   every new note on the page comes to you as a Pyre message) and a starter
   `index.html`. The name is the URL and part of every note's key: never rename it.
3. **Write `index.html`** as a complete, self-contained page: inline CSS, no
   external scripts or fonts, images in `assets/` referenced relatively
   (`assets/shot.png`). The site adds its stylesheet first (tokens `var(--bg)`,
   `--panel`, `--fg`, `--mut`, `--line`, `--soft`, `--link`, `--ok`, `--warn`,
   `--bad`; classes `.wrap`, `.card`, `.card-head`, `.card-body`, `.card-title`,
   `.meta`, `.grid`/`.col`, `.lbl`, `.b b-high|b-medium|b-low|b-ok`, `.chip`,
   `.prose`), a top bar with a theme toggle, and the notes widget. Do not add your
   own nav. Use the tokens so the page works in light and dark, and check it at
   390px wide.
4. **Mark what people comment on** with a stable `data-note="<id>"` (letters,
   digits, `.`, `_`, `-`), one per item: `<article class="card"
   data-note="decision-1">`. Each gets a "Notes (n)" block. Keep IDs unique and
   stable across edits; notes stay on the ID they were left on. A page that
   renders items with JavaScript calls `PyreNotes.decorate(el)` afterwards.
5. **Status.** In `page.json` set `status` to `draft`, `ready` (for review),
   `decided` or `archived`, and bump `updated`. The index sorts by `order`, then
   newest first.
6. **Check.** `pyre site build` validates every page (page.json, index.html,
   duplicate or malformed data-note IDs) and exits non-zero on a problem. Pages
   are rendered on each request: a saved change is live at once, no restart.
7. **Share** the page's URL (`<site URL>/p/<name>/`) in your report or ask.
8. **Read notes.** New notes arrive as Pyre messages ("Review note on your page
   ..."). `pyre site notes <name>` (or `--json`, `--since 24h`) prints them all.
   Answer by updating the page (and its status), or by message to whoever asked;
   do not edit `notes.json` by hand.
