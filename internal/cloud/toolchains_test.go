package cloud

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// vortex is the owner's Electron app's CI toolchain (GitHub windows-2025).
var vortex = []string{"node@24.21.0", "pnpm", "bun@1.3.14", "rust", "cmake", "llvm", "python", "vs-buildtools"}

func TestResolveToolchains(t *testing.T) {
	for _, tc := range []struct {
		name, os         string
		configured, flag []string
		want             []string
	}{
		{"default windows", OSWindows, nil, nil, []string{"git", "python", "vs-buildtools", "node@lts"}},
		{"default linux", OSLinux, nil, nil, []string{"git", "python"}},
		{"flags add to the default", OSWindows, nil, []string{"rust"}, []string{"git", "python", "vs-buildtools", "node@lts", "rust"}},
		{"electron windows", OSWindows, []string{"electron"}, nil, []string{"git", "python", "vs-buildtools", "node@lts", "pnpm", "gh"}},
		{"electron linux has no vs-buildtools", OSLinux, []string{"electron"}, nil, []string{"git", "python", "node@lts", "pnpm", "gh"}},
		{"media windows adds parsec", OSWindows, []string{"media"}, nil, []string{"git", "python", "vs-buildtools", "node@lts", "pnpm", "gh", "parsec"}},
		{"media linux drops parsec", OSLinux, []string{"media"}, nil, []string{"git", "python", "node@lts", "pnpm", "gh"}},
		{"vortex exact list, install order", OSWindows, vortex, nil,
			[]string{"python", "vs-buildtools", "node@24.21.0", "pnpm", "bun@1.3.14", "rust", "cmake", "llvm"}},
		{"a flag overrides the configured version", OSWindows, []string{"electron"}, []string{"node@24.21.0", "bun@1.3.14"},
			[]string{"git", "python", "vs-buildtools", "node@24.21.0", "pnpm", "bun@1.3.14", "gh"}},
		{"later entry wins inside one list", OSLinux, []string{"node@22", "node@24.21.0"}, nil, []string{"node@24.21.0"}},
		{"pnpm pulls node", OSLinux, []string{"pnpm@11.1.0"}, nil, []string{"node@lts", "pnpm@11.1.0"}},
		{"rust on windows pulls the build tools", OSWindows, []string{"rust@1.85.0"}, nil, []string{"vs-buildtools", "rust@1.85.0"}},
		{"rust on linux does not", OSLinux, []string{"rust@nightly-2026-01-02"}, nil, []string{"rust@nightly-2026-01-02"}},
		{"normalized", OSLinux, []string{" Node ", "bun@v1.3.14", "NODE@V24.21.0"}, nil, []string{"node@24.21.0", "bun@1.3.14"}},
	} {
		got, err := ResolveToolchains(tc.os, tc.configured, tc.flag)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	for _, bad := range [][]string{{"node@latest"}, {"node@24.1"}, {"bun@1.3"}, {"bun@latest"}, {"git@2.40.0"}, {"vs-buildtools@17"},
		{"rust@stablest"}, {"pnpm@11"}, {"cmake@3.x"}, {"ruby"}, {"node@"}, {"parsec"}} {
		if _, err := ResolveToolchains(OSLinux, bad, nil); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("%v accepted on linux: %v", bad, err)
		}
	}
	if got, err := ResolveToolchains(OSWindows, []string{"parsec"}, nil); err != nil || !reflect.DeepEqual(got, []string{"parsec"}) {
		t.Errorf("parsec on windows: %v %v", got, err)
	}
	if got := withHarnessToolchains([]string{"git"}, []string{"claude", "gh"}); !reflect.DeepEqual(got, []string{"git", "gh"}) {
		t.Errorf("gh harness: %v", got)
	}
	if got := withHarnessToolchains([]string{"gh"}, []string{"gh"}); !reflect.DeepEqual(got, []string{"gh"}) {
		t.Errorf("gh twice: %v", got)
	}
}

func TestLoadConfigToolchainsAndInstallers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cloud.json")
	_ = os.WriteFile(p, []byte(`{"gcp":{"toolchains":["node@24.21.0","pnpm","bun@1.3.14","rust","cmake","llvm","python","vs-buildtools"],
	  "windows_installers":[{"name":"davinci-resolve","gcs_uri":"gs://synthetic-bucket/installers/Resolve_Synthetic.exe",
	    "sha256":"`+strings.Repeat("AB", 32)+`","args":["/i","/q","/noreboot"]}],
	  "windows_gpu_driver":"gs://nvidia-drivers-us-public/GRID/vGPU18.1/synthetic_grid_win10_win11_server2022_server2025.exe"}}`), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.GCP.Toolchains, vortex) || len(c.GCP.WindowsInstallers) != 1 || c.GCP.WindowsInstallers[0].Args[2] != "/noreboot" {
		t.Fatalf("config: %+v", c.GCP)
	}
	ins, err := validateInstallers(c.GCP.WindowsInstallers)
	if err != nil || ins[0].SHA256 != strings.Repeat("ab", 32) {
		t.Fatalf("installers: %v %+v", err, ins)
	}
	if err := validateGPUDriver(c.GCP.WindowsGPUDriver); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []WindowsInstaller{
		{Name: "x y", GCSURI: "gs://b/x.exe", SHA256: strings.Repeat("a", 64)},
		{Name: "x", GCSURI: "https://vendor.invalid/x.exe", SHA256: strings.Repeat("a", 64)},
		{Name: "x", GCSURI: "gs://bkt/x.exe", SHA256: "abc"},
		{Name: "x", GCSURI: "gs://bkt/x.zip", SHA256: strings.Repeat("a", 64)},
	} {
		if _, err := validateInstallers([]WindowsInstaller{bad}); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Errorf("installer %+v accepted", bad)
		}
	}
	if err := validateGPUDriver("gs://someone-elses-bucket/driver.exe"); err == nil {
		t.Error("a private-bucket driver needs a storage identity the VM does not get")
	}
}

func TestCreateCarriesToolchains(t *testing.T) {
	m, g, h, _ := newManager(t)
	m.Cfg.GCP.Toolchains = []string{"electron"}
	g.onCreate = func() { go h.connect("win-build") }
	if _, err := m.Create(context.Background(), CreateOptions{Name: "win-build", OS: OSWindows, Toolchains: vortex}); err != nil {
		t.Fatal(err)
	}
	var bc BootConfig
	if err := json.Unmarshal([]byte(g.files[MetaConfig]), &bc); err != nil {
		t.Fatal(err)
	}
	want := []string{"git", "python", "vs-buildtools", "node@24.21.0", "pnpm", "bun@1.3.14", "rust", "cmake", "llvm", "gh"}
	if !reflect.DeepEqual(bc.Toolchains, want) || bc.GPU != "" {
		t.Fatalf("boot config toolchains: %v (gpu %q)", bc.Toolchains, bc.GPU)
	}
	args := g.called("compute instances create")[0]
	if !contains(args, "--image-family=windows-2025") || DefaultWindowsImage != "windows-2025" {
		t.Fatalf("default Windows image should be Server 2025: %v", args)
	}
	// No gs:// release and no installers: no identity at all.
	m2, g2, _, _ := newManager(t)
	m2.Cfg.Release.URL = "https://releases.invalid/pyre/0.0.0-test/"
	if _, err := m2.Create(context.Background(), CreateOptions{Name: "bad", Toolchains: []string{"node@lts", "ruby"}}); !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "ruby") {
		t.Fatalf("unknown toolchain: %v", err)
	}
	if len(g2.called("compute instances create")) != 0 {
		t.Fatal("a VM was created despite an invalid toolchain")
	}
}

func TestBakeCarriesToolchains(t *testing.T) {
	m, g, _, _ := newManager(t)
	if _, err := m.Bake(context.Background(), BakeOptions{OS: OSWindows, Toolchains: vortex}); err != nil {
		t.Fatal(err)
	}
	var bc BootConfig
	_ = json.Unmarshal([]byte(g.files[MetaConfig]), &bc)
	if bc.Mode != "bake" || !contains(bc.Toolchains, "node@24.21.0") || !contains(bc.Toolchains, "llvm") {
		t.Fatalf("bake toolchains: %+v", bc.Toolchains)
	}
	if !contains(g.called("compute instances create")[0], "--image-family="+DefaultWindowsImage) {
		t.Fatal("Windows bake should start from Server 2025")
	}
}

func TestWindowsGPUNodeUsesVirtualWorkstation(t *testing.T) {
	m, g, h, _ := newManager(t)
	m.Cfg.Release.URL = "https://releases.invalid/pyre/0.0.0-test/"
	m.Cfg.GCP.Toolchains = []string{"media"}
	m.Cfg.GCP.WindowsInstallers = []WindowsInstaller{{Name: "davinci-resolve", GCSURI: "gs://synthetic-bucket/Resolve_Synthetic.exe",
		SHA256: strings.Repeat("cd", 32), Args: []string{"/i", "/q", "/noreboot"}}}
	g.onCreate = func() { go h.connect("win-media") }
	res, err := m.Create(context.Background(), CreateOptions{Name: "win-media", OS: OSWindows, GPU: "nvidia-l4:1", RDP: true})
	if err != nil {
		t.Fatal(err)
	}
	args := g.called("compute instances create")[0]
	for _, want := range []string{"--machine-type=g2-standard-8", "--accelerator=type=nvidia-l4-vws,count=1", "--maintenance-policy=TERMINATE",
		"--scopes=https://www.googleapis.com/auth/devstorage.read_only"} {
		if !contains(args, want) {
			t.Errorf("create args miss %s: %v", want, args)
		}
	}
	var bc BootConfig
	_ = json.Unmarshal([]byte(g.files[MetaConfig]), &bc)
	if bc.GPU != "nvidia-l4-vws:1" || len(bc.Installers) != 1 || bc.Installers[0].Name != "davinci-resolve" || !contains(bc.Toolchains, "parsec") {
		t.Fatalf("boot config: gpu=%q installers=%+v toolchains=%v", bc.GPU, bc.Installers, bc.Toolchains)
	}
	plain := EstimateCost("g2-standard-8", "nvidia-l4:1", false, 200, "", nil).Hourly(true)
	if res.RunningUSD <= plain {
		t.Fatalf("estimate should include the vWS license and Windows: %v <= %v", res.RunningUSD, plain)
	}
	// T4 attaches to N1 as its vWS variant.
	m.Cfg.GCP.WindowsInstallers = nil
	g.onCreate = func() { go h.connect("win-t4") }
	if _, err := m.Create(context.Background(), CreateOptions{Name: "win-t4", OS: OSWindows, GPU: "nvidia-tesla-t4:1"}); err != nil {
		t.Fatal(err)
	}
	args = g.called("compute instances create")[1]
	if !contains(args, "--machine-type=n1-standard-8") || !contains(args, "--accelerator=type=nvidia-tesla-t4-vws,count=1") || !contains(args, "--no-service-account") {
		t.Fatalf("t4 args: %v", args)
	}
	if _, err := m.Create(context.Background(), CreateOptions{Name: "win-a100", OS: OSWindows, GPU: "nvidia-tesla-a100:1"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("A100 has no vWS: %v", err)
	}
	// Linux GPUs are unchanged (bundled L4, no vWS).
	g.onCreate = func() { go h.connect("lin-gpu") }
	if _, err := m.Create(context.Background(), CreateOptions{Name: "lin-gpu", GPU: "nvidia-l4:1"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range g.called("compute instances create")[2] {
		if strings.HasPrefix(a, "--accelerator") {
			t.Fatalf("linux L4 should stay bundled: %s", a)
		}
	}
}

// The boot scripts have an install branch for every toolchain id, and the
// versioned ones download that exact version and check its checksum.
func TestBootScriptsInstallEveryToolchain(t *testing.T) {
	sh, ps := StartupScript, WindowsStartupScript
	for _, id := range toolchainOrder {
		if !windowsOnly[id] && !strings.Contains(sh, "\n    "+id+") ") {
			t.Errorf("startup.sh has no branch for %s", id)
		}
		if !strings.Contains(ps, "\n    '"+id+"' { ") {
			t.Errorf("startup.ps1 has no branch for %s", id)
		}
	}
	for name, s := range map[string]string{"startup.sh": sh, "startup.ps1": ps} {
		for _, want := range []string{"toolchains", "SHASUMS256.txt", "rustup-init", "corepack", "bun-v$ver", "nodejs.org/dist/$ver/", "static.rust-lang.org/rustup/dist/"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s lacks %q", name, want)
			}
		}
	}
	for _, want := range []string{
		// exact versions, checksum-verified
		`ver=v$want`, `check_sha "$RUN/$ntb" "$(sum_for "$RUN/node-SHASUMS256.txt" "$ntb")"`, `check_sha "$RUN/$a.zip"`,
		`check_sha "$RUN/rustup-init"`, `--default-toolchain "$ch"`, `corepack install -g "pnpm@$ver"`,
		// later boots retry; bake fails hard
		"if [ \"$MODE\" = node ] && [ -f \"$STATE/installed-$REL_VER\" ] && [ -f \"$CRED\" ]; then\n  install_toolchains",
		`[ "$MODE" != bake ] || die "toolchain $t failed`, "umask 022",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("startup.sh lacks %q", want)
		}
	}
	for _, want := range []string{
		`$ver = "v$want"`, `Assert-Sha256 $f (Sum-For (Fetch-Text "https://nodejs.org/dist/$ver/SHASUMS256.txt") $zip) $zip`,
		`$base = "https://github.com/oven-sh/bun/releases/download/bun-v$ver"`, `Assert-Sha256 $f (Sum-For (Fetch-Text "$base/SHASUMS256.txt") $zip)`,
		`'--default-host', $hostTriple, '--default-toolchain', $ch`, `Assert-Sha256 $f (((Fetch-Text "$u.sha256")`,
		`if ($ver) { $a += @('--version', $ver, '--allow-downgrade') }`, `'ADD_CMAKE_TO_PATH=System'`, `Set-MachineEnv 'LIBCLANG_PATH' $bin`,
		`Set-MachineEnv 'RUSTUP_HOME'`, `Set-MachineEnv 'COREPACK_ENABLE_DOWNLOAD_PROMPT' '0'`, `Prepend-MachinePath $dir "$root\*"`,
		`Assert-Signed $f '*O=Microsoft Corporation*' 'vs_buildtools.exe'`, `Assert-Signed $f '*O=Python Software Foundation*'`,
		`[Environment]::SetEnvironmentVariable('Path', ((@($dir) + $parts) -join ';'), 'Machine')`,
		// GPU: GRID driver from Google's bucket (MD5 + NVIDIA signature), RDP hardware graphics, reboot when installed later
		`nvidia-drivers-us-public`, `Assert-Signed $f '*O=NVIDIA Corporation*' 'GRID driver'`, `'bEnumerateHWBeforeSW' -Value 1`,
		"if ($NeedReboot) {", `PCI\VEN_10DE*`,
		// parsec, owner installers (private bucket, SHA-256, once per hash)
		`https://builds.parsec.app/package/parsec-windows.exe`, `Assert-Signed $f '*Parsec*'`,
		`Download ([string]$i.gcs_uri) $f`, `Assert-Sha256 $f $sha "installer $name"`, `installer-$name-$($sha.Substring(0, 16)).done`,
		// harness fallback and bake failure
		`if (((Want 'claude') -or (Want 'codex')) -and -not (Installed node)) { TC-Node 'lts' }`,
		`if ($Mode -eq 'bake') { Die "toolchain ${t}: $($_.Exception.Message)" }`,
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("startup.ps1 lacks %q", want)
		}
	}
	// Toolchains run before the later-boot exit (every boot) and before bake ends.
	tcAt, laterAt, bakeAt := strings.Index(ps, "foreach ($t in $Toolchains)"), strings.Index(ps, "# ---- later boots"), strings.Index(ps, "if ($Mode -eq 'bake') {\n")
	if tcAt < 0 || tcAt > laterAt || tcAt > bakeAt {
		t.Errorf("startup.ps1 toolchain loop at %d, later-boot exit at %d, bake at %d", tcAt, laterAt, bakeAt)
	}
	if strings.Contains(ps, "Latest-GitHubAsset") || strings.Contains(sh, "latest-v22.x") {
		t.Error("old unpinned install paths remain")
	}
}
