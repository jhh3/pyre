package secretegress

// Cmd is one simple command of a shell line, for other cheap command
// checks (internal/killguard). Name is the program after assignments and
// wrappers (sudo, env, nohup, timeout N, xargs, ...), lowercased; Args are
// the words after it, quotes removed. Words are all of its words, wrappers
// included.
type Cmd struct {
	Name  string
	Args  []string
	Words []string
}

// Commands splits a shell line into its simple commands with the same
// forgiving lexer as Check: $(...) substitutions are commands of their own,
// and the strings and heredocs a local shell runs (bash -c "...", eval,
// bash <<EOF) are parsed too, up to a bounded depth. ssh's remote commands
// are not included (they run on another machine).
func Commands(line string) []Cmd {
	var out []Cmd
	var walk func(cmds []command, depth int)
	walk = func(cmds []command, depth int) {
		for _, c := range cmds {
			name, rest := c.name()
			out = append(out, Cmd{Name: name, Args: rest, Words: c.words})
			if shells[name] && depth < maxDepth {
				for _, w := range rest {
					if shellArgRe.MatchString(w) {
						walk(parse(w, depth+1), depth+1)
					}
				}
				for _, h := range c.heredocs {
					walk(parse(h, depth+1), depth+1)
				}
			}
		}
	}
	walk(parse(line, 0), 0)
	return out
}
