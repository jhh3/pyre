# wt — shell shim over `hive wt` (worktree + tmux + session birth/death).
# Only the final `cd` and completion live here; all logic is in hive
# (plugins/hive/lib/hive/wt.py). Replaces ~/.zsh/wt.zsh.
#
# Repo recipes live in ~/.config/hive/hive.toml (see docs/hive.toml.example):
#   [repos.webapp]
#   path = "~/code/acme/webapp"
#   default = true                # `wt` with no -r targets this repo
#   # setup = ["make setup"]      # omit -> Makefile / backend/Makefile fallback
#   # launch = "claude"           # sent into freshly created tmux sessions
#   # env = { FOO = "bar" }       # tmux session + setup environment

wt() {
  local -a repo; local tflag="" dflag="" arg="" cmd=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -t)         tflag="--tmux" ;;
      -d|-td|-dt) tflag="--tmux"; dflag="--detached" ;;
      -r)         shift; repo=(--repo "${1:?wt: -r needs a repo name or path}") ;;
      ls|rm)      cmd="$1" ;;
      -h|help)    hive wt --help; return ;;
      *)          arg="$1" ;;
    esac
    shift
  done
  case "$cmd" in
    ls) hive wt ls "${repo[@]}"; return ;;
    rm) hive wt rm ${arg:+"$arg"} "${repo[@]}"; return ;;
  esac
  local p
  if [[ -n "$tflag" ]]; then
    [[ -z "$arg" ]] && { arg="$(hive wt pick --branch "${repo[@]}")" || return; }
    hive wt open "$arg" $tflag $dflag "${repo[@]}"
  elif [[ -z "$arg" ]]; then
    p="$(hive wt pick "${repo[@]}")" && [[ -n "$p" ]] && cd "$p"
  else
    p="$(hive wt open "$arg" --print-path "${repo[@]}")" && [[ -n "$p" ]] && cd "$p"
  fi
}

_wt() {
  if [[ "${words[CURRENT-1]}" == "-r" ]]; then
    compadd -- ${(f)"$(hive wt repos --names 2>/dev/null)"}
    return
  fi
  compadd -- ls rm -t -d -r
}
(( $+functions[compdef] )) && compdef _wt wt
