# Pyre web app (frontend)

The single-page app served by `pyre web serve` (see `docs/web.md`). React +
TypeScript + Vite + Tailwind + shadcn/ui (Radix), TanStack Query, lucide
icons, react-markdown with rehype-sanitize. The build output is committed to
`internal/web/static/` and embedded with `go:embed`, so Go builds and
releases never need Node.

```sh
cd web
npm ci                 # exact versions from package-lock.json
npm run build          # typecheck, then write ../internal/web/static/
```

Commit `internal/web/static/` together with the source change. Keep npm's
cache off the main disk on server: `npm_config_cache=/home/user/fast/cache/npm`.

Development:

- `npm run dev` serves the app with hot reload and proxies `/api` to
  `pyre web serve --dev` on 127.0.0.1:7390.
- Open any URL with `?fixtures=1` to run against an in-browser fake API built
  from `public/fixtures/*.json` (synthetic data; timestamps shift to "now").
  `&fail=1` makes answers, replies and archiving fail; `&theme=dark|light` forces a
  theme. Regenerate the fixtures with
  `python3 scripts/gen-fixtures.py public/fixtures`.

Rules: no inline scripts or `style=""` markup (the server's CSP is
`script-src 'self'; style-src 'self'`); no runtime CDNs; all markdown from
sessions and leads goes through `Markdown` (no raw HTML).

Keyboard: `e` archives (the open thread, or the highlighted row in the
Open list); `Esc` goes back to the list. The help dialog (`?`) lists the rest.

Layout: `src/lib` (API client, types, fixtures backend, time formatting in
Pacific time, UI store), `src/hooks` (queries and actions, live events,
routing, keyboard), `src/components` (views; `ui/` is shadcn-generated).
