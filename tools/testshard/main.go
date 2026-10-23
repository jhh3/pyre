// Command testshard runs the Go test suite as many small go test processes
// at once, so a package whose tests mostly wait (Settle loops, poll
// intervals, real tmux) no longer sets the wall time of the whole run. It is
// what the fast CI check runs (`make test-fast`); `go test ./...` stays the
// plain, unsharded way to run the suite.
//
//	go run ./tools/testshard [-short] [-race] [-j N] [-shards N -index I] [packages]
//
// Each package's top-level tests are split into chunks of about -chunk
// seconds, using the durations in weights.json (unknown tests count
// -default). Chunks are spread over -shards CI jobs (this job runs chunk set
// -index), and each job runs its chunks -j at a time, longest first. A chunk
// is one `go test -run '^(A|B|...)$' pkg`, so go test's result cache applies:
// a chunk whose package and dependencies did not change since a cached
// passing run is not run again (pass -count 1 to rerun everything). Before
// the chunks, one `go list -export` compiles every package the job needs,
// so concurrent go test processes do not compile the same package twice.
//
// Test names come from parsing the package's test files (those go list
// selects for this GOOS and tags) with go test's own rules: Test*, Fuzz*
// and Example* functions with an Output comment.
//
// -record FILE writes the observed per-test durations as a new weights file
// (regenerate with `make test-weights`; only balance depends on it).
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:embed weights.json
var weightsJSON []byte

type pkg struct {
	ImportPath   string
	Dir          string
	TestGoFiles  []string
	XTestGoFiles []string
}

type chunk struct {
	pkg    *pkg
	tests  []string // empty: the package's whole test binary
	weight float64
}

type event struct {
	Action  string
	Package string
	Test    string
	Elapsed float64
	Output  string
}

type result struct {
	c       chunk
	ok      bool
	cached  bool
	dur     time.Duration
	failed  []string
	output  map[string]*strings.Builder // per test, plus "" for package-level output
	elapsed map[string]float64
	execErr error
}

type options struct {
	short, race bool
	count       int
	timeout     time.Duration
	verbose     bool
}

func main() {
	var o options
	flag.BoolVar(&o.short, "short", false, "go test -short")
	flag.BoolVar(&o.race, "race", false, "go test -race")
	flag.IntVar(&o.count, "count", 0, "go test -count (1 reruns cached results; 0 omits the flag)")
	flag.DurationVar(&o.timeout, "timeout", 10*time.Minute, "go test -timeout, per process")
	flag.BoolVar(&o.verbose, "v", false, "print every chunk as it finishes")
	jobs := flag.Int("j", 2*runtime.NumCPU(), "go test processes at once")
	shards := flag.Int("shards", 1, "number of CI jobs the suite is split over")
	index := flag.Int("index", 0, "which of the -shards jobs this is (0-based)")
	chunkSecs := flag.Float64("chunk", 5, "target seconds of tests per process (weights.json seconds: a fast machine; CI runs them about 2-3x slower)")
	def := flag.Float64("default", 0.1, "assumed seconds for a test missing from weights.json")
	record := flag.String("record", "", "write observed test durations to this weights file")
	pin := flag.Bool("pin-mtimes", false, "only set each tracked file's mtime from its content (CI, after checkout)")
	showPlan := flag.Bool("plan", false, "only print each shard's planned processes and seconds (weights.json)")
	verify := flag.Bool("verify", false, "only check the parsed test names against go test -list (builds every test binary)")
	alone := flag.String("alone", "./tests/throttle", "comma-separated packages whose tests run alone, after the rest (latency budgets)")
	flag.Parse()
	if *index < 0 || *index >= *shards || *jobs < 1 {
		fatalf("bad -shards/-index/-j")
	}
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	weights := map[string]float64{}
	if err := json.Unmarshal(weightsJSON, &weights); err != nil {
		fatalf("weights.json: %v", err)
	}

	if *pin {
		n, err := pinMtimes()
		if err != nil {
			fatalf("pin mtimes: %v", err)
		}
		fmt.Printf("testshard: pinned the mtimes of %d tracked files\n", n)
		return
	}
	pkgs, err := listPackages(patterns)
	if err != nil {
		fatalf("%v", err)
	}
	if *verify {
		if err := verifyNames(pkgs); err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("testshard: test names match go test -list in %d packages\n", len(pkgs))
		return
	}
	all, err := plan(pkgs, weights, *def, *chunkSecs)
	if err != nil {
		fatalf("%v", err)
	}
	assigned := assign(all, *shards)
	if *showPlan {
		for i, cs := range assigned {
			fmt.Printf("shard %d: %3d processes, %6.1f s of tests\n", i+1, len(cs), load(cs))
		}
		best, bestMax := 0, -1.0
		for seed := 0; seed < 1000; seed++ {
			m := 0.0
			for _, cs := range assignSeeded(all, *shards, seed) {
				m = max(m, load(cs))
			}
			if bestMax < 0 || m < bestMax {
				best, bestMax = seed, m
			}
		}
		fmt.Printf("assignSeed %d gives the heaviest shard %.1f s (the current seed, %d, is shown above)\n", best, bestMax, assignSeed)
		return
	}
	mine := assigned[*index]

	start := time.Now()
	need := map[string]bool{}
	for _, c := range mine {
		need[c.pkg.ImportPath] = true
	}
	compile(need, o.race)
	built := time.Since(start)

	// Latency-budget tests (-alone) run after the rest, one process at a
	// time, so the other processes do not eat their CPU.
	var shared, solo []chunk
	for _, c := range mine {
		if isAlone(c.pkg.ImportPath, *alone) {
			solo = append(solo, c)
		} else {
			shared = append(shared, c)
		}
	}
	results := run(shared, o, *jobs)
	results = append(results, run(solo, o, 1)...)

	failed, cached := 0, 0
	observed := map[string]float64{}
	for _, r := range results {
		for t, e := range r.elapsed {
			observed[r.c.pkg.ImportPath+" "+t] = e
		}
		if r.cached {
			cached++
		}
		if r.ok {
			continue
		}
		failed++
		fmt.Printf("\n--- FAIL %s (%s)\n", r.c.pkg.ImportPath, label(r.c))
		if len(r.failed) == 0 {
			// Build failure, crash, panic or timeout outside any one test:
			// show everything.
			fmt.Printf("    %v\n", r.execErr)
			for _, b := range r.output {
				fmt.Print(indent(b.String()))
			}
			continue
		}
		if b := r.output[""]; b != nil && strings.TrimSpace(b.String()) != "" {
			fmt.Print(indent(b.String()))
		}
		for _, t := range r.failed {
			if b := r.output[t]; b != nil {
				fmt.Print(indent(b.String()))
			}
		}
	}
	slowest(observed, 15)
	fmt.Printf("testshard: shard %d/%d: %d packages, %d processes (%d cached), compile %s, total %s\n",
		*index+1, *shards, len(need), len(results), cached, built.Round(100*time.Millisecond), time.Since(start).Round(100*time.Millisecond))
	if *record != "" {
		if err := writeWeights(*record, observed); err != nil {
			fatalf("%v", err)
		}
	}
	if failed > 0 {
		fmt.Printf("FAIL: %d of %d processes failed\n", failed, len(results))
		os.Exit(1)
	}
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "testshard: "+f+"\n", a...)
	os.Exit(2)
}

// isAlone reports whether importPath is one of the -alone packages, given
// as import paths or as ./dir relative to the module root.
func isAlone(importPath, list string) bool {
	for _, a := range strings.Split(list, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if importPath == a || (strings.HasPrefix(a, "./") && strings.HasSuffix(importPath, "/"+strings.TrimPrefix(a, "./"))) {
			return true
		}
	}
	return false
}

func listPackages(patterns []string) ([]*pkg, error) {
	args := append([]string{"list", "-json=ImportPath,Dir,TestGoFiles,XTestGoFiles"}, patterns...)
	out, err := exec.Command("go", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w", err)
	}
	var pkgs []*pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		p := new(pkg)
		if err := dec.Decode(p); err != nil {
			return nil, err
		}
		if len(p.TestGoFiles)+len(p.XTestGoFiles) > 0 {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, nil
}

// plan splits each package's tests into chunks of about chunkSecs.
func plan(pkgs []*pkg, weights map[string]float64, def, chunkSecs float64) ([]chunk, error) {
	var out []chunk
	for _, p := range pkgs {
		names, err := testNames(p)
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			continue // test files without tests: nothing to run
		}
		w := func(t string) float64 {
			if v, ok := weights[p.ImportPath+" "+t]; ok {
				return v
			}
			return def
		}
		// Heaviest first, then fill chunks greedily (first fit decreasing),
		// so one long test does not drag short ones into its process.
		sort.Slice(names, func(i, j int) bool {
			if w(names[i]) != w(names[j]) {
				return w(names[i]) > w(names[j])
			}
			return names[i] < names[j]
		})
		var cs []chunk
		for _, t := range names {
			placed := false
			for i := range cs {
				if cs[i].weight+w(t) <= chunkSecs {
					cs[i].tests = append(cs[i].tests, t)
					cs[i].weight += w(t)
					placed = true
					break
				}
			}
			if !placed {
				cs = append(cs, chunk{pkg: p, tests: []string{t}, weight: w(t)})
			}
		}
		if len(cs) == 1 {
			cs[0].tests = nil // the whole package: a plain go test pkg
		}
		out = append(out, cs...)
	}
	return out, nil
}

// testNames lists what go test would run in p, by go test's own rules
// (cmd/go/internal/load/test.go): top-level Test* and Fuzz* functions whose
// next rune is not lower case (TestMain only with a *testing.T), and
// examples with an Output comment.
func testNames(p *pkg) ([]string, error) {
	fset := token.NewFileSet()
	var names []string
	for _, f := range append(append([]string{}, p.TestGoFiles...), p.XTestGoFiles...) {
		file, err := parser.ParseFile(fset, filepath.Join(p.Dir, f), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			name := fn.Name.Name
			switch {
			case name == "TestMain":
				if takesT(fn) {
					names = append(names, name)
				}
			case isTest(name, "Test"), isTest(name, "Fuzz"):
				names = append(names, name)
			}
		}
		for _, e := range doc.Examples(file) {
			if e.Output != "" || e.EmptyOutput {
				names = append(names, "Example"+e.Name)
			}
		}
	}
	return names, nil
}

// pinMtimes gives every tracked file an mtime derived from its blob hash,
// and their directories one fixed mtime. go test's result cache keys the
// files a test opens (goldens, testdata) by size, mode and mtime, and a
// fresh checkout makes every mtime new, so without this no such test is
// ever cached in CI. Same content, same mtime; changed content, another.
func pinMtimes() (int, error) {
	out, err := exec.Command("git", "ls-files", "-s", "-z").Output()
	if err != nil {
		return 0, fmt.Errorf("git ls-files: %w", err)
	}
	base := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	dirs := map[string]bool{".": true}
	n := 0
	for _, rec := range strings.Split(string(out), "\x00") {
		// "<mode> <sha> <stage>\t<path>"
		meta, name, ok := strings.Cut(rec, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 || !strings.HasPrefix(f[0], "100") {
			continue // not a regular file (symlink, submodule)
		}
		var off int64
		if _, err := fmt.Sscanf(f[1][:12], "%x", &off); err != nil {
			return n, err
		}
		t := base.Add(time.Duration(off%(20*365*24*3600)) * time.Second)
		if err := os.Chtimes(name, t, t); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return n, err
		}
		n++
		for d := filepath.Dir(name); !dirs[d]; d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	for d := range dirs {
		if err := os.Chtimes(d, base, base); err != nil && !os.IsNotExist(err) {
			return n, err
		}
	}
	return n, nil
}

// verifyNames compares testNames with what each test binary lists, so a
// test the parser would miss (and no chunk would run) shows up.
func verifyNames(pkgs []*pkg) error {
	var bad []string
	for _, p := range pkgs {
		names, err := testNames(p)
		if err != nil {
			return err
		}
		out, err := exec.Command("go", "test", "-vet=off", "-list", ".*", p.ImportPath).Output()
		if err != nil {
			return fmt.Errorf("go test -list %s: %v", p.ImportPath, err)
		}
		want := map[string]bool{}
		for _, l := range strings.Split(string(out), "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasPrefix(l, "ok ") || isTest(l, "Benchmark") {
				continue
			}
			want[l] = true
		}
		got := map[string]bool{}
		for _, n := range names {
			got[n] = true
		}
		for n := range want {
			if !got[n] {
				bad = append(bad, p.ImportPath+": missed "+n)
			}
		}
		for n := range got {
			if !want[n] {
				bad = append(bad, p.ImportPath+": extra "+n)
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("test names differ from go test -list:\n  %s", strings.Join(bad, "\n  "))
	}
	return nil
}

func isTest(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	if len(name) == len(prefix) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len(prefix):])
	return !unicode.IsLower(r)
}

// takesT reports whether fn's one parameter is a *testing.T (or *T).
func takesT(fn *ast.FuncDecl) bool {
	ps := fn.Type.Params.List
	if len(ps) != 1 {
		return false
	}
	star, ok := ps[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch x := star.X.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "T"
	case *ast.Ident:
		return x.Name == "T"
	}
	return false
}

// assign spreads chunks over shards: a package's chunks go round-robin
// from a shard picked by hashing its import path. Every job computes the
// same assignment, and it is stable: a package moves only when its own
// chunks change, so the other packages keep their shard, and with it
// their compiled code and cached results in that shard's CI cache.
// (Packing longest-first onto the lightest shard balances a little better,
// but one new test reshuffles every shard's packages.)
//
// assignSeed salts the hash; `testshard -plan -shards 6` prints the seed
// that balances the CI shards best for the current weights. Change it only
// together with re-recorded weights (it moves every package).
const assignSeed = 253

func assign(cs []chunk, shards int) [][]chunk { return assignSeeded(cs, shards, assignSeed) }

func assignSeeded(cs []chunk, shards, seed int) [][]chunk {
	out := make([][]chunk, shards)
	next := map[string]int{}
	for _, c := range cs {
		k, ok := next[c.pkg.ImportPath]
		if !ok {
			h := fnv.New32a()
			fmt.Fprintf(h, "%d %s", seed, c.pkg.ImportPath)
			k = int(h.Sum32() % uint32(shards))
		}
		out[k] = append(out[k], c)
		next[c.pkg.ImportPath] = (k + 1) % shards
	}
	return out
}

func load(cs []chunk) float64 {
	var s float64
	for _, c := range cs {
		s += c.weight
	}
	return s
}

// compile builds every package (test variants included) the job's go test
// processes need, in one go command, which compiles each package once and
// in parallel. It links nothing: go test links (or skips, when its result
// is cached) per chunk.
func compile(need map[string]bool, race bool) {
	args := []string{"list", "-test", "-export", "-deps", "-f", ""}
	if race {
		args = append(args, "-race")
	}
	for p := range need {
		args = append(args, p)
	}
	if out, err := exec.Command("go", args...).CombinedOutput(); err != nil {
		// A compile error: the chunks of that package report it as their
		// build failure, so go on rather than hide the other packages.
		fmt.Fprintf(os.Stderr, "testshard: go list -export: %v\n%s", err, out)
	}
}

func run(cs []chunk, o options, jobs int) []*result {
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].weight > cs[j].weight })
	queue := make(chan chunk, len(cs))
	for _, c := range cs {
		queue <- c
	}
	close(queue)
	var mu sync.Mutex
	var results []*result
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range queue {
				r := runChunk(c, o)
				if o.verbose {
					st := map[bool]string{true: "ok", false: "FAIL"}[r.ok]
					if r.cached {
						st = "cached"
					}
					fmt.Printf("%-6s %s (%s) %s\n", st, c.pkg.ImportPath, label(c), r.dur.Round(10*time.Millisecond))
				}
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return results
}

func label(c chunk) string {
	if len(c.tests) == 0 {
		return "all tests"
	}
	if len(c.tests) <= 3 {
		return strings.Join(c.tests, ",")
	}
	return fmt.Sprintf("%s +%d more", c.tests[0], len(c.tests)-1)
}

func runChunk(c chunk, o options) *result {
	r := &result{c: c, output: map[string]*strings.Builder{}, elapsed: map[string]float64{}}
	args := []string{"test", "-json", "-vet=off", "-timeout", o.timeout.String()}
	if o.short {
		args = append(args, "-short")
	}
	if o.race {
		args = append(args, "-race")
	}
	if o.count > 0 {
		args = append(args, fmt.Sprintf("-count=%d", o.count))
	}
	if len(c.tests) > 0 {
		q := make([]string, len(c.tests))
		for i, t := range c.tests {
			q[i] = regexp.QuoteMeta(t)
		}
		args = append(args, "-run", "^("+strings.Join(q, "|")+")$")
	}
	args = append(args, c.pkg.ImportPath)
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout+2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.execErr = err
		return r
	}
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		r.execErr = err
		return r
	}
	sawPass := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		top := e.Test
		if i := strings.IndexByte(top, '/'); i >= 0 {
			top = top[:i]
		}
		if e.Output != "" {
			b := r.output[top]
			if b == nil {
				b = new(strings.Builder)
				r.output[top] = b
			}
			b.WriteString(e.Output)
			if e.Test == "" && strings.HasPrefix(e.Output, "ok ") && strings.Contains(e.Output, "(cached)") {
				r.cached = true
			}
		}
		switch {
		case e.Test == "" && e.Action == "pass":
			sawPass = true
		case e.Test != "" && e.Test == top && (e.Action == "pass" || e.Action == "fail" || e.Action == "skip"):
			r.elapsed[top] = e.Elapsed
			if e.Action == "fail" {
				r.failed = append(r.failed, top)
			}
		}
	}
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	r.dur = time.Since(t0)
	r.execErr = werr
	r.ok = werr == nil && sawPass && len(r.failed) == 0
	if !r.ok && r.execErr == nil {
		r.execErr = fmt.Errorf("go test did not report a pass")
	}
	return r
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(s, "\n") {
		if l != "" {
			b.WriteString("    " + l)
		}
	}
	return b.String()
}

func slowest(observed map[string]float64, n int) {
	type kv struct {
		k string
		v float64
	}
	var all []kv
	for k, v := range observed {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	if len(all) > n {
		all = all[:n]
	}
	if len(all) == 0 {
		return
	}
	fmt.Println("slowest tests:")
	for _, e := range all {
		fmt.Printf("  %6.2fs %s\n", e.v, strings.TrimPrefix(e.k, "github.com/jhh3/pyre/"))
	}
}

// writeWeights keeps tests of at least 0.05 s (the rest use -default), merged
// over the existing file so a partial run does not drop other packages.
func writeWeights(path string, observed map[string]float64) error {
	merged := map[string]float64{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &merged)
	}
	for k, v := range observed {
		if v >= 0.05 {
			merged[k] = float64(int(v*100+0.5)) / 100
		} else {
			delete(merged, k)
		}
	}
	b, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
