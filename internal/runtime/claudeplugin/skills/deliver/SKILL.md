---
name: deliver
description: Copy finished outputs to ~/Downloads/<project>/ (or the NAS when named), open them, and say what is where in one table. Use when the owner types /deliver or asks for the results on his machine.
argument-hint: "[what]"
---

# /deliver — put the outputs where the owner can open them

What: $ARGUMENTS (default: every finished output of this work — reports, HTML, videos,
screenshots, builds).

1. **Destination.** `~/Downloads/<project>/` (project name from `get_context`, else a short
   kebab name for the work). Use the NAS path only when the owner names it. If the
   owner is on another machine, `pyre publish` each file and give the ver_ IDs instead.
2. **Copy, don't move.** Keep the originals. Reports are self-contained HTML (inline CSS
   and images) so they open anywhere.
3. **Bundle.** Several HTML files and videos: also zip them together as
   `~/Downloads/<project>/<project>.zip`.
4. **Open** the main item — app, video or HTML — with `xdg-open` (Linux) or `open`
   (macOS).
5. **Report one table**: item, what it is, absolute path. Nothing else unless something
   failed to copy or open.
