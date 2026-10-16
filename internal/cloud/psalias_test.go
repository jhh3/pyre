package cloud

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// PowerShell resolves aliases before functions, so a script function named
// like a built-in alias is never called: `function Md` ran mkdir instead
// (the first real Windows VM boot read a new folder as its config).
var psBuiltinAliases = strings.Fields(`ac asnp cat cd chdir clc clear clhy cli clp cls clv cnsn compare copy cp cpi cpp
curl cvpa dbp del diff dir dnsn ebp echo epal epcsv epsn erase etsn exsn fc fhx fl foreach ft fw gal gbp gc gcb gci
gcm gcs gdr ghy gi gin gjb gl gm gmo gp gps gpv group gsn gsnp gsv gtz gu gv gwmi h history icm iex ihy ii ipal ipcsv
ipmo ipsn irm ise iwmi iwr kill lp ls man md measure mi mount move mp mv nal ndr ni nmo npssc nsn nv ogv oh popd ps
pushd pwd r rbp rcjb rcsn rd rdr ren ri rjb rm rmdir rmo rni rnp rp rsn rsnp rujb rv rvpa rwmi sajb sal saps sasv sbp
sc scb select set shcm si sl sleep sls sort sp spjb spps spsv start stz sujb sv swmi tee trcm type wget where wjb write`)

func TestPowerShellFunctionsDoNotShadowAliases(t *testing.T) {
	alias := map[string]bool{}
	for _, a := range psBuiltinAliases {
		alias[a] = true
	}
	files, _ := filepath.Glob("*.ps1")
	more, _ := filepath.Glob(filepath.Join("..", "..", "scripts", "install", "*.ps1"))
	files = append(files, more...)
	if len(files) < 3 {
		t.Fatalf("found only %v", files)
	}
	fn := regexp.MustCompile(`(?im)^\s*function\s+([A-Za-z][\w-]*)`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range fn.FindAllStringSubmatch(string(b), -1) {
			if alias[strings.ToLower(m[1])] {
				t.Errorf("%s: function %s is shadowed by the built-in PowerShell alias %q", f, m[1], strings.ToLower(m[1]))
			}
		}
	}
}
