#!/usr/bin/env bash
# jhh3-mem — lib/adapters/_projection.sh
# The 14-column TSV contract, shared by every adapter. Never host-specific.
_jm_adapter_projection() {
  cat <<SQL
SELECT
  replace(replace(replace(coalesce(f_title,''),       char(13),''),char(9),'\t'),char(10),'\n'),
  replace(replace(replace(coalesce(f_first_prompt,''),char(13),''),char(9),'\t'),char(10),'\n'),
  replace(replace(replace(coalesce(f_request,''),     char(13),''),char(9),'\t'),char(10),'\n'),
  replace(replace(replace(coalesce(f_outcome,''),     char(13),''),char(9),'\t'),char(10),'\n'),
  replace(replace(replace(coalesce(f_files,''),       char(13),''),char(9),'\t'),char(10),'\n'),
  coalesce(f_nmod,0),
  coalesce(f_nread,0),
  replace(replace(replace(coalesce(f_cmds,''),        char(13),''),char(9),'\t'),char(10),'\n'),
  replace(replace(replace(coalesce(f_branch,''),      char(13),''),char(9),'\t'),char(10),'\n'),
  replace(replace(replace(coalesce(f_cwd,''),         char(13),''),char(9),'\t'),char(10),'\n'),
  coalesce(f_turns,0),
  coalesce(f_started,0),
  coalesce(f_valid,0),
  coalesce(f_total,0)
FROM f;
SQL
}
