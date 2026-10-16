# Pyre cloud node bootstrap for Windows: the GCE windows-startup-script-ps1
# of every Windows VM made by `pyre node create --os windows` /
# `pyre node image bake --os windows`. Runs as SYSTEM on every boot and is
# idempotent; output goes to the serial console
# (gcloud compute instances get-serial-port-output <vm> --zone <z>).
#
# Settings come from the non-secret pyre-config metadata. Secrets (the
# single-use ephemeral Tailscale auth key and the one-time hub enrollment
# ticket) are read from metadata into files under a SYSTEM-only directory,
# passed by file or stdin (never argv, never printed) and deleted; the CLI
# removes the metadata keys once the node connected. The dev user's
# auto-logon password is generated here, stored only as the LSA secret
# Winlogon reads, and never leaves the VM.
#
# Pyre itself is a per-user install (scripts\install\install.ps1: state in
# the user's %LOCALAPPDATA%\Pyre, a Task Scheduler logon task for the user who
# runs it), so SYSTEM never runs install.ps1. Instead it verifies the release
# and stages the zip, SHA256SUMS(.sig), the allowed signers, install.ps1 and
# (first enrollment only) the ticket file in C:\ProgramData\pyre-install,
# ACL'd to the dev user and SYSTEM only, and registers the one-shot task
# "pyre-first-install": at the dev user's logon, as that user, interactive,
# non-elevated. It runs install.ps1 -EnrollTicketFile, deletes the ticket
# file when install.ps1 returns (success or failure), logs to
# %LOCALAPPDATA%\Pyre\first-install.log (no secrets) and unregisters itself.
# The first boot reboots into auto-logon, which fires it. A new release
# version takes the same path without a ticket (install.ps1 skips enrollment
# when the node credential exists) and starts the task at once when the user
# is already logged on. Bake mode only installs Tailscale, the toolchains and
# the owner's installers and verifies the release: no user, no task, no
# ticket. GPU nodes (the CLI attaches an NVIDIA RTX Virtual Workstation GPU)
# get the GRID driver and hardware graphics for Remote Desktop sessions.
#
# Remote Desktop (optional, rdp=true) is reachable only from tailnet
# addresses: no VPC firewall rule is created for 3389 (the pyre-deny-ingress
# rule blocks it from the internet) and the Windows firewall rules of the
# Remote Desktop group are limited to 100.64.0.0/10 and fd7a:115c:a1e0::/48.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Log([string]$m) { Write-Output "pyre-bootstrap: $m" }
function Die([string]$m) { Log "FAILED: $m"; exit 1 }

$MD = 'http://metadata.google.internal/computeMetadata/v1/instance'
function Get-Metadata([string]$path) {
  Invoke-RestMethod -Headers @{ 'Metadata-Flavor' = 'Google' } -Uri "$MD/$path" -UseBasicParsing
}
# MdSecret KEY FILE: a secret goes from metadata into FILE only.
function MdSecret([string]$key, [string]$file) {
  try {
    Invoke-WebRequest -Headers @{ 'Metadata-Flavor' = 'Google' } -Uri "$MD/attributes/$key" -UseBasicParsing -OutFile $file
    return $true
  } catch { return $false }
}

$Root = 'C:\ProgramData\pyre-bootstrap'
$Run = Join-Path $Root 'run'
New-Item -ItemType Directory -Force -Path $Run | Out-Null
# SYSTEM and Administrators only.
& icacls.exe $Root /inheritance:r /grant:r 'SYSTEM:(OI)(CI)F' 'Administrators:(OI)(CI)F' | Out-Null
Get-ChildItem $Run -Force -ErrorAction SilentlyContinue | Remove-Item -Force -Recurse -ErrorAction SilentlyContinue

try {
  $cfg = Get-Metadata 'attributes/pyre-config'
  if ($cfg -is [string]) { $cfg = $cfg | ConvertFrom-Json }
} catch { Die 'no pyre-config metadata' }
$Mode = if ($cfg.mode) { $cfg.mode } else { 'node' }
$User = if ($cfg.user) { $cfg.user } else { 'pyre' }
$Name = $cfg.name
$Ver = $cfg.release_version
$RelUrl = ([string]$cfg.release_url).TrimEnd('/')
$Harnesses = @($cfg.harnesses)
Log "mode=$Mode node=$Name user=$User release=$Ver toolchains=$(@($cfg.toolchains) -join ' ') gpu=$($cfg.gpu)"

function Download([string]$url, [string]$dest) {
  if ($url -like 'gs://*') {
    $rest = $url.Substring(5); $bucket = $rest.Split('/')[0]; $obj = $rest.Substring($bucket.Length + 1)
    $tok = (Get-Metadata 'service-accounts/default/token').access_token
    $enc = [Uri]::EscapeDataString($obj)
    Invoke-WebRequest -UseBasicParsing -Headers @{ Authorization = "Bearer $tok" } -OutFile $dest `
      -Uri "https://storage.googleapis.com/storage/v1/b/$bucket/o/$($enc)?alt=media"
  } elseif ($url -like 'https://*') {
    Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $dest
  } else { Die "release url must be gs:// or https://" }
}
function Installed([string]$cmd) { [bool](Get-Command $cmd -ErrorAction SilentlyContinue) }
function Refresh-Path {
  $env:Path = [Environment]::GetEnvironmentVariable('Path', 'Machine') + ';' + [Environment]::GetEnvironmentVariable('Path', 'User')
}
function Add-MachinePath([string]$dir) {
  $p = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  if (($p -split ';') -notcontains $dir) { [Environment]::SetEnvironmentVariable('Path', "$p;$dir", 'Machine') }
  Refresh-Path
}
function Run-Exe([string]$exe, [string[]]$argList) {
  $p = Start-Process -FilePath $exe -ArgumentList $argList -Wait -PassThru -NoNewWindow
  if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) { Die "$exe exited $($p.ExitCode)" }
}
function Want([string]$h) { $Harnesses -contains $h }

# ---- the dev user (auto-logon password generated here, LSA only) ----
$LsaSource = @'
using System;
using System.Runtime.InteropServices;
public static class PyreLsa {
  [StructLayout(LayoutKind.Sequential)] struct LSA_UNICODE_STRING { public ushort Length; public ushort MaximumLength; public IntPtr Buffer; }
  [StructLayout(LayoutKind.Sequential)] struct LSA_OBJECT_ATTRIBUTES { public int Length; public IntPtr RootDirectory; public IntPtr ObjectName; public int Attributes; public IntPtr SecurityDescriptor; public IntPtr SecurityQualityOfService; }
  [DllImport("advapi32.dll")] static extern uint LsaOpenPolicy(IntPtr system, ref LSA_OBJECT_ATTRIBUTES attrs, int access, out IntPtr handle);
  [DllImport("advapi32.dll")] static extern uint LsaStorePrivateData(IntPtr handle, ref LSA_UNICODE_STRING key, ref LSA_UNICODE_STRING data);
  [DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr handle);
  static LSA_UNICODE_STRING Str(string s) {
    var u = new LSA_UNICODE_STRING(); u.Buffer = Marshal.StringToHGlobalUni(s);
    u.Length = (ushort)(s.Length * 2); u.MaximumLength = (ushort)(s.Length * 2 + 2); return u;
  }
  // Store sets the Winlogon DefaultPassword LSA secret (as Sysinternals Autologon does).
  public static void Store(string secret) {
    var attrs = new LSA_OBJECT_ATTRIBUTES(); IntPtr h;
    uint st = LsaOpenPolicy(IntPtr.Zero, ref attrs, 0x00000020 | 0x00000004, out h);
    if (st != 0) throw new Exception("LsaOpenPolicy " + st);
    var k = Str("DefaultPassword"); var d = Str(secret);
    try { st = LsaStorePrivateData(h, ref k, ref d); if (st != 0) throw new Exception("LsaStorePrivateData " + st); }
    finally { Marshal.FreeHGlobal(k.Buffer); Marshal.FreeHGlobal(d.Buffer); LsaClose(h); }
  }
}
'@
Add-Type -Language CSharp -TypeDefinition $LsaSource
function New-Password {
  $bytes = New-Object byte[] 24
  [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
  'Py!' + [Convert]::ToBase64String($bytes).Replace('/', 'x').Replace('+', 'y').Replace('=', 'z') + '9a'
}
if ($Mode -eq 'node' -and -not (Get-LocalUser -Name $User -ErrorAction SilentlyContinue)) {
  $pw = New-Password
  $sec = ConvertTo-SecureString $pw -AsPlainText -Force
  New-LocalUser -Name $User -Password $sec -PasswordNeverExpires -AccountNeverExpires -Description 'Pyre node user' | Out-Null
  Add-LocalGroupMember -Group 'Administrators' -Member $User
  if ($cfg.rdp) { Add-LocalGroupMember -Group 'Remote Desktop Users' -Member $User -ErrorAction SilentlyContinue }
  [PyreLsa]::Store($pw)
  $pw = $null; $sec = $null
  $wl = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
  Set-ItemProperty $wl -Name AutoAdminLogon -Value '1'
  Set-ItemProperty $wl -Name DefaultUserName -Value $User
  Set-ItemProperty $wl -Name DefaultDomainName -Value $env:COMPUTERNAME
  Remove-ItemProperty $wl -Name DefaultPassword -ErrorAction SilentlyContinue  # never plaintext
  Log "created user $User with auto-logon (password in LSA only)"
}

# Auto-logon refresher: `pyre node rdp --reset-password` resets the user's
# password through gcloud and hands the new one over in the
# pyre-autologon-password metadata key for about a minute; this SYSTEM task
# polls the local metadata server and stores a new value as the LSA secret,
# so auto-logon keeps working.
$refresher = Join-Path $Root 'autologon-refresh.ps1'
$refresherBody = @'
$ErrorActionPreference = 'SilentlyContinue'
$u = 'http://metadata.google.internal/computeMetadata/v1/instance/attributes/pyre-autologon-password'
$last = ''
while ($true) {
  try {
    $r = Invoke-WebRequest -UseBasicParsing -Headers @{ 'Metadata-Flavor' = 'Google' } -Uri $u
    $etag = [string]$r.Headers['ETag']
    $pw = ([string]$r.Content).Trim()
    if ($pw -and $etag -ne $last) { [PyreLsa]::Store($pw); $last = $etag; Write-Output 'pyre: auto-logon password refreshed' }
    $pw = $null
  } catch {}
  Start-Sleep -Seconds 5
}
'@
Set-Content -Path $refresher -Encoding UTF8 -Value ("Add-Type -Language CSharp -TypeDefinition @'`r`n" + $LsaSource + "`r`n'@`r`n" + $refresherBody)

# ---- Tailscale (MSI; joined with the single-use key in node mode) ----
$ts = 'C:\Program Files\Tailscale\tailscale.exe'
if (-not (Test-Path $ts)) {
  $msi = Join-Path $Run 'tailscale.msi'
  Invoke-WebRequest -UseBasicParsing -Uri 'https://pkgs.tailscale.com/stable/tailscale-setup-latest-amd64.msi' -OutFile $msi
  Run-Exe 'msiexec.exe' @('/i', "`"$msi`"", '/quiet', '/norestart', 'TS_UNATTENDEDMODE=always')
  Log 'installed Tailscale'
}
# The node runs Tailscale unattended as SYSTEM only. The Tailscale tray app
# (tailscale-ipn) that Windows starts in the auto-logged-on dev user's session
# connects to tailscaled as that user and switches it to an empty per-user
# profile (NeedsLogin): the first real GCP node dropped off the tailnet about
# 20 s after every logon. Enforce unattended mode by policy and never start
# the tray app at logon.
New-Item -Force -Path 'HKLM:\SOFTWARE\Policies\Tailscale' | Out-Null
Set-ItemProperty -Path 'HKLM:\SOFTWARE\Policies\Tailscale' -Name 'UnattendedMode' -Value 'always'
foreach ($rk in 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run', 'HKLM:\SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Run') {
  $props = Get-ItemProperty -Path $rk -ErrorAction SilentlyContinue
  if ($props) {
    foreach ($n in ($props.PSObject.Properties | Where-Object { [string]$_.Value -match 'tailscale-ipn' } | ForEach-Object { $_.Name })) {
      Remove-ItemProperty -Path $rk -Name $n -ErrorAction SilentlyContinue
    }
  }
}
Remove-Item -Force -ErrorAction SilentlyContinue "$env:ProgramData\Microsoft\Windows\Start Menu\Programs\StartUp\Tailscale*.lnk"
Get-Process -Name 'tailscale-ipn' -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
if ($Mode -eq 'node') {
  $state = ''
  try { $state = ((& $ts status --json 2>$null) | ConvertFrom-Json).BackendState } catch {}
  if ($state -ne 'Running') {
    $keyFile = Join-Path $Run 'ts.key'
    if (-not (MdSecret 'pyre-ts-authkey' $keyFile)) { Die 'not on the tailnet and no pyre-ts-authkey in metadata (pyre node start supplies one)' }
    & $ts up --unattended "--auth-key=file:$keyFile" "--hostname=$($cfg.ts_hostname)"
    $rc = $LASTEXITCODE
    Remove-Item -Force $keyFile
    if ($rc -ne 0) { Die 'tailscale up failed (expired or used key?)' }
    Log "joined the tailnet as $($cfg.ts_hostname)"
  }
}

# ---- long paths (machine-wide; dev tools exceed MAX_PATH) ----
Set-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' -Name 'LongPathsEnabled' -Value 1 -Type DWord
if (Test-Path 'C:\Program Files\Git\cmd\git.exe') { & 'C:\Program Files\Git\cmd\git.exe' config --system core.longpaths true }

# ---- Remote Desktop: tailnet addresses only ----
$ts_ranges = @('100.64.0.0/10', 'fd7a:115c:a1e0::/48')
if ($cfg.rdp) {
  Set-ItemProperty 'HKLM:\System\CurrentControlSet\Control\Terminal Server' -Name fDenyTSConnections -Value 0
  Enable-NetFirewallRule -DisplayGroup 'Remote Desktop'
  Get-NetFirewallRule -DisplayGroup 'Remote Desktop' | Set-NetFirewallRule -RemoteAddress $ts_ranges
} else {
  Set-ItemProperty 'HKLM:\System\CurrentControlSet\Control\Terminal Server' -Name fDenyTSConnections -Value 1
  Disable-NetFirewallRule -DisplayGroup 'Remote Desktop' -ErrorAction SilentlyContinue
}

# ---- per-user install staging: the dev user and SYSTEM only ----
$Stage = 'C:\ProgramData\pyre-install'
$TaskName = 'pyre-first-install'
$UserLog = "C:\Users\$User\AppData\Local\Pyre\first-install.log"
# Report-FirstInstall: the outcome the task left for this release (serial
# console; the result file holds only "ok" or "failed exit N"). SYSTEM removes
# a finished task the user's own task could not unregister.
function Report-FirstInstall {
  $res = Join-Path $Stage "result-$Ver"
  if (Test-Path $res) {
    $r = (Get-Content -Raw $res).Trim()
    if ($r -eq 'ok') { Log "per-user install of $Ver done (log: $UserLog)" }
    else { Log "per-user install of $Ver FAILED ($r); see $UserLog" }
    if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
      Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
    }
  } else { Log "per-user install of $Ver pending: task $TaskName runs at $User logon" }
}

# ---- developer toolchains (gcp.toolchains / --toolchain, resolved by the CLI) ----
# Every boot, idempotent: an id is skipped when the requested version is
# present. Installs are machine-wide (machine PATH and environment), so the
# dev user's next logon sees them. Verification: Node and Bun zips against
# the vendor's SHASUMS256.txt, rustup-init.exe against its .sha256, Git and
# gh against the GitHub release asset digest, VS Build Tools, Python, Parsec
# and the NVIDIA driver by Authenticode publisher (the driver also against
# the bucket's MD5); CMake and LLVM come from Chocolatey community packages
# (which check the checksums their maintainers embed), as the CI installs
# them. Node mode: a failure is logged and the node comes up without that
# tool (the next boot retries); bake mode: it fails the bake.
$Toolchains = @($cfg.toolchains | Where-Object { $_ })
if ($null -eq $cfg.toolchains) { $Toolchains = @('git', 'node@lts', 'python', 'vs-buildtools') }
$Tools = 'C:\Program Files\PyreTools'
$Shared = 'C:\ProgramData\pyre'
$ChocoExe = 'C:\ProgramData\chocolatey\bin\choco.exe'
$NeedReboot = $false

# (Value-returning helpers never Log: Log writes to the output stream.)
function Fetch-Text([string]$url) {
  $f = Join-Path $Run ('fetch-' + [guid]::NewGuid().ToString('N'))
  Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $f
  $t = [IO.File]::ReadAllText($f)
  Remove-Item -Force $f
  $t
}
# Sum-For SUMS NAME: the SHA-256 a SHASUMS file lists for NAME.
function Sum-For([string]$sums, [string]$name) {
  foreach ($l in ($sums -split "`r?`n")) {
    $p = $l.Trim() -split '\s+', 2
    if ($p.Count -eq 2 -and $p[1].TrimStart('*') -eq $name) { return $p[0].ToLower() }
  }
  ''
}
function Assert-Sha256([string]$file, [string]$want, [string]$what) {
  if (-not $want) { throw "${what}: no published SHA-256 to check against" }
  $got = (Get-FileHash -Algorithm SHA256 $file).Hash.ToLower()
  if ($got -ne $want.Trim().ToLower()) { throw "${what}: SHA-256 mismatch" }
  Log "${what}: SHA-256 verified"
}
# Assert-Signed FILE SUBJECT-PATTERN WHAT: a valid Authenticode signature by the publisher.
function Assert-Signed([string]$file, [string]$subject, [string]$what) {
  $s = Get-AuthenticodeSignature -FilePath $file
  if ($s.Status -ne 'Valid' -or -not $s.SignerCertificate -or $s.SignerCertificate.Subject -notlike $subject) {
    throw "${what}: Authenticode signature not valid for $subject ($($s.Status))"
  }
  Log "${what}: Authenticode signature valid ($($s.SignerCertificate.Subject.Split(',')[0]))"
}
function Invoke-Checked([string]$exe, [string[]]$argList) {
  $sp = @{ FilePath = $exe; Wait = $true; PassThru = $true; NoNewWindow = $true }
  if ($argList -and $argList.Count -gt 0) { $sp.ArgumentList = $argList }
  $p = Start-Process @sp
  if ($p.ExitCode -notin @(0, 1641, 3010)) { throw "$(Split-Path -Leaf $exe) exited $($p.ExitCode)" }
}
# Native EXE ARGV: a console program, output to the serial log; throws on failure.
function Native([string]$exe, [string[]]$argv) {
  $ErrorActionPreference = 'Continue'
  & $exe @argv 2>&1 | ForEach-Object { Write-Output "  $_" }
  if ($LASTEXITCODE -notin @(0, 1641, 3010)) { throw "$(Split-Path -Leaf $exe) $($argv -join ' ') exited $LASTEXITCODE" }
}
function Tool-Version([string]$cmd, [string[]]$a) {
  if (-not (Installed $cmd)) { return '' }
  $ErrorActionPreference = 'Continue'
  try { ((& $cmd @a 2>$null) | Select-Object -First 1 | Out-String).Trim() } catch { '' }
}
function Set-MachineEnv([string]$name, [string]$value) {
  if ([Environment]::GetEnvironmentVariable($name, 'Machine') -ne $value) {
    [Environment]::SetEnvironmentVariable($name, $value, 'Machine')
    Log "machine environment: $name=$value (seen from the next logon)"
  }
  Set-Item -Path "env:$name" -Value $value
}
# Prepend-MachinePath DIR DROP: DIR first on the machine PATH; entries like DROP (other versions) removed.
function Prepend-MachinePath([string]$dir, [string]$drop) {
  $parts = @(([Environment]::GetEnvironmentVariable('Path', 'Machine') -split ';') |
      Where-Object { $_ -and $_ -ne $dir -and -not ($drop -and $_ -like $drop) })
  [Environment]::SetEnvironmentVariable('Path', ((@($dir) + $parts) -join ';'), 'Machine')
  Refresh-Path
}
# Shared-Dir DIR: a machine-wide cache the (non-elevated) dev user writes to.
function Shared-Dir([string]$dir) {
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  & icacls.exe $dir /grant '*S-1-5-32-545:(OI)(CI)M' /Q | Out-Null
}
function Expand-Zip([string]$zip, [string]$dest) {
  New-Item -ItemType Directory -Force -Path $dest | Out-Null
  & tar.exe -xf $zip -C $dest
  if ($LASTEXITCODE -ne 0) { Expand-Archive -Force -Path $zip -DestinationPath $dest }
}
function GitHub-Asset([string]$repo, [string]$pattern) {
  $rel = Invoke-RestMethod -UseBasicParsing -Uri "https://api.github.com/repos/$repo/releases/latest"
  $a = $rel.assets | Where-Object { $_.name -match $pattern } | Select-Object -First 1
  if (-not $a) { throw "no $pattern asset in the latest $repo release" }
  $a
}
function Fetch-GitHubAsset($a, [string]$dest) {
  Invoke-WebRequest -UseBasicParsing -Uri $a.browser_download_url -OutFile $dest
  if ([string]$a.digest -like 'sha256:*') { Assert-Sha256 $dest ([string]$a.digest).Substring(7) $a.name }
  else { Log "$($a.name): the release publishes no digest (TLS only)" }
}

function TC-Git {
  if (Installed git) { Log 'toolchain git: present'; return }
  $a = GitHub-Asset 'git-for-windows/git' '^Git-[0-9.]+-64-bit\.exe$'
  $f = Join-Path $Run 'git.exe'
  Fetch-GitHubAsset $a $f
  Invoke-Checked $f @('/VERYSILENT', '/NORESTART', '/NOCANCEL', '/SP-', '/SUPPRESSMSGBOXES')
  Refresh-Path; Log "toolchain git: installed $($a.name)"
}
function TC-Gh {
  if (Installed gh) { Log 'toolchain gh: present'; return }
  $a = GitHub-Asset 'cli/cli' '^gh_[0-9.]+_windows_amd64\.msi$'
  $f = Join-Path $Run 'gh.msi'
  Fetch-GitHubAsset $a $f
  Invoke-Checked 'msiexec.exe' @('/i', "`"$f`"", '/qn', '/norestart')
  Refresh-Path; Log "toolchain gh: installed $($a.name)"
}
function TC-Python([string]$ver) {
  if (-not $ver) {
    if (Test-Path 'C:\Program Files\Python3*\python.exe') { Log 'toolchain python: present'; return }
    $ver = '3.12.8'
  } else {
    $p = $ver.Split('.')
    $exe = "C:\Program Files\Python$($p[0])$($p[1])\python.exe"
    if ((Test-Path $exe) -and ((Tool-Version $exe @('--version')) -eq "Python $ver")) { Log "toolchain python@${ver}: present"; return }
  }
  $f = Join-Path $Run "python-$ver-amd64.exe"
  Invoke-WebRequest -UseBasicParsing -Uri "https://www.python.org/ftp/python/$ver/python-$ver-amd64.exe" -OutFile $f
  Assert-Signed $f '*O=Python Software Foundation*' "python-$ver-amd64.exe"
  Invoke-Checked $f @('/quiet', 'InstallAllUsers=1', 'PrependPath=1', 'Include_test=0')
  Refresh-Path; Log "toolchain python@${ver}: installed"
}
# The C++ workload plus the Spectre-mitigated libraries (Electron apps that
# build native modules with /Qspectre, e.g. the owner's app, check for
# VC\Tools\MSVC\<ver>\lib\spectre\x64). An existing install that lacks
# them is modified in place.
$VSComponents = @('Microsoft.VisualStudio.Workload.VCTools', 'Microsoft.VisualStudio.Component.VC.Runtimes.x86.x64.Spectre')
function TC-VSBuildTools {
  $bt = 'C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools'
  $spectre = @(Get-ChildItem -Path "$bt\VC\Tools\MSVC\*\lib\spectre\x64" -ErrorAction SilentlyContinue).Count -gt 0
  if ((Test-Path "$bt\VC") -and $spectre) { Log 'toolchain vs-buildtools: present (with Spectre libraries)'; return }
  $f = Join-Path $Run 'vs_buildtools.exe'
  Invoke-WebRequest -UseBasicParsing -Uri 'https://aka.ms/vs/17/release/vs_buildtools.exe' -OutFile $f
  Assert-Signed $f '*O=Microsoft Corporation*' 'vs_buildtools.exe'
  $add = @(); foreach ($c in $VSComponents) { $add += @('--add', $c) }
  if (Test-Path "$bt\VC") {
    # Modify through the installed Visual Studio Installer (the downloaded
    # bootstrapper's `modify` exited 1 on the first GCP node); on failure,
    # surface the installer's own log so the cause is visible.
    $setup = 'C:\Program Files (x86)\Microsoft Visual Studio\Installer\setup.exe'
    $exe = if (Test-Path $setup) { $setup } else { $f }
    $p = Start-Process -FilePath $exe -ArgumentList (@('modify', '--installPath', "`"$bt`"", '--quiet', '--norestart') + $add) -Wait -PassThru
    if ($p.ExitCode -ne 0 -and $p.ExitCode -ne 3010) {
      Get-ChildItem "$env:TEMP\dd_*.log", 'C:\Windows\Temp\dd_*.log' -ErrorAction SilentlyContinue | Sort-Object LastWriteTime | Select-Object -Last 2 | ForEach-Object {
        Log "vs installer log $($_.Name):"
        Get-Content $_.FullName -Tail 12 | ForEach-Object { Log "  $_" }
      }
      throw "Visual Studio Installer modify exited $($p.ExitCode)"
    }
    Log 'toolchain vs-buildtools: added the Spectre-mitigated libraries'
  } else {
    Invoke-Checked $f (@('--quiet', '--wait', '--norestart', '--nocache') + $add + @('--includeRecommended'))
    Log 'toolchain vs-buildtools: installed Visual Studio Build Tools 2022 (C++ workload, Windows SDK, Spectre libraries)'
  }
}
# TC-Node lts|MAJOR|X.Y.Z: the official zip into PyreTools\node, first on the machine PATH.
function TC-Node([string]$want) {
  if (-not $want) { $want = 'lts' }
  $have = (Tool-Version 'node' @('--version')).TrimStart('v')
  if ($have -and ($want -eq 'lts' -or $have -eq $want -or ($want -match '^\d+$' -and $have -like "$want.*"))) {
    Log "toolchain node@${want}: v$have present"; return
  }
  $ver = "v$want"
  if ($want -eq 'lts' -or $want -match '^\d+$') {
    $idx = Invoke-RestMethod -UseBasicParsing -Uri 'https://nodejs.org/dist/index.json'
    if ($want -eq 'lts') { $ver = ($idx | Where-Object { $_.lts } | Select-Object -First 1).version }
    else { $ver = ($idx | Where-Object { $_.version -like "v$want.*" } | Select-Object -First 1).version }
    if (-not $ver) { throw "node@${want}: no such release" }
  }
  $zip = "node-$ver-win-x64.zip"
  $f = Join-Path $Run $zip
  Invoke-WebRequest -UseBasicParsing -Uri "https://nodejs.org/dist/$ver/$zip" -OutFile $f
  Assert-Sha256 $f (Sum-For (Fetch-Text "https://nodejs.org/dist/$ver/SHASUMS256.txt") $zip) $zip
  $root = Join-Path $Tools 'node'
  $dir = Join-Path $root "node-$ver-win-x64"
  if (Test-Path $dir) { Remove-Item -Recurse -Force $dir }
  Expand-Zip $f $root
  Remove-Item -Force $f
  if (-not (Test-Path (Join-Path $dir 'node.exe'))) { throw "node: $zip has no node.exe" }
  Prepend-MachinePath $dir "$root\*"
  Log "toolchain node@${want}: installed $ver ($dir, first on the machine PATH)"
}
# TC-Pnpm [X.Y.Z]: corepack (a repo's packageManager field picks the pnpm version).
function TC-Pnpm([string]$ver) {
  if (-not (Installed node)) { TC-Node 'lts' }
  $ch = Join-Path $Shared 'corepack'
  Shared-Dir $ch
  Set-MachineEnv 'COREPACK_HOME' $ch
  Set-MachineEnv 'COREPACK_ENABLE_DOWNLOAD_PROMPT' '0'
  $nodeDir = Split-Path -Parent (Get-Command node | Select-Object -First 1).Source
  if (-not (Test-Path (Join-Path $nodeDir 'corepack.cmd'))) {
    Native (Join-Path $nodeDir 'npm.cmd') @('install', '-g', '-q', '--prefix', $nodeDir, 'corepack') # Node 25+ no longer bundles it
  }
  if (Test-Path (Join-Path $nodeDir 'pnpm.cmd')) { Log "toolchain pnpm: corepack shims present in $nodeDir" }
  else {
    Native (Join-Path $nodeDir 'corepack.cmd') @('enable', 'pnpm')
    Log "toolchain pnpm: corepack enabled in $nodeDir (the repo's packageManager field picks the version)"
  }
  if ($ver) {
    if (Test-Path (Join-Path $ch "v1\pnpm\$ver")) { Log "toolchain pnpm@${ver}: present" }
    else {
      Native (Join-Path $nodeDir 'corepack.cmd') @('install', '-g', "pnpm@$ver")
      Log "toolchain pnpm@${ver}: prepared (the default outside a repo with packageManager)"
    }
  }
}
# TC-Bun [X.Y.Z]: the official release zip, checked against its SHASUMS256.txt.
function TC-Bun([string]$ver) {
  $have = Tool-Version 'bun' @('--version')
  if ($have -and (-not $ver -or $have -eq $ver)) { Log "toolchain bun: $have present"; return }
  if (-not $ver) { $ver = ([string](Invoke-RestMethod -UseBasicParsing -Uri 'https://api.github.com/repos/oven-sh/bun/releases/latest').tag_name) -replace '^bun-v', '' }
  $base = "https://github.com/oven-sh/bun/releases/download/bun-v$ver"
  $zip = 'bun-windows-x64.zip'
  $f = Join-Path $Run $zip
  Invoke-WebRequest -UseBasicParsing -Uri "$base/$zip" -OutFile $f
  Assert-Sha256 $f (Sum-For (Fetch-Text "$base/SHASUMS256.txt") $zip) "bun $ver $zip"
  $x = Join-Path $Run 'bun-x'
  Expand-Zip $f $x
  $exe = Get-ChildItem -Recurse -Path $x -Filter bun.exe | Select-Object -First 1
  if (-not $exe) { throw "bun: $zip has no bun.exe" }
  $root = Join-Path $Tools 'bun'
  $dir = Join-Path $root $ver
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  Copy-Item -Force $exe.FullName (Join-Path $dir 'bun.exe')
  Copy-Item -Force $exe.FullName (Join-Path $dir 'bunx.exe')
  Remove-Item -Recurse -Force $x, $f
  Prepend-MachinePath $dir "$root\*"
  Log "toolchain bun@${ver}: installed ($dir)"
}
# TC-Rust [CHANNEL]: machine-wide rustup (RUSTUP_HOME/CARGO_HOME under
# ProgramData, writable by the dev user), MSVC host.
function TC-Rust([string]$ch) {
  if (-not $ch) { $ch = 'stable' }
  $rr = Join-Path $Shared 'rust'
  Shared-Dir $rr
  Set-MachineEnv 'RUSTUP_HOME' (Join-Path $rr 'rustup')
  Set-MachineEnv 'CARGO_HOME' (Join-Path $rr 'cargo')
  $bin = Join-Path $rr 'cargo\bin'
  Add-MachinePath $bin
  $hostTriple = 'x86_64-pc-windows-msvc'
  $rustup = Join-Path $bin 'rustup.exe'
  if (Test-Path $rustup) {
    $have = (& $rustup toolchain list) -join "`n"
    if ($have -match "(?m)^$([regex]::Escape($ch))-$hostTriple") { Log "toolchain rust@${ch}: present" }
    else { Native $rustup @('toolchain', 'install', $ch, '--profile', 'default'); Log "toolchain rust@${ch}: installed" }
    Native $rustup @('default', "$ch-$hostTriple")
    return
  }
  $u = "https://static.rust-lang.org/rustup/dist/$hostTriple/rustup-init.exe"
  $f = Join-Path $Run 'rustup-init.exe'
  Invoke-WebRequest -UseBasicParsing -Uri $u -OutFile $f
  Assert-Sha256 $f (((Fetch-Text "$u.sha256").Trim() -split '\s+')[0]) 'rustup-init.exe'
  Invoke-Checked $f @('-y', '--no-modify-path', '--profile', 'default', '--default-host', $hostTriple, '--default-toolchain', $ch)
  Log "toolchain rust@${ch}: installed ($hostTriple, $rr)"
}
function Ensure-Choco {
  if (Test-Path $ChocoExe) { return }
  $f = Join-Path $Run 'chocolatey-install.ps1'
  Invoke-WebRequest -UseBasicParsing -Uri 'https://community.chocolatey.org/install.ps1' -OutFile $f
  Native 'powershell.exe' @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $f)
  if (-not (Test-Path $ChocoExe)) { throw 'Chocolatey install failed' }
  Log 'installed Chocolatey (community.chocolatey.org)'
}
# TC-Choco ID PACKAGE VERSION HAVE-VERSION [INSTALL-ARGS]: a Chocolatey package (as the CI does).
function TC-Choco([string]$id, [string]$pkg, [string]$ver, [string]$have, [string]$installArgs) {
  if ($have -and (-not $ver -or $have -eq $ver)) { Log "toolchain ${id}: $have present"; return }
  Ensure-Choco
  $a = @('install', $pkg, '-y', '--no-progress', '--limit-output')
  if ($ver) { $a += @('--version', $ver, '--allow-downgrade') }
  if ($installArgs) { $a += @('--install-arguments', $installArgs) }
  Native $ChocoExe $a
  Log "toolchain ${id}: installed (chocolatey $pkg$(if ($ver) { " $ver" }))"
}
function TC-Cmake([string]$ver) {
  $exe = 'C:\Program Files\CMake\bin\cmake.exe'
  $have = ''
  if (Test-Path $exe) { $have = (Tool-Version $exe @('--version')) -replace '^cmake version\s+', '' }
  TC-Choco 'cmake' 'cmake' $ver $have 'ADD_CMAKE_TO_PATH=System'
  Add-MachinePath (Split-Path -Parent $exe)
}
function TC-Llvm([string]$ver) {
  $bin = 'C:\Program Files\LLVM\bin'
  $have = ''
  if ((Test-Path "$bin\clang.exe") -and (Test-Path "$bin\libclang.dll")) {
    $have = (Tool-Version "$bin\clang.exe" @('--version')) -replace '^.*clang version ([0-9.]+).*$', '$1'
  }
  TC-Choco 'llvm' 'llvm' $ver $have ''
  Add-MachinePath $bin
  Set-MachineEnv 'LIBCLANG_PATH' $bin # bindgen / clang-sys
}
# TC-Parsec: the Parsec host for all users (signed by Parsec); the owner signs in once over RDP.
function TC-Parsec {
  if ((Test-Path 'C:\Program Files\Parsec\parsecd.exe') -or (Test-Path 'C:\Program Files\Parsec\parsec.exe')) { Log 'toolchain parsec: present'; return }
  $f = Join-Path $Run 'parsec-windows.exe'
  Invoke-WebRequest -UseBasicParsing -Uri 'https://builds.parsec.app/package/parsec-windows.exe' -OutFile $f
  Assert-Signed $f '*Parsec*' 'parsec-windows.exe'
  Invoke-Checked $f @('/silent', '/shared')
  Log 'toolchain parsec: installed the Parsec host (sign in once as the dev user over RDP; it then reaches the node through Tailscale)'
}
function Install-Toolchain([string]$id) {
  $name, $ver = $id.Split('@', 2)
  switch ($name) {
    'git' { TC-Git }
    'python' { TC-Python $ver }
    'vs-buildtools' { TC-VSBuildTools }
    'node' { TC-Node $ver }
    'pnpm' { TC-Pnpm $ver }
    'bun' { TC-Bun $ver }
    'rust' { TC-Rust $ver }
    'cmake' { TC-Cmake $ver }
    'llvm' { TC-Llvm $ver }
    'gh' { TC-Gh }
    'parsec' { TC-Parsec }
    default { throw "unknown toolchain id $id" }
  }
}

# ---- GPU (NVIDIA RTX Virtual Workstation): GRID driver, hardware graphics over RDP ----
# The CLI attaches the -vws accelerator (it carries the GRID license). The
# driver comes from Google's public driver bucket (newest GRID driver for
# Windows Server, or gcp.windows_gpu_driver), checked against the object's
# MD5 in the bucket and NVIDIA's Authenticode signature.
function Gcs-Md5([string]$url) {
  $h = (Invoke-WebRequest -UseBasicParsing -Method Head -Uri $url).Headers['x-goog-hash']
  if (([string]($h -join ',')) -match 'md5=([^,\s]+)') { return $matches[1] }
  ''
}
function File-Md5Base64([string]$file) {
  $md5 = [Security.Cryptography.MD5]::Create()
  $s = [IO.File]::OpenRead($file)
  try { [Convert]::ToBase64String($md5.ComputeHash($s)) } finally { $s.Dispose() }
}
function Grid-DriverUrl {
  $u = [string]$cfg.gpu_driver
  if ($u -like 'gs://*') { return 'https://storage.googleapis.com/' + $u.Substring(5) }
  if ($u) { return $u }
  $best = ''; $bestVer = [version]'0.0'; $tok = ''
  do {
    $q = 'https://storage.googleapis.com/storage/v1/b/nvidia-drivers-us-public/o?prefix=GRID/&fields=items(name),nextPageToken'
    if ($tok) { $q += '&pageToken=' + [Uri]::EscapeDataString($tok) }
    $r = Invoke-RestMethod -UseBasicParsing -Uri $q
    foreach ($it in @($r.items)) {
      if ([string]$it.name -match '^GRID/vGPU([0-9]+(\.[0-9]+)?)/[^/]*_grid_win[^/]*server20[0-9][0-9][^/]*\.exe$') {
        $v = $matches[1]; if ($v -notlike '*.*') { $v += '.0' }
        if ([version]$v -gt $bestVer) { $bestVer = [version]$v; $best = [string]$it.name }
      }
    }
    $tok = [string]$r.nextPageToken
  } while ($tok)
  if (-not $best) { throw 'no GRID driver for Windows Server in gs://nvidia-drivers-us-public/GRID/ (set gcp.windows_gpu_driver)' }
  'https://storage.googleapis.com/nvidia-drivers-us-public/' + $best
}
function Install-GridDriver {
  $smi = @("$env:WINDIR\System32\nvidia-smi.exe", 'C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe')
  if ($smi | Where-Object { Test-Path $_ }) { Log 'GPU: NVIDIA driver present'; return }
  $url = Grid-DriverUrl
  $f = Join-Path $Run 'nvidia-grid-driver.exe'
  Log "GPU: downloading $url"
  Invoke-WebRequest -UseBasicParsing -Uri $url -OutFile $f
  if ($url -like 'https://storage.googleapis.com/*') {
    $want = Gcs-Md5 $url
    if (-not $want -or (File-Md5Base64 $f) -ne $want) { throw 'GRID driver: MD5 does not match the bucket object' }
    Log 'GPU: driver MD5 matches the bucket object'
  }
  Assert-Signed $f '*O=NVIDIA Corporation*' 'GRID driver'
  $p = Start-Process -FilePath $f -ArgumentList @('-s', '-noreboot', '-noeula') -Wait -PassThru
  Remove-Item -Force $f -ErrorAction SilentlyContinue
  if (-not ($smi | Where-Object { Test-Path $_ })) { throw "GRID driver setup exited $($p.ExitCode) and nvidia-smi is missing" }
  $script:NeedReboot = $true
  Log "GPU: NVIDIA GRID driver installed (setup exit $($p.ExitCode)); a reboot activates it"
}
function Set-RdpHardwareGraphics {
  # Group Policy "Use hardware graphics adapters for all Remote Desktop
  # Services sessions", plus H.264/AVC 444 with hardware encoding.
  $k = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows NT\Terminal Services'
  if (-not (Test-Path $k)) { New-Item -Force -Path $k | Out-Null }
  Set-ItemProperty -Path $k -Name 'bEnumerateHWBeforeSW' -Value 1 -Type DWord
  Set-ItemProperty -Path $k -Name 'AVC444ModePreferred' -Value 1 -Type DWord
  Set-ItemProperty -Path $k -Name 'AVCHardwareEncodePreferred' -Value 1 -Type DWord
  Log 'GPU: Remote Desktop sessions use the hardware graphics adapter (AVC 444, hardware encode)'
}

# ---- owner installers (gcp.windows_installers): private bucket, SHA-256, silent ----
function Run-Installers {
  foreach ($i in @($cfg.installers | Where-Object { $_ })) {
    $name = [string]$i.name
    $sha = ([string]$i.sha256).ToLower()
    $mark = Join-Path $Root "installer-$name-$($sha.Substring(0, 16)).done"
    if (Test-Path $mark) { Log "installer ${name}: done earlier"; continue }
    $leaf = ([string]$i.gcs_uri).Split('/')[-1]
    $f = Join-Path $Run $leaf
    try {
      Download ([string]$i.gcs_uri) $f
      Assert-Sha256 $f $sha "installer $name"
      $ia = @($i.args | Where-Object { $null -ne $_ } | ForEach-Object { [string]$_ })
      if ($leaf -like '*.msi') { Invoke-Checked 'msiexec.exe' (@('/i', "`"$f`"", '/qn', '/norestart') + $ia) }
      else { Invoke-Checked $f $ia }
      New-Item -ItemType File -Force -Path $mark | Out-Null
      Log "installer ${name}: installed"
    } catch {
      if ($Mode -eq 'bake') { Die "installer ${name}: $($_.Exception.Message)" }
      Log "WARNING: installer ${name} failed: $($_.Exception.Message) (the next boot retries)"
    } finally {
      Remove-Item -Force $f -ErrorAction SilentlyContinue
    }
  }
}

$tcFailed = @()
foreach ($t in $Toolchains) {
  try { Install-Toolchain ([string]$t) }
  catch {
    if ($Mode -eq 'bake') { Die "toolchain ${t}: $($_.Exception.Message)" }
    Log "WARNING: toolchain $t failed: $($_.Exception.Message) (the node comes up without it; the next boot retries)"
    $tcFailed += [string]$t
  }
}
Log "toolchains: $($Toolchains -join ' ')$(if ($tcFailed.Count) { " (failed: $($tcFailed -join ' '))" }); machine PATH and environment apply from the next logon"
$hasNvidia = [bool](Get-PnpDevice -PresentOnly -ErrorAction SilentlyContinue | Where-Object { $_.InstanceId -like 'PCI\VEN_10DE*' })
if ($cfg.gpu -or $hasNvidia) {
  if ($hasNvidia) {
    try { Install-GridDriver }
    catch {
      if ($Mode -eq 'bake') { Die "GPU driver: $($_.Exception.Message)" }
      Log "WARNING: GPU driver failed: $($_.Exception.Message) (the next boot retries)"
    }
  } else { Log "WARNING: gpu=$($cfg.gpu) configured but no NVIDIA device is present" }
  Set-RdpHardwareGraphics
}
Run-Installers

# ---- later boots: this release is staged; the tasks do the rest ----
if ($Mode -eq 'node' -and (Test-Path (Join-Path $Root "staged-$Ver"))) {
  if ($NeedReboot) {
    Log 'restarting to activate the GPU driver'
    Restart-Computer -Force
    exit 0
  }
  Start-ScheduledTask -TaskName 'PyreAutologonRefresh' -ErrorAction SilentlyContinue
  Report-FirstInstall
  Log "node $Name ready (release $Ver staged; pyred runs at $User logon)"
  exit 0
}

# Claude Code and Codex: machine-wide npm prefix on the machine PATH (node
# LTS when no toolchain installed one).
if (((Want 'claude') -or (Want 'codex')) -and -not (Installed node)) { TC-Node 'lts' }
$npmPrefix = 'C:\ProgramData\pyre\npm'
New-Item -ItemType Directory -Force -Path $npmPrefix | Out-Null
Add-MachinePath $npmPrefix
$pkgs = @()
# A harness counts as installed only when it runs: npm 11+ skips install
# scripts it was not told to allow, leaving Claude Code without its binary.
function Harness-Works([string]$cmd) {
  $exe = Join-Path $npmPrefix "$cmd.cmd"
  if (-not (Test-Path $exe)) { return $false }
  & cmd.exe /d /c "`"$exe`" --version >nul 2>&1"
  return ($LASTEXITCODE -eq 0)
}
if ((Want 'claude') -and -not (Harness-Works 'claude')) { $pkgs += '@anthropic-ai/claude-code' }
if ((Want 'codex') -and -not (Harness-Works 'codex')) { $pkgs += '@openai/codex' }
if ($pkgs.Count -gt 0) {
  & npm.cmd install -g -q --prefix $npmPrefix --allow-scripts=@anthropic-ai/claude-code,@openai/codex @pkgs
  if ($LASTEXITCODE -ne 0) { Die "npm install $pkgs failed" }
  Log "installed $($pkgs -join ' ')"
}

# ---- fetch and verify the signed Windows release ----
$zipName = "pyre-$Ver-windows-amd64.zip"
$rel = Join-Path $Root "release\$Ver"
New-Item -ItemType Directory -Force -Path $rel | Out-Null
Download "$RelUrl/$zipName" (Join-Path $rel $zipName)
Download "$RelUrl/SHA256SUMS" (Join-Path $rel 'SHA256SUMS')
$signers = Join-Path $Root 'release-allowed-signers'
Set-Content -Path $signers -Value ([string]$cfg.allowed_signers) -Encoding ascii
$sigOK = $false
try { Download "$RelUrl/SHA256SUMS.sig" (Join-Path $rel 'SHA256SUMS.sig'); $sigOK = $true } catch {}
if ($sigOK -and ([string]$cfg.allowed_signers).Trim()) {
  $kg = 'C:\Windows\System32\OpenSSH\ssh-keygen.exe'
  if (-not (Test-Path $kg)) { Die 'ssh-keygen (Windows OpenSSH client) is required to verify the release signature' }
  # cmd.exe redirection feeds the exact bytes: a PowerShell pipe into a
  # native program re-encodes the text and the signature no longer matches.
  $sums = Join-Path $rel 'SHA256SUMS'; $sigf = Join-Path $rel 'SHA256SUMS.sig'
  $out = & cmd.exe /d /c "`"$kg`" -Y verify -f `"$signers`" -I pyre-release -n pyre-release -s `"$sigf`" < `"$sums`" 2>&1"
  if ($LASTEXITCODE -ne 0) { Write-Output $out; Die 'release signature verification FAILED' }
  Log "release signature verified: $($out | Select-Object -First 1)"
} elseif (-not $cfg.allow_unsigned) { Die 'release signature not verifiable: refusing an unverified release' }
$want = (Get-Content (Join-Path $rel 'SHA256SUMS') | Where-Object { $_ -match "\s\*?$([regex]::Escape($zipName))$" } | Select-Object -First 1)
if (-not $want) { Die "$zipName is not listed in SHA256SUMS" }
if ((Get-FileHash -Algorithm SHA256 (Join-Path $rel $zipName)).Hash.ToLower() -ne $want.Split(' ')[0].ToLower()) { Die 'zip checksum mismatch' }
Log 'release zip checksum verified'

if ($Mode -eq 'bake') {
  # Toolchains installed and release verified; Pyre itself is installed per
  # user on the node made from the image (no user, task or ticket here).
  Log 'bake complete; generalizing (GCESysprep) and shutting down'
  Get-ChildItem $Run -Force | Remove-Item -Force -Recurse -ErrorAction SilentlyContinue
  & GCESysprep
  exit 0
}

# ---- stage for the dev user (ACL: that user + SYSTEM, nobody else) ----
$userSid = (Get-LocalUser -Name $User).SID.Value
New-Item -ItemType Directory -Force -Path $Stage | Out-Null
& icacls.exe $Stage /inheritance:r /grant:r "*$($userSid):(OI)(CI)F" '*S-1-5-18:(OI)(CI)F' /Q | Out-Null
if ($LASTEXITCODE -ne 0) { Die "icacls $Stage failed" }
$ticketFile = Join-Path $Stage 'enroll-ticket.json'
# Drop the previous release's files; keep a pending ticket and the markers.
Get-ChildItem $Stage -Force | Where-Object { $_.Name -notin @('enroll-ticket.json', 'enrolled') -and $_.Name -notlike 'result-*' } |
  Remove-Item -Force -Recurse -ErrorAction SilentlyContinue
$sRel = Join-Path $Stage 'release'
New-Item -ItemType Directory -Force -Path $sRel | Out-Null
foreach ($f in @($zipName, 'SHA256SUMS', 'SHA256SUMS.sig')) {
  if (Test-Path (Join-Path $rel $f)) { Copy-Item -Force (Join-Path $rel $f) (Join-Path $sRel $f) }
}
$sSigners = ''
if (([string]$cfg.allowed_signers).Trim()) { $sSigners = Join-Path $Stage 'allowed-signers'; Copy-Item -Force $signers $sSigners }
$ex = Join-Path $Stage 'x'
Expand-Archive -Force -Path (Join-Path $sRel $zipName) -DestinationPath $ex
$inst = Get-ChildItem -Recurse -Path $ex -Filter install.ps1 | Where-Object { $_.FullName -like '*scripts\install\install.ps1' } | Select-Object -First 1
if (-not $inst) { Die 'release has no scripts\install\install.ps1' }
$utf8 = New-Object System.Text.UTF8Encoding($false)
# Non-secret install arguments for the task.
$ic = @{ version = $Ver; zip = (Join-Path $sRel $zipName); signers = $sSigners; install_ps1 = $inst.FullName
  name = $Name; hub_url = [string]$cfg.hub_url
  allow_unsigned = ([bool]$cfg.allow_unsigned -and -not ($sigOK -and $sSigners)) } | ConvertTo-Json -Compress
[IO.File]::WriteAllText((Join-Path $Stage 'install.json'), $ic, $utf8)
# --permissions bypass in gcp.pyred_args is the owner's consent for this
# Pyre-made node: record it as the dev user's acceptance of Claude's
# bypass-permissions warning (pyred refuses bypass launches without it).
$cfgArgs = @($cfg.pyred_args)
$bypass = $false
for ($i = 0; $i -lt $cfgArgs.Count - 1; $i++) { if ($cfgArgs[$i] -eq '--permissions' -and $cfgArgs[$i + 1] -eq 'bypass') { $bypass = $true } }
if ($bypass) {
  $cd = "C:\Users\$User\.claude"
  New-Item -ItemType Directory -Force -Path $cd | Out-Null
  $sf = Join-Path $cd 'settings.json'
  $sj = [ordered]@{}
  if (Test-Path $sf) { try { (Get-Content -Raw $sf | ConvertFrom-Json).PSObject.Properties | ForEach-Object { $sj[$_.Name] = $_.Value } } catch {} }
  if ($sj['skipDangerousModePermissionPrompt'] -ne $true) {
    $sj['skipDangerousModePermissionPrompt'] = $true
    [IO.File]::WriteAllText($sf, ($sj | ConvertTo-Json -Depth 20), $utf8)
    & icacls.exe $cd /grant "${User}:(OI)(CI)F" /T | Out-Null
    Log "recorded the bypass-permissions acceptance for $User (gcp.pyred_args has --permissions bypass)"
  }
}
# Extra pyred run arguments (gcp.pyred_args), e.g. --trust-dir/--permissions.
$pa = @($cfg.pyred_args | Where-Object { $_ })
if ($pa.Count -gt 0) { [IO.File]::WriteAllText((Join-Path $Stage 'pyred-args.json'), (ConvertTo-Json -InputObject @($pa) -Compress), $utf8) }
else { Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $Stage 'pyred-args.json') }

# The enrollment ticket (first enrollment only): metadata secret -> SYSTEM-only
# file -> ticket file in the staging dir, never argv or the log. The task
# deletes it once install.ps1 returns; the CLI deletes the metadata key once
# the node connected.
$credFile = "C:\Users\$User\AppData\Local\Pyre\node-credential.json"
$enrolled = (Test-Path (Join-Path $Stage 'enrolled')) -or (Test-Path $credFile)
if (-not $enrolled -and -not (Test-Path $ticketFile)) {
  $tf = Join-Path $Run 'enroll.secret'
  if (-not (MdSecret 'pyre-enroll-secret' $tf)) { Die 'no pyre-enroll-secret in metadata (expired? re-create the node)' }
  $ticket = @{ node_id = $cfg.node_id; secret = (Get-Content -Raw $tf).Trim(); hub_url = $cfg.hub_url; hub_cert_sha256 = $cfg.hub_pin } | ConvertTo-Json -Compress
  Remove-Item -Force $tf
  [IO.File]::WriteAllText($ticketFile, $ticket, $utf8)
  $ticket = $null
  Log 'enrollment ticket staged for the dev user (contents not logged)'
} elseif ($enrolled) {
  Remove-Item -Force $ticketFile -ErrorAction SilentlyContinue
}

# The task body: runs as the dev user, so install.ps1 installs into that
# user's profile and registers pyred's logon task for that user.
$firstInstall = @'
# pyre-first-install: one-shot, at the dev user's logon, as that user
# (written by the cloud bootstrap, internal/cloud/startup.ps1).
$ErrorActionPreference = 'Continue'
$Stage = Split-Path -Parent $MyInvocation.MyCommand.Path
$TaskName = 'pyre-first-install'
$ticket = Join-Path $Stage 'enroll-ticket.json'
$state = Join-Path $env:LOCALAPPDATA 'Pyre'
New-Item -ItemType Directory -Force -Path $state | Out-Null
$log = Join-Path $state 'first-install.log'
function L([string]$m) { "$(Get-Date -Format o) pyre-first-install: $m" | Out-File -Append -Encoding utf8 -FilePath $log }
$c = $null; $rc = 1
try {
  $c = Get-Content -Raw (Join-Path $Stage 'install.json') | ConvertFrom-Json
  if (Test-Path (Join-Path $Stage "result-$($c.version)")) { L "release $($c.version) already handled"; $rc = -1 }
  else {
    $a = @('-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', $c.install_ps1, '-Zip', $c.zip, '-Service', 'node', '-Restart', '-EditPath')
    if ($c.signers) { $a += @('-AllowedSigners', $c.signers) }
    if ($c.allow_unsigned) { $a += '-AllowUnsigned' }
    if ($c.name) { $a += @('-Name', $c.name) }
    if ($c.hub_url) { $a += @('-HubUrl', $c.hub_url) }
    $paf = Join-Path (Split-Path -Parent $ticket) 'pyred-args.json'
    if (Test-Path -LiteralPath $paf) { $a += @('-PyredArgsFile', $paf) }
    if (Test-Path -LiteralPath $ticket) { $a += @('-EnrollTicketFile', $ticket); L 'enrollment ticket present (contents not logged)' }
    L "install.ps1 -Zip $($c.zip) as $env:USERDOMAIN\$env:USERNAME"
    & powershell.exe @a 2>&1 | ForEach-Object { "$_" } | Out-File -Append -Encoding utf8 -FilePath $log
    $rc = $LASTEXITCODE
    L "install.ps1 exit $rc"
  }
} catch {
  L "error: $($_.Exception.Message)"
} finally {
  # The ticket is single-use: gone once install.ps1 returned, success or failure.
  Remove-Item -Force -LiteralPath $ticket -ErrorAction SilentlyContinue
  if (Test-Path -LiteralPath $ticket) { L "WARNING: could not delete $ticket" }
  if ($null -ne $c -and $rc -ne -1) {
    $res = if ($rc -eq 0) { 'ok' } else { "failed exit $rc" }
    Set-Content -Path (Join-Path $Stage "result-$($c.version)") -Value $res -Encoding ascii
  }
  if (Test-Path (Join-Path $state 'node-credential.json')) { New-Item -ItemType File -Force -Path (Join-Path $Stage 'enrolled') | Out-Null }
  try { Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction Stop; L "unregistered task $TaskName" }
  catch { L "could not unregister task $TaskName ($($_.Exception.Message)); the bootstrap removes it at the next boot" }
}
'@
Set-Content -Path (Join-Path $Stage 'first-install.ps1') -Encoding UTF8 -Value $firstInstall
Remove-Item -Force (Join-Path $Stage "result-$Ver") -ErrorAction SilentlyContinue

# One-shot logon task: as the dev user, interactive token, not elevated (the
# install and pyred's own task must belong to the user's normal token).
$uid = "$env:COMPUTERNAME\$User"
$pFirst = New-ScheduledTaskPrincipal -UserId $uid -LogonType Interactive -RunLevel Limited
$tFirst = New-ScheduledTaskTrigger -AtLogOn -User $uid
$aFirst = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$Stage\first-install.ps1`""
$sFirst = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Hours 1)
Register-ScheduledTask -TaskName $TaskName -Principal $pFirst -Trigger $tFirst -Action $aFirst -Settings $sFirst -Force | Out-Null
# Let the (non-elevated) dev user's task unregister itself.
try {
  $tsvc = New-Object -ComObject Schedule.Service; $tsvc.Connect()
  $tsvc.GetFolder('\').GetTask($TaskName).SetSecurityDescriptor("D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;$userSid)", 0)
} catch { Log "could not let $User delete task $TaskName (SYSTEM removes it at the next boot): $($_.Exception.Message)" }
Log "registered task $TaskName (at $User logon; install log $UserLog)"
# Already logged on (a new release on a running node): run it now.
$loggedOn = @(Get-CimInstance Win32_Process -Filter "Name='explorer.exe'" -ErrorAction SilentlyContinue |
  ForEach-Object { (Invoke-CimMethod -InputObject $_ -MethodName GetOwner).User }) -contains $User
if ($loggedOn) {
  Start-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  Log "$User is logged on: started $TaskName"
}

# ---- scheduled tasks: auto-logon refresher, idle auto-stop ----
$idle = @'
$ErrorActionPreference = 'SilentlyContinue'
$md = 'http://metadata.google.internal/computeMetadata/v1/instance/attributes'
$min = 30; try { $min = [int](Invoke-RestMethod -UseBasicParsing -Headers @{ 'Metadata-Flavor' = 'Google' } -Uri "$md/pyre-idle-minutes") } catch {}
$stamp = 'C:\ProgramData\pyre-bootstrap\idle-since'
if ($min -le 0) { Remove-Item $stamp -Force; exit 0 }
$busy = $false
# The dev user's per-user install (install.ps1 layout); SYSTEM may use its socket.
$pyred = 'C:\Users\__USER__\AppData\Local\Programs\Pyre\current\bin\pyred.exe'
$st = $null
if (Test-Path $pyred) { try { $st = (& $pyred status --json --state 'C:\Users\__USER__\AppData\Local\Pyre' 2>$null) | ConvertFrom-Json } catch {} }
if ($st) { if ([int]$st.live_runs -gt 0) { $busy = $true } }
else {
  # pyred not answering: idle only after three windows, so a broken node still stops.
  $min = $min * 3
}
# An active Remote Desktop session (not the auto-logon console) counts as busy.
if ((& quser.exe 2>$null) -match 'rdp-tcp#\d+\s+\d+\s+Active') { $busy = $true }
if ($busy) { Remove-Item $stamp -Force; exit 0 }
if (-not (Test-Path $stamp)) { [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() | Set-Content $stamp }
if ([DateTimeOffset]::UtcNow.ToUnixTimeSeconds() - [long](Get-Content $stamp) -ge $min * 60) {
  Write-Output "pyre-idle-stop: no live runs for $min minutes; shutting down (disk kept)"
  & 'C:\Program Files\Tailscale\tailscale.exe' logout
  Stop-Computer -Force
}
'@
Set-Content -Path 'C:\ProgramData\pyre-bootstrap\idle-stop.ps1' -Encoding UTF8 -Value $idle.Replace('__USER__', $User)
$sys = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
$tIdle = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(2) -RepetitionInterval (New-TimeSpan -Minutes 1)
$aIdle = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoProfile -ExecutionPolicy Bypass -File C:\ProgramData\pyre-bootstrap\idle-stop.ps1'
Register-ScheduledTask -TaskName 'PyreIdleStop' -Principal $sys -Trigger $tIdle -Action $aIdle -Force | Out-Null
$tBoot = New-ScheduledTaskTrigger -AtStartup
$aRef = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -File $refresher"
Register-ScheduledTask -TaskName 'PyreAutologonRefresh' -Principal $sys -Trigger $tBoot -Action $aRef -Force | Out-Null
Start-ScheduledTask -TaskName 'PyreAutologonRefresh'

New-Item -ItemType File -Force -Path (Join-Path $Root "staged-$Ver") | Out-Null
Get-ChildItem $Run -Force | Remove-Item -Force -Recurse -ErrorAction SilentlyContinue
Log "node $Name staged (install + pyred at $User logon; auto-logon on; RDP $(if ($cfg.rdp) { 'tailnet-only' } else { 'off' }))"
# First boot: the dev user is not logged on yet; restart once so auto-logon
# fires pyre-first-install (which installs, enrolls and starts pyred).
if (-not (Test-Path (Join-Path $Root 'rebooted'))) {
  New-Item -ItemType File -Force -Path (Join-Path $Root 'rebooted') | Out-Null
  Restart-Computer -Force
}
