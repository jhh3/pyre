#!/usr/bin/env bash
# jhh3-mem — lib/embed.sh
#
# Optional semantic search. Everything here is additive: with no key, no network, or
# no extension, `search` returns exactly the lexical results it returns today. That
# is not a fallback bolted on afterwards -- it is the design (SPEC 5.6), because a
# retrieval path that can fail is only safe if failing costs nothing.
#
# NO PYTHON, NO MODEL ON DISK. Query and row vectors come from the embeddings API via
# curl; the vector index is sqlite-vec, a 160 KB loadable extension driven by the
# sqlite3 CLI. That keeps the whole feature inside the plugin's existing idiom --
# bash, sqlite3, coreutils, curl -- rather than dragging in a runtime.
#
# WHY AN API AND NOT A LOCAL MODEL. Measured, not assumed. On four probe queries
# against real rows, static local embeddings (model2vec potion-base-8M/32M) scored
# 3/4 and 2/4; text-embedding-3-small scored 4/4. The misses were exactly the queries
# semantic search exists for -- "payments broken" finding a Stripe row requires world
# knowledge a token-lookup table does not carry. A local model that fails those is
# 134 MB spent to not solve the problem.
#
# WHY THIS FILE MAY TOUCH THE NETWORK. INV-6 permits a network client only in named
# files. What it protects is "hooks never touch the network, and no credential is
# ever stored" -- both still hold: SessionStart ranks by recency and scope and never
# embeds anything, and the key is read from the environment and never written to
# disk. See SPEC 20.

JM_VEC_VERSION='0.1.9'
# 30s, not 8s. A full batch of 96 inputs is a ~200KB request whose response is a
# few megabytes of floats; 8 seconds lost those batches to the clock often enough
# that a whole corpus could never finish. This bounds a hung socket, it is not a
# performance target -- a healthy batch returns in about two seconds.
JM_EMBED_TIMEOUT="${JHH3_MEM_EMBED_TIMEOUT:-30}"  # seconds, per API call
JM_EMBED_BATCH="${JHH3_MEM_EMBED_BATCH:-96}"      # inputs per call

# Pinned checksums, one per supported platform. A fetched binary that does not match
# is discarded: this file is loaded into the same process as the user's memory.
_jm_vec_sha() {
  case "${1:-}" in
    linux-x86_64)   printf 'b959baa1d8dc88861b1edb337b8587178cdcb12d60b4998f9d10b6a82052d5d7\n' ;;
    linux-aarch64)  printf 'ea03d39541e478fab5974253c461e1cb5d77742f69e40cf96e3fad5bc309a37c\n' ;;
    macos-x86_64)   printf '53ad76e400786515e2edcaed2f01271dda846316390b761fadbd2dcf56aa4713\n' ;;
    macos-aarch64)  printf '8282126333399ddfe98bbbcc7a1936e7252625aac49df056a98be602e46bfd29\n' ;;
    *) return 1 ;;
  esac
}

# _jm_vec_platform — echo the release slug for this host, or fail.
_jm_vec_platform() {
  local os arch
  case "$(uname -s)" in Linux) os=linux ;; Darwin) os=macos ;; *) return 1 ;; esac
  case "$(uname -m)" in x86_64|amd64) arch=x86_64 ;; arm64|aarch64) arch=aarch64 ;; *) return 1 ;; esac
  printf '%s-%s\n' "$os" "$arch"
}

# jm_embed_have_key / jm_embed_have_ext / jm_embed_ready — the three preconditions.
jm_embed_have_key() { [ -n "${OPENAI_API_KEY:-}" ]; }
jm_embed_have_ext() { [ -r "$JM_VEC_SO.so" ]; }
jm_embed_ready()    { jm_embed_have_ext && jm_embed_have_key; }

# jm_embed_fetch_extension — download and verify sqlite-vec into the store.
# rc 0 ok (or already present) · 1 failed · 2 unsupported platform
jm_embed_fetch_extension() {
  jm_embed_have_ext && return 0
  local plat want url stage
  plat="$(_jm_vec_platform)" || { printf 'jhh3-mem: no sqlite-vec build for %s/%s\n' "$(uname -s)" "$(uname -m)" >&2; return 2; }
  want="$(_jm_vec_sha "$plat")" || return 2
  jm_have curl || { printf 'jhh3-mem: curl is required to fetch the extension\n' >&2; return 1; }

  url="https://github.com/asg017/sqlite-vec/releases/download/v${JM_VEC_VERSION}/sqlite-vec-${JM_VEC_VERSION}-loadable-${plat}.tar.gz"
  stage="$(jm_mkstage)" || return 1

  curl -sSL --max-time 120 -o "$stage/v.tar.gz" "$url" 2>/dev/null || { rm -rf -- "$stage"; return 1; }
  local got
  got="$(jm_sha256_file "$stage/v.tar.gz")" || { rm -rf -- "$stage"; return 1; }
  if [ "$got" != "$want" ]; then
    printf 'jhh3-mem: checksum mismatch for sqlite-vec (%s)\n' "$plat" >&2
    printf '          expected %s\n          got      %s\n' "$want" "$got" >&2
    rm -rf -- "$stage"; return 1
  fi
  tar -xzf "$stage/v.tar.gz" -C "$stage" 2>/dev/null || { rm -rf -- "$stage"; return 1; }
  mkdir -p -- "$JM_VEC_DIR" 2>/dev/null || { rm -rf -- "$stage"; return 1; }
  local so; so="$(find "$stage" -name 'vec0.*' -type f 2>/dev/null | head -1)"
  [ -n "$so" ] || { rm -rf -- "$stage"; return 1; }
  cp -- "$so" "$JM_VEC_SO.so" 2>/dev/null || { rm -rf -- "$stage"; return 1; }
  chmod 0644 "$JM_VEC_SO.so" 2>/dev/null || true
  rm -rf -- "$stage"
  jm_log embed ok "extension=v$JM_VEC_VERSION platform=$plat"
  return 0
}

# _jm_embed_api INPUT_JSON_FILE OUT_FILE — one embeddings call. rc 0 ok.
_jm_embed_api() {
  local in="${1:-}" out="${2:-}"
  curl -sS --max-time "$JM_EMBED_TIMEOUT" https://api.openai.com/v1/embeddings \
    -H "Authorization: Bearer $OPENAI_API_KEY" \
    -H 'Content-Type: application/json' \
    --data-binary "@$in" >"$out" 2>/dev/null || return 1
  # An error body is valid JSON too; require the vector to actually be there.
  jm_db_read_mem "SELECT count(*) FROM json_each(readfile('$(jm_sql_quote "$out" | sed "s/^'//;s/'\$//")'), '\$.data[0].embedding');" \
    2>/dev/null | grep -qE '^[1-9]' || return 1
  return 0
}

# _jm_embed_api_error RESP_FILE — the API's own error message, or nothing.
# Worth the extra call: "api error or timeout" covers a dead network, an expired
# key and a malformed body, and those want three different responses from whoever
# is reading the output.
_jm_embed_api_error() {
  [ -s "${1:-}" ] || return 0
  jm_db_read_mem "SELECT substr(coalesce(json_extract(readfile('$(jm_sql_quote "$1" | sed "s/^'//;s/'\$//")'), '\$.error.message'),''),1,120);" 2>/dev/null
}

# _jm_embed_write STAGE IDS_FILE RESP_FILE — parse one response and store its
# vectors. Shared by the batch path and the one-row retry so they cannot drift.
_jm_embed_write() {
  local stage="${1:-}" idsf="${2:-}" respf="${3:-}"
  {
    printf 'CREATE TEMP TABLE _ids(pos INTEGER PRIMARY KEY, id INTEGER);\n'
    local i=0 id
    while IFS= read -r id; do
      printf 'INSERT INTO _ids(pos,id) VALUES(%s,%s);\n' "$i" "$id"
      i=$((i + 1))
    done < "$idsf"
    printf 'CREATE TEMP TABLE _emb AS SELECT key AS pos, value AS obj FROM json_each(readfile(%s), %s);\n' \
      "'$respf'" "'\$.data'"
    printf 'BEGIN IMMEDIATE;\n'
    printf 'INSERT OR REPLACE INTO entry_vec(id,e)\n'
    printf '  SELECT i.id, vec_f32(json_extract(e.obj, %s)) FROM _ids i JOIN _emb e ON e.pos = i.pos;\n' \
      "'\$.embedding'"
    printf 'COMMIT;\n'
  } > "$stage/ins.sql"
  jm_vec_write "$(cat "$stage/ins.sql")"
}

# _jm_embed_singly STAGE — re-send the current slice one row at a time; echo how
# many stored. The batch API is all-or-nothing, so without this ONE unembeddable
# row costs its whole batch on every future run: the same rows are pending, so they
# are grouped the same way and fail the same way, permanently.
_jm_embed_singly() {
  local stage="${1:-}" ok=0 line id txt
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    id="${line%%$'\x1f'*}"; txt="${line#*$'\x1f'}"
    printf '{"model":"%s","dimensions":%s,"input":[' "$JM_EMBED_MODEL" "$JM_EMBED_DIM" > "$stage/one.json"
    jm_json_string "$txt" >> "$stage/one.json"
    printf ']}\n' >> "$stage/one.json"
    _jm_embed_api "$stage/one.json" "$stage/one-resp.json" || continue
    printf '%s\n' "$id" > "$stage/one-ids.txt"
    _jm_embed_write "$stage" "$stage/one-ids.txt" "$stage/one-resp.json" || continue
    ok=$((ok + 1))
  done < "$stage/slice.txt"
  printf '%s\n' "$ok"
}

# jm_embed_query TEXT — echo the query vector as a CSV of floats. rc 0 ok · 1 no vector.
#
# Failure here is ordinary and must stay cheap: the caller drops to lexical-only.
jm_embed_query() {
  jm_embed_ready || return 1
  local text="${1:-}" stage rc=0
  [ -n "$text" ] || return 1
  stage="$(jm_mkstage)" || return 1
  jm_json_string "$text" > "$stage/t.json" || { rm -rf -- "$stage"; return 1; }
  printf '{"model":"%s","input":[%s],"dimensions":%s}\n' \
    "$JM_EMBED_MODEL" "$(cat "$stage/t.json")" "$JM_EMBED_DIM" > "$stage/req.json"
  if _jm_embed_api "$stage/req.json" "$stage/resp.json"; then
    jm_db_read_mem "SELECT group_concat(value) FROM json_each(readfile('$(jm_sql_quote "$stage/resp.json" | sed "s/^'//;s/'\$//")'), '\$.data[0].embedding');"
  else
    rc=1
  fi
  rm -rf -- "$stage"
  return "$rc"
}

# ── store access with the extension loaded ──────────────────────────────────
#
# A separate path from jm_db_read/jm_db_write on purpose: those must keep working
# with no extension present, which is the whole no-regression guarantee. Only code
# that genuinely needs vec0 goes through here.

# jm_vec_read SQL — read with vec0 loaded. rc 0 ok · 1 failed.
jm_vec_read() {
  jm_embed_have_ext || return 1
  printf '%s\n' "${1:-}" | sqlite3 -bail -batch -noheader -readonly \
    -cmd ".timeout $JM_DB_BUSY_READ_MS" -cmd ".load $JM_VEC_SO" \
    "$(_jm_db_uri 'mode=ro')" 2>/dev/null
}

# jm_vec_write SQL — write with vec0 loaded. rc 0 ok · non-zero from sqlite3.
jm_vec_write() {
  jm_embed_have_ext || return 1
  printf '%s\n' "${1:-}" | sqlite3 -bail -batch \
    -cmd ".timeout $JM_DB_BUSY_WRITE_MS" -cmd ".load $JM_VEC_SO" \
    -cmd 'PRAGMA foreign_keys=ON;' -- "$JM_DB" >/dev/null 2>&1
}

# jm_embed_ensure_table — create entry_vec on first use.
#
# Created lazily rather than in schema.sql: bootstrap must succeed on a machine that
# has never fetched the extension, and a virtual table whose module is missing makes
# the whole database unopenable -- including for lexical search, which would turn an
# optional feature into a total outage.
jm_embed_ensure_table() {
  jm_embed_have_ext || return 1
  jm_vec_write "CREATE VIRTUAL TABLE IF NOT EXISTS entry_vec USING vec0(
     id INTEGER PRIMARY KEY, e float[$JM_EMBED_DIM] distance_metric=cosine);"
}

# jm_embed_pending — how many live rows still lack a vector.
jm_embed_pending() {
  jm_vec_read "SELECT count(*) FROM entries e
               WHERE e.superseded = 0
                 AND NOT EXISTS (SELECT 1 FROM entry_vec v WHERE v.id = e.id);" 2>/dev/null
}

# jm_embed_rows LIMIT DRY_RUN — embed rows that lack a vector, newest first.
# stdout: progress. rc 0 ok · 1 partial · 3 not ready.
jm_embed_rows() {
  local limit="${1:-500}" dry="${2:-0}"
  case "$limit" in '' | *[!0-9]*) return 2 ;; esac
  jm_embed_have_ext || { printf 'jhh3-mem: sqlite-vec is not installed — run `jhh3-mem embed --install`\n' >&2; return 3; }
  jm_embed_have_key || { printf 'jhh3-mem: OPENAI_API_KEY is not set; semantic search stays off\n' >&2; return 3; }
  jm_embed_ensure_table || return 1

  local stage; stage="$(jm_mkstage)" || return 1
  # shellcheck disable=SC2064
  trap "rm -rf -- '$stage' 2>/dev/null" RETURN

  # The embedded text mirrors what BM25 indexes, minus the columns that are paths or
  # command names: those tokenise usefully for lexical search and only add noise to a
  # sentence embedding.
  jm_vec_read "
    SELECT e.id || char(31) || substr(replace(replace(
             e.title || '. ' || coalesce(e.outcome,'') || ' ' || coalesce(e.learned,''),
             char(10),' '), char(9),' '), 1, 2000)
    FROM entries e
    WHERE e.superseded = 0
      AND NOT EXISTS (SELECT 1 FROM entry_vec v WHERE v.id = e.id)
    ORDER BY e.ts DESC LIMIT $limit;" > "$stage/rows.txt" || return 1

  local total; total="$(grep -c . "$stage/rows.txt" 2>/dev/null)" || total=0
  [ "${total:-0}" -gt 0 ] || { printf 'nothing to embed\n'; return 0; }
  if [ "$dry" = '1' ]; then printf 'would embed %s row(s)\n' "$total"; return 0; fi

  local done=0 failed=0
  while [ "$done" -lt "$total" ]; do
    : > "$stage/ids.txt"; : > "$stage/inputs.json"; : > "$stage/slice.txt"
    local n=0 first=1
    printf '{"model":"%s","dimensions":%s,"input":[' "$JM_EMBED_MODEL" "$JM_EMBED_DIM" > "$stage/req.json"
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      printf '%s\n' "${line%%$'\x1f'*}" >> "$stage/ids.txt"
      printf '%s\n' "$line" >> "$stage/slice.txt"
      [ "$first" = 1 ] || printf ',' >> "$stage/req.json"
      first=0
      # Truncation happens in SQL (substr counts CHARACTERS) rather than here.
      # `cut -c` counts bytes on this input, so it split a multi-byte character --
      # a single em-dash landing on the 2000th byte produced invalid UTF-8, which
      # the API rejects for the WHOLE request. That failed 96 good rows for one
      # bad one, every run, forever, and reported it as a network error.
      jm_json_string "${line#*$'\x1f'}" >> "$stage/req.json"
      n=$((n + 1))
      [ "$n" -ge "$JM_EMBED_BATCH" ] && break
    done < <(tail -n +$((done + 1)) "$stage/rows.txt")
    printf ']}\n' >> "$stage/req.json"
    [ "$n" -gt 0 ] || break

    if ! _jm_embed_api "$stage/req.json" "$stage/resp.json"; then
      local why; why="$(_jm_embed_api_error "$stage/resp.json")"
      printf '  FAIL  batch of %s (%s)\n' "$n" "${why:-no response, timeout, or network error}"
      if [ "$n" -gt 1 ]; then
        printf '        retrying row by row so one bad row cannot hold up the rest\n'
        local okn; okn="$(_jm_embed_singly "$stage")"
        [ "${okn:-0}" -gt 0 ] && printf '        recovered %s of %s\n' "$okn" "$n"
        failed=$((failed + n - ${okn:-0})); done=$((done + n))
      else
        failed=$((failed + n)); done=$((done + n))
      fi
      continue
    fi

    # Parse the response ONCE. The first version emitted one INSERT per row, each
    # with its own readfile() of the same multi-megabyte body -- 96 re-reads per
    # batch, which was slow enough to lose the write lock and fail whole batches
    # intermittently. Load it into a temp table, then join positionally: the API
    # guarantees data[] comes back in input order, and json_each preserves it.
    if _jm_embed_write "$stage" "$stage/ids.txt" "$stage/resp.json"; then
      done=$((done + n))
      printf '  ok    %s/%s\n' "$done" "$total"
    else
      failed=$((failed + n)); done=$((done + n))
      printf '  FAIL  batch write rejected\n'
    fi
  done

  jm_log embed ok "rows=$total failed=$failed dim=$JM_EMBED_DIM"
  printf 'embedded %s of %s row(s)\n' "$((total - failed))" "$total"
  [ "$failed" -gt 0 ] && return 1
  return 0
}

# jm_embed_rrf QUERY FTS_EXPR LIMIT [REPO] — fused id list, best first.
#
# Reciprocal Rank Fusion over the two ranked lists: score = sum 1/(K + rank). RRF is
# used rather than blending the raw scores because BM25 and cosine are not on a
# comparable scale and never will be -- fusing by RANK sidesteps the calibration
# problem entirely, which is why it is the standard choice for hybrid retrieval.
#
# K=60 is the conventional constant; it flattens the top of each list just enough
# that one list cannot dominate purely by being confident.
#
# A row present in only one list still scores: this can only ADD results, never
# remove one lexical search would have found. That is the additive guarantee.
jm_embed_rrf() {
  local qvec="${1:-}" fts="${2:-}" limit="${3:-10}" repo="${4:-}"
  [ -n "$qvec" ] || return 1
  local scope=''
  [ -n "$repo" ] && scope="AND e.repo_key = '$(jm_sql_quote "$repo" | sed "s/^'//;s/'\$//")'"

  jm_vec_read "
    WITH lex AS (
      SELECT e.id AS id, row_number() OVER (ORDER BY bm25(entries_fts, 8.0,2.0,2.0,3.0,3.0,2.5,1.5,2.0,1.0)) AS r
      FROM entries_fts JOIN entries e ON e.id = entries_fts.rowid
      WHERE entries_fts MATCH '$(printf '%s' "$fts" | sed "s/'/''/g")'
        AND e.superseded = 0 $scope
      LIMIT 200
    ),
    sem AS (
      SELECT v.id AS id, row_number() OVER (ORDER BY v.distance) AS r
      FROM entry_vec v
      WHERE v.e MATCH vec_f32('[$qvec]') AND k = 200
    ),
    fused AS (
      SELECT id, sum(w) AS s FROM (
        SELECT id, 1.0/(60 + r) AS w FROM lex
        UNION ALL
        SELECT s.id, 1.0/(60 + s.r) FROM sem s
          JOIN entries e2 ON e2.id = s.id
         WHERE e2.superseded = 0 $(printf '%s' "$scope" | sed 's/e\./e2./g')
      ) GROUP BY id
    )
    SELECT f.id FROM fused f ORDER BY f.s DESC LIMIT $limit;"
}

# jm_embed_status — one line per fact, for `doctor` and `status`.
jm_embed_status() {
  if ! jm_embed_have_ext; then printf 'extension  absent (semantic search off)\n'; return 0; fi
  if ! jm_embed_have_key; then printf 'extension  present, but OPENAI_API_KEY is unset (semantic search off)\n'; return 0; fi
  local n p
  n="$(jm_vec_read 'SELECT count(*) FROM entry_vec;' 2>/dev/null)"
  p="$(jm_embed_pending 2>/dev/null)"
  printf 'ready      %s vector(s), %s row(s) pending, dim %s, %s\n' \
    "${n:-0}" "${p:-?}" "$JM_EMBED_DIM" "$JM_EMBED_MODEL"
}

# jm_embed_valid_ids LIST
# stdout: LIST if it is a bare comma-separated id list, nothing otherwise.
# rc 0 valid · 1 rejected.
# The value this guards comes back from an HTTP response and is then interpolated
# into rank.sql. Accept only the exact shape the store produces -- integers joined
# by single commas -- and drop the lane entirely on anything else. Dropping
# silently is correct here: the caller's fallback is a lexical search that already
# works, so a bad response costs recall, never correctness or an error.
jm_embed_valid_ids() {
  case "${1:-}" in
    '' | *[!0-9,]* | ,* | *, | *,,*) return 1 ;;
  esac
  printf '%s\n' "$1"
}
