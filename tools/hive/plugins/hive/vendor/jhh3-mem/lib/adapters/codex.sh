#!/usr/bin/env bash
# jhh3-mem — lib/adapters/codex.sh
#
# Adapter for OpenAI Codex rollout transcripts. See claude-code.sh for the full
# ADAPTER CONTRACT; this file emits the same `f` CTE from a different input shape.
#
# Codex rollout JSONL lines are {timestamp, type, payload}:
#   session_meta   payload.{session_id,cwd,originator,cli_version}   (line 1 only)
#   event_msg      payload.type in user_message|agent_message|task_started|token_count
#   response_item  payload.type in function_call|custom_tool_call|reasoning|message
#
# TWO TOOL-CALL SHAPES, both live in the wild and both handled below:
#   function_call      payload.name + payload.arguments  (a JSON *string* to re-parse)
#   custom_tool_call   payload.name + payload.input      (a RAW string, not JSON)
# apply_patch arrives as a custom_tool_call, and its input is a patch whose
# `*** Add|Update|Delete File: <path>` headers name the files -- so on this path Codex
# *does* yield real paths, and they are used rather than guessed at. Verified against a
# live codex 0.147.0 session; the 0.142.0-era corpus on this machine uses function_call
# with exec_command, so both must keep working.
#
# NORMALIZATION. Tool names are mapped into Claude's canonical vocabulary so every
# downstream consumer (durability, ranking, render) stays host-agnostic.
# Measured over 15,200 real function_call rows on this machine:
#   exec_command 12915 (85%) · write_stdin 2034 · update_plan 159 · spawn_agent 20 · rest <20
# There is NO apply_patch / write_file / read_file tool: Codex mutates files THROUGH
# the shell. So file paths are not recoverable from a tool name the way they are on
# Claude Code, and f_files is deliberately left empty rather than guessed at by
# parsing heredocs out of command text.
#
# f_nmod therefore counts MUTATING COMMANDS, not distinct paths. That keeps the
# file-grounding signal that `durability` depends on (a measured 3.5-4.1x predictor)
# non-zero for Codex sessions. Without it every Codex row would look like a session
# that touched nothing, score below the 0.9 injection floor, and be captured but
# never injected -- silently defeating cross-host recall.
#
# EXCLUDED ON PURPOSE:
#   payload.base_instructions on session_meta -- Codex's entire system prompt, the
#     bulk of a rollout's bytes (764 KB of one 197-line sample) and pure noise.
#   response_item/reasoning -- model chain-of-thought; not ours to persist.

_jm_adapter_cte() {
  cat <<SQL
WITH RECURSIVE
v AS (SELECT rowid AS rn, line FROM temp.raw WHERE json_valid(line) = 1),
p AS (
  SELECT rn, line,
    json_extract(line,'\$.type')          AS ltype,
    json_extract(line,'\$.payload.type')  AS ptype
  FROM v
),
fc AS (
  SELECT rn,
    json_extract(line,'\$.payload.name')      AS tname_raw,
    json_extract(line,'\$.payload.arguments') AS args,
    NULL                                      AS rawin
  FROM p WHERE ltype='response_item' AND ptype='function_call'
  UNION ALL
  SELECT rn,
    json_extract(line,'\$.payload.name')  AS tname_raw,
    NULL                                  AS args,
    json_extract(line,'\$.payload.input') AS rawin
  FROM p WHERE ltype='response_item' AND ptype='custom_tool_call'
),
-- Patch headers, one row per touched file. Split the patch body on newlines and keep
-- the star-prefixed Add/Update/Delete File header lines; this is the only place
-- Codex hands us a real path rather than a command string.
patchlines(rn, rest, ln) AS (
  SELECT rn, rawin || char(10), '' FROM fc WHERE tname_raw='apply_patch' AND coalesce(rawin,'') <> ''
  UNION ALL
  SELECT rn,
         substr(rest, instr(rest, char(10)) + 1),
         substr(rest, 1, instr(rest, char(10)) - 1)
  FROM patchlines WHERE instr(rest, char(10)) > 0
),
patchfiles AS (
  SELECT DISTINCT rn, trim(substr(ln, instr(ln, 'File: ') + 6)) AS pth
  FROM patchlines
  WHERE ln LIKE '*** %File: %' AND instr(ln, 'File: ') > 0
),
tu AS (
  SELECT rn,
    CASE
      WHEN tname_raw = 'apply_patch' THEN 'Edit'
      WHEN tname_raw IN ('exec_command','shell','write_stdin') THEN 'Bash'
      WHEN tname_raw IS NULL THEN NULL
      ELSE tname_raw
    END AS tname,
    CASE WHEN json_valid(coalesce(args,'')) = 1
         THEN coalesce(json_extract(json(args),'\$.cmd'), json_extract(json(args),'\$.chars'))
    END AS bcmd
  FROM fc
),
mut AS (
  SELECT (SELECT count(*) FROM patchfiles) + (SELECT count(*) FROM (
  SELECT 1 FROM tu
  WHERE tname='Bash' AND coalesce(bcmd,'') <> '' AND (
       bcmd LIKE '%apply_patch%' OR bcmd LIKE '%git apply%'  OR bcmd LIKE '%git commit%'
    OR bcmd LIKE '%sed -i%'      OR bcmd LIKE '%tee %'       OR bcmd LIKE '%>%'
    OR bcmd LIKE 'mv %'          OR bcmd LIKE 'cp %'         OR bcmd LIKE 'rm %'
    OR bcmd LIKE '%mkdir %'      OR bcmd LIKE '%touch %'
  ))) AS n
),
rd AS (
  SELECT count(*) AS n FROM tu
  WHERE tname='Bash' AND coalesce(bcmd,'') <> '' AND (
       bcmd LIKE 'cat %'  OR bcmd LIKE 'rg %'   OR bcmd LIKE 'grep %'
    OR bcmd LIKE 'ls %'   OR bcmd LIKE 'find %' OR bcmd LIKE 'head %'
    OR bcmd LIKE 'tail %' OR bcmd LIKE 'wc %'
  )
),
cmdh AS (
  SELECT DISTINCT substr(
           CASE WHEN instr(t,' ') > 0 THEN substr(t,1,instr(t,' ')-1) ELSE t END, 1, 32) AS h
  FROM (
    SELECT trim(replace(replace(replace(coalesce(bcmd,''),char(10),' '),char(9),' '),char(13),' ')) AS t
    FROM tu WHERE tname = 'Bash'
  )
  WHERE t <> ''
),
tp AS (
  SELECT rn, json_extract(line,'\$.payload.message') AS t
  FROM p WHERE ltype='event_msg' AND ptype='user_message'
  ORDER BY rn LIMIT $JM_CAP_MAX_PROMPTS
),
f AS (
  SELECT
    ''                                                                       AS f_title,
    (SELECT t FROM tp WHERE coalesce(t,'') <> '' ORDER BY rn LIMIT 1)        AS f_first_prompt,
    (SELECT group_concat(t, char(10)) FROM
       (SELECT t FROM tp WHERE coalesce(t,'') <> '' ORDER BY rn))            AS f_request,
    (SELECT json_extract(line,'\$.payload.message') FROM p
      WHERE ltype='event_msg' AND ptype='agent_message'
        AND coalesce(json_extract(line,'\$.payload.message'),'') <> ''
      ORDER BY rn DESC LIMIT 1)                                              AS f_outcome,
    (SELECT group_concat(pth, char(10)) FROM
       (SELECT DISTINCT pth FROM patchfiles ORDER BY pth LIMIT $JM_CAP_MAX_FILES)) AS f_files,
    (SELECT n FROM mut)                                                      AS f_nmod,
    (SELECT n FROM rd)                                                       AS f_nread,
    (SELECT group_concat(h, ' ') FROM
       (SELECT h FROM cmdh WHERE h <> '' ORDER BY h LIMIT $JM_CAP_MAX_CMDS)) AS f_cmds,
    ''                                                                       AS f_branch,
    (SELECT json_extract(line,'\$.payload.cwd') FROM p
      WHERE ltype='session_meta' AND coalesce(json_extract(line,'\$.payload.cwd'),'') <> ''
      ORDER BY rn ASC LIMIT 1)                                               AS f_cwd,
    (SELECT count(*) FROM temp.raw)                                          AS f_turns,
    (SELECT CAST(strftime('%s', substr(json_extract(line,'\$.timestamp'),1,19)) AS INTEGER)
       FROM p WHERE coalesce(json_extract(line,'\$.timestamp'),'') <> ''
      ORDER BY rn ASC LIMIT 1)                                               AS f_started,
    (SELECT count(*) FROM v)                                                 AS f_valid,
    (SELECT count(*) FROM temp.raw)                                          AS f_total
)
SQL
}
