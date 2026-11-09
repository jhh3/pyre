// pyre site notes widget. Every element with data-note="<item>" gets a
// collapsed "Notes (n)" block; the page is named by <meta name="pyre-site-page">
// (index cards carry data-note-page). Call PyreNotes.decorate(root) after
// rendering new items with JavaScript. Also wires the Theme button.
(function () {
  "use strict";
  const MAX = 4000;
  const esc = s => String(s == null ? "" : s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const meta = document.querySelector('meta[name="pyre-site-page"]');
  const pageOf = el => el.getAttribute("data-note-page") || (meta && meta.content) || "";
  const keyOf = el => pageOf(el) + "/" + el.getAttribute("data-note");
  const base = meta ? "../../" : "";
  let notes = {}, me = "", ready = false, failed = false;
  const open = new Set(), drafts = {};

  const when = iso => new Date(iso).toLocaleString(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit", timeZone: "America/Los_Angeles" }) + " PT";

  async function api(method, path, body) {
    const r = await fetch(base + path, { method, credentials: "same-origin", headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : undefined });
    let d = {};
    try { d = await r.json(); } catch (_) { /* not JSON */ }
    if (!r.ok) throw new Error(d.error || "Couldn't save (HTTP " + r.status + ").");
    return d;
  }
  function put(n) {
    const k = n.page + "/" + n.item;
    notes[k] = (notes[k] || []).filter(x => x.id !== n.id).concat([n]).sort((a, b) => a.created.localeCompare(b.created));
  }
  function noteHTML(n) {
    const sent = n.delivery ? (n.delivery.message_id ? " · sent to the page's agent" : " · not sent to the agent") : "";
    return '<li class="note" data-id="' + esc(n.id) + '"><div class="note-h"><b>' + esc(n.name || n.author.split("@")[0]) + "</b><span>" +
      esc(when(n.created)) + (n.edited ? " · edited" : "") + esc(sent) + "</span>" +
      (n.mine ? '<span class="note-act"><button type="button" class="lnk" data-act="edit">Edit</button><button type="button" class="lnk" data-act="del">Delete</button></span>' : "") +
      '</div><div class="note-t">' + esc(n.text) + "</div></li>";
  }
  function bodyHTML(key, list) {
    if (failed) return '<p class="note-st err">Notes couldn\'t be loaded. Reload to try again.</p>';
    if (!ready) return '<p class="note-empty">Loading…</p>';
    return '<ul class="note-list">' + (list.map(noteHTML).join("") || '<li class="note-empty">No notes yet.</li>') + "</ul>" +
      (me ? '<form class="note-form"><textarea maxlength="' + MAX + '" rows="2" placeholder="Add a note for everyone…" aria-label="New note">' + esc(drafts[key] || "") +
        '</textarea><div class="note-row"><span class="note-st" role="status" aria-live="polite"></span><button class="chip note-add" type="submit">Add note</button></div></form>'
        : '<p class="note-empty">Sign in to add notes.</p>');
  }
  function badge(el, c) {
    const m = el.querySelector(".meta");
    if (!m) return;
    let b = m.querySelector(".note-badge");
    if (!c) { if (b) b.remove(); return; }
    if (!b) { b = document.createElement("span"); b.className = "b note-badge"; m.appendChild(b); }
    b.textContent = c + " note" + (c > 1 ? "s" : "");
  }
  function decorate(root) {
    (root || document).querySelectorAll("[data-note]").forEach(el => {
      const key = keyOf(el);
      let d = Array.from(el.children).find(c => c.matches("details.notes"));
      if (!d) {
        d = document.createElement("details");
        d.className = "notes";
        d.innerHTML = '<summary></summary><div class="notes-b"></div>';
        el.appendChild(d);
        d.addEventListener("toggle", () => {
          if (d.open) { open.add(key); const t = d.querySelector("textarea"); if (t) t.focus({ preventScroll: true }); } else open.delete(key);
        });
      }
      d.open = open.has(key);
      const list = notes[key] || [];
      d.querySelector("summary").textContent = ready && list.length ? "Notes (" + list.length + ")" : "Notes";
      d.querySelector(".notes-b").innerHTML = bodyHTML(key, list);
      badge(el, list.length);
    });
  }
  const refresh = key => document.querySelectorAll("[data-note]").forEach(el => { if (keyOf(el) === key) decorate(el.parentElement || document); });
  window.PyreNotes = { decorate, count: key => (notes[key] || []).length };

  document.addEventListener("input", e => {
    const ta = e.target.closest && e.target.closest(".notes textarea");
    if (!ta) return;
    const el = ta.closest("[data-note]");
    if (ta.closest(".note-form")) drafts[keyOf(el)] = ta.value;
    const st = ta.closest("form").querySelector(".note-st");
    st.className = "note-st";
    st.textContent = ta.value.length > MAX - 200 ? (MAX - ta.value.length) + " characters left" : "";
  });
  document.addEventListener("keydown", e => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && e.target.closest && e.target.closest(".notes textarea")) { e.preventDefault(); e.target.closest("form").requestSubmit(); }
  });
  document.addEventListener("submit", async e => {
    const f = e.target.closest && e.target.closest(".note-form, .note-edit");
    if (!f) return;
    e.preventDefault();
    const el = f.closest("[data-note]"), key = keyOf(el), ta = f.querySelector("textarea"), st = f.querySelector(".note-st"), btn = f.querySelector("button[type=submit]");
    const text = ta.value.trim();
    if (!text) { st.className = "note-st err"; st.textContent = "Write something first."; return; }
    btn.disabled = true; st.className = "note-st"; st.textContent = "Saving…";
    try {
      if (f.classList.contains("note-edit")) put(await api("PATCH", "api/notes/" + encodeURIComponent(f.dataset.id), { text }));
      else { put(await api("POST", "api/notes", { page: pageOf(el), item: el.getAttribute("data-note"), text })); delete drafts[key]; }
      refresh(key);
      const s = el.querySelector(".note-form .note-st");
      if (s) { s.className = "note-st ok"; s.textContent = "Saved"; setTimeout(() => { if (s.textContent === "Saved") s.textContent = ""; }, 2500); }
    } catch (err) {
      btn.disabled = false; st.className = "note-st err"; st.textContent = err.message;
    }
  });
  document.addEventListener("click", async e => {
    const t = e.target.closest && e.target.closest(".ps-theme");
    if (t) {
      const cur = document.documentElement.dataset.theme;
      const dark = cur ? cur === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
      document.documentElement.dataset.theme = dark ? "light" : "dark";
      try { localStorage.setItem("pyre-site-theme", document.documentElement.dataset.theme); } catch (_) { /* private mode */ }
      return;
    }
    const b = e.target.closest && e.target.closest(".note [data-act], .note-edit [data-act]");
    if (!b) return;
    const li = b.closest(".note"), el = b.closest("[data-note]"), key = keyOf(el), nid = li.dataset.id;
    const n = (notes[key] || []).find(x => x.id === nid);
    if (b.dataset.act === "edit" && n) {
      li.innerHTML = '<form class="note-edit" data-id="' + esc(nid) + '"><textarea maxlength="' + MAX + '" rows="3" aria-label="Edit note">' + esc(n.text) +
        '</textarea><div class="note-row"><span class="note-st" role="status" aria-live="polite"></span><button type="button" class="chip" data-act="cancel">Cancel</button><button class="chip note-add" type="submit">Save</button></div></form>';
      li.querySelector("textarea").focus();
    } else if (b.dataset.act === "cancel") {
      refresh(key);
    } else if (b.dataset.act === "del") {
      if (!confirm("Delete this note?")) return;
      b.disabled = true;
      try {
        await api("DELETE", "api/notes/" + encodeURIComponent(nid));
        notes[key] = (notes[key] || []).filter(x => x.id !== nid);
        refresh(key);
      } catch (err) { b.disabled = false; alert(err.message); }
    }
  });

  try { const t = localStorage.getItem("pyre-site-theme"); if (t) document.documentElement.dataset.theme = t; } catch (_) { /* private mode */ }
  decorate(document);
  const q = meta ? "?page=" + encodeURIComponent(meta.content) : "";
  fetch(base + "api/notes" + q, { credentials: "same-origin" }).then(r => { if (!r.ok) throw new Error(String(r.status)); return r.json(); }).then(d => {
    notes = {}; me = d.me || "";
    (d.notes || []).forEach(put);
    ready = true;
    decorate(document);
  }).catch(() => { failed = true; decorate(document); });
})();
