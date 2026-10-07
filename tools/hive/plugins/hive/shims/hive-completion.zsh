# zsh completion for the hive CLI — source from .zshrc (after compinit),
# or place on $fpath as _hive. Kept dependency-free and fast: the only
# subprocess is `hive wt repos --names`, and only when completing --repo.

_hive_repos() {
  local -a repos
  repos=(${(f)"$(hive wt repos --names 2>/dev/null)"})
  (( $#repos )) && _describe -t repos 'repo' repos
}

_hive() {
  local -a verbs wt_verbs
  verbs=(ask search grep show timeline why session resume-context brief
         wt ws mem sync raw enrich doctor status install uninstall migrate
         rebuild done park pin note forget wrong tick help)
  wt_verbs=(open ls rm pick repos)

  if (( CURRENT == 2 )); then
    _describe -t commands 'hive command' verbs
    return
  fi

  if [[ $words[CURRENT-1] == --repo ]]; then
    _hive_repos
    return
  fi

  case $words[2] in
    wt)
      if (( CURRENT == 3 )); then
        _describe -t commands 'hive wt command' wt_verbs
      else
        _default
      fi
      ;;
    ws|mem)
      _default   # vendored CLIs: fall through to file/default completion
      ;;
    *)
      _default
      ;;
  esac
}

(( $+functions[compdef] )) && compdef _hive hive
