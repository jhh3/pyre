#!/usr/bin/env bash
# jhh3-mem — lib/adapters/claude-code.sh
#
# ADAPTER CONTRACT. An adapter defines one function, _jm_adapter_cte, which emits
# a SQL CTE chain ending in a CTE named `f` whose columns are exactly:
#   f_title f_first_prompt f_request f_outcome f_files f_nmod f_nread f_cmds
#   f_branch f_cwd f_turns f_started f_valid f_total
# capture.sh appends the shared 14-column projection (_jm_adapter_projection), so
# the column contract lives in one place and cannot drift between hosts.
#
# Input relation: temp.raw(line TEXT) — one JSONL line per row, .import'ed by
# capture.sh. Adapters never read files or shell out; they are pure SQL emitters.
#
# This adapter reads Claude Code's transcript shape: $.type in assistant/user/ai-title,
# with tool calls as content blocks carrying $.name / $.input.file_path / $.input.command.
# Tool names are Claude's native vocabulary (Edit/Write/NotebookEdit/Bash/Read/Grep/Glob),
# which is the CANONICAL vocabulary every adapter normalizes into.

_jm_adapter_cte() {
  cat <<SQL
WITH
v AS (SELECT rowid AS rn, line FROM temp.raw WHERE json_valid(line) = 1),
asm AS (
  SELECT rn, line FROM v
  WHERE json_extract(line,'\$.type') = 'assistant'
    AND json_type(line,'\$.message.content') = 'array'
),
blk AS (
  SELECT a.rn AS rn, c.key AS ck, c.type AS ctype, c.value AS cval
  FROM asm a, json_each(a.line,'\$.message.content') c
),
tu AS (
  SELECT rn, ck,
    CASE WHEN ctype='object' THEN json_extract(cval,'\$.name') END              AS tname,
    CASE WHEN ctype='object' THEN json_extract(cval,'\$.input.file_path') END   AS fpath,
    CASE WHEN ctype='object' THEN json_extract(cval,'\$.input.command') END     AS bcmd,
    CASE WHEN ctype='object' THEN json_extract(cval,'\$.type') END              AS btype,
    CASE WHEN ctype='object' THEN json_extract(cval,'\$.text') END              AS btext
  FROM blk
),
fm AS (
  SELECT DISTINCT fpath AS p FROM tu
  WHERE tname IN ('Edit','Write','NotebookEdit') AND coalesce(fpath,'') <> ''
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
  SELECT rn,
    CASE
      WHEN json_type(line,'\$.message.content') = 'text'
        THEN json_extract(line,'\$.message.content')
      WHEN json_type(line,'\$.message.content') = 'array'
        THEN (SELECT group_concat(json_extract(c.value,'\$.text'), ' ')
                FROM json_each(line,'\$.message.content') c
               WHERE CASE WHEN c.type='object' THEN json_extract(c.value,'\$.type') END = 'text')
      ELSE ''
    END AS t
  FROM v
  WHERE json_extract(line,'\$.type') = 'user'
    AND json_extract(line,'\$.promptSource') = 'typed'
  ORDER BY rn LIMIT $JM_CAP_MAX_PROMPTS
),
f AS (
  SELECT
    (SELECT json_extract(line,'\$.aiTitle') FROM v
      WHERE json_extract(line,'\$.type')='ai-title'
        AND coalesce(json_extract(line,'\$.aiTitle'),'') <> ''
      ORDER BY rn DESC LIMIT 1)                                              AS f_title,
    (SELECT t FROM tp WHERE coalesce(t,'') <> '' ORDER BY rn LIMIT 1)        AS f_first_prompt,
    (SELECT group_concat(t, char(10)) FROM
       (SELECT t FROM tp WHERE coalesce(t,'') <> '' ORDER BY rn))            AS f_request,
    (SELECT (SELECT btext FROM tu t2
              WHERE t2.rn = a.rn AND t2.btype='text' AND coalesce(t2.btext,'') <> ''
              ORDER BY t2.ck DESC LIMIT 1)
       FROM asm a
      WHERE EXISTS (SELECT 1 FROM tu t3
                     WHERE t3.rn = a.rn AND t3.btype='text' AND coalesce(t3.btext,'') <> '')
      ORDER BY a.rn DESC LIMIT 1)                                            AS f_outcome,
    (SELECT group_concat(p, char(10)) FROM
       (SELECT p FROM fm ORDER BY p LIMIT $JM_CAP_MAX_FILES))                AS f_files,
    (SELECT count(*) FROM fm)                                                AS f_nmod,
    (SELECT count(*) FROM tu WHERE tname IN ('Read','Grep','Glob'))          AS f_nread,
    (SELECT group_concat(h, ' ') FROM
       (SELECT h FROM cmdh WHERE h <> '' ORDER BY h LIMIT $JM_CAP_MAX_CMDS)) AS f_cmds,
    (SELECT json_extract(line,'\$.gitBranch') FROM v
      WHERE coalesce(json_extract(line,'\$.gitBranch'),'') <> ''
      ORDER BY rn DESC LIMIT 1)                                              AS f_branch,
    (SELECT json_extract(line,'\$.cwd') FROM v
      WHERE coalesce(json_extract(line,'\$.cwd'),'') <> ''
      ORDER BY rn DESC LIMIT 1)                                              AS f_cwd,
    (SELECT count(*) FROM temp.raw)                                          AS f_turns,
    (SELECT CAST(strftime('%s', substr(json_extract(line,'\$.timestamp'),1,19)) AS INTEGER)
       FROM v WHERE coalesce(json_extract(line,'\$.timestamp'),'') <> ''
      ORDER BY rn ASC LIMIT 1)                                               AS f_started,
    (SELECT count(*) FROM v)                                                 AS f_valid,
    (SELECT count(*) FROM temp.raw)                                          AS f_total
)
SQL
}
