<#
.SYNOPSIS
Per-user Pyre install on Windows (idempotent). Never needs Administrator;
never edits the user PATH unless -EditPath; never overwrites files it does not own.

.DESCRIPTION
  install.ps1 -Zip FILE | -From DIR [-Sums SHA256SUMS] [-Sig SHA256SUMS.sig] [-Version V]
              [-AllowedSigners FILE] [-AllowUnsigned]
              [-Service none|node] [-Restart] [-NoActivate] [-NoPrune] [-EditPath]
              [node: -State DIR -Credential FILE -Name NAME -SshAlias A -MaxRuns N
                     -DevMock -NoIntegrations -PyredArg ARG (repeatable)
                     -HubUrl URL -EnrollTicketFile FILE (or the ticket on stdin)]
              [-Arch amd64|arm64] [-Root DIR] [-DryRun]

The Windows counterpart of scripts/install/install.sh (docs/operations.md,
"Windows nodes"). Layout (per user):
  %LOCALAPPDATA%\Programs\Pyre\<version>\bin\{pyre.exe,pyred.exe}   immutable version dirs
  %LOCALAPPDATA%\Programs\Pyre\current  (directory junction -> <version>)
  %LOCALAPPDATA%\Programs\Pyre\.previous, .pyre-install
  %LOCALAPPDATA%\Programs\Pyre\<old version>  (junction -> current after pruning)
  %LOCALAPPDATA%\Pyre        node state (pyred default; -State overrides)
  %APPDATA%\Pyre             config: client.json, release-allowed-signers (pin), keys.toml
There are no shims: -EditPath adds ...\Pyre\current\bin to the user PATH
(HKCU\Environment); without it the command to do so is printed.

-From DIR: a release directory (DIR\bin\pyred.exe) or a build directory
(DIR\pyred.exe). Version: -Version, else DIR\VERSION, else `pyred version --json`.

Release signature (-Zip, before anything is extracted): SHA256SUMS beside the
zip must carry a valid `ssh-keygen -Y` signature (SHA256SUMS.sig, namespace
pyre-release) from a trusted key: -AllowedSigners FILE, else the pinned
%APPDATA%\Pyre\release-allowed-signers, else packaging\release-allowed-signers
of the source checkout this script runs from. A verified key is pinned when
nothing is pinned yet. Verification uses ssh-keygen (Windows OpenSSH >= 8.1
supports -Y); without it, the ALREADY INSTALLED pyred.exe (`pyred verify-release`)
verifies -- never a binary from the candidate archive. A bad signature or a
tampered SHA256SUMS is always refused (exit 3); a missing signature, a missing
SHA256SUMS, no trusted key or no verifier is refused unless -AllowUnsigned.
-From DIR cannot be signature-checked: refused once a key is pinned or given,
unless -AllowUnsigned.

-Service node registers the per-user logon task with
`pyred service install --state DIR -- <run args>` and starts it. The task runs
in the interactive session (not a Windows service: session 0 has no desktop).
-Restart restarts the node task after activating a new version (upgrade).
-Service hub is refused: the hub is unix-only.

Enrollment (-Service node): an enrollment ticket (the JSON printed by
`pyre --json enroll-node --name X`) on stdin, or in -EnrollTicketFile FILE, is
redeemed after activation with `pyred enroll --ticket-stdin --state DIR
[--hub URL]`; the node credential lands in <state>\node-credential.json, which
the task's `pyred run` uses. -HubUrl overrides the ticket's hub URL (the hub as
reachable from this node). The ticket is never echoed, logged or put on a
command line. Skipped (with a note) when node-credential.json already exists.
  Get-Content ticket.json | powershell -NoProfile -File install.ps1 -Zip Z `
    -AllowedSigners A -Service node -Name NAME -HubUrl URL
Run it as the user the node belongs to, never as SYSTEM or another account:
everything (binaries, state, config, the logon task) is that user's. GCP
Windows nodes (internal/cloud/startup.ps1) do this with a one-shot logon task
"pyre-first-install" that runs this script as the dev user with
-EnrollTicketFile and deletes the ticket file afterwards.

-PyredArg ARG adds ARG to the task's `pyred run` command line (e.g.
--trust-dir=DIR, --tool-path=DIR); never a secret.

Exit codes: 0 ok, 1 error, 2 usage, 3 refused (a non-Pyre file is in the way,
or the release signature/checksum did not verify).
#>
[CmdletBinding()]
param(
  [string]$Zip = "",
  [string]$From = "",
  [string]$Sums = "",
  [string]$Sig = "",
  [string]$AllowedSigners = "",
  [switch]$AllowUnsigned,
  [string]$Version = "",
  [ValidateSet("none", "node", "hub")][string]$Service = "none",
  [switch]$Restart,
  [switch]$NoActivate,
  [switch]$NoPrune,
  [switch]$EditPath,
  [string]$State = "",
  [string]$Credential = "",
  [string]$Name = "",
  [string]$SshAlias = "",
  [int]$MaxRuns = 0,
  [switch]$DevMock,
  [switch]$NoIntegrations,
  [string]$HubUrl = "",
  [string]$EnrollTicketFile = "",
  [string[]]$PyredArg = @(),
  [string]$PyredArgsFile = "",
  [ValidateSet("", "amd64", "arm64")][string]$Arch = "",
  [string]$Root = "",
  [switch]$DryRun
)
Set-StrictMode -Version 2.0
$ErrorActionPreference = "Stop"
$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
$Marker = "pyre-managed"
$Bins = @("pyre.exe", "pyred.exe")
$Namespace = "pyre-release"

# ---- helpers ----
function Note([string]$m) { Write-Output "==> $m" }
function Warn([string]$m) { [Console]::Error.WriteLine("warning: $m") }
function Die([string]$m, [int]$code = 1) { [Console]::Error.WriteLine("error: $m"); exit $code }
function Refuse([string]$m) { Die "$m; nothing was installed" 3 }

# R PATH: the path as written on this machine (fake-root prefixed with -Root).
function Under-Root([string]$p) {
  if ($Root -eq "") { return $p }
  return (Join-Path $Root ($p -replace '^[A-Za-z]:', ''))
}
function Act([string]$desc, [scriptblock]$do) {
  if ($DryRun) { Write-Output "[dry-run] $desc"; return }
  & $do
}
function Sha256([string]$f) { return (Get-FileHash -Algorithm SHA256 -LiteralPath $f).Hash.ToLowerInvariant() }

function Valid-Version([string]$v) {
  if ($v -eq "" -or $v -eq "current" -or $v -eq "bin" -or $v.StartsWith(".")) { return $false }
  return ($v -match '^[A-Za-z0-9._+-]+$')
}

# Windows command-line quoting (CommandLineToArgvW rules) for .NET Framework,
# whose ProcessStartInfo has no ArgumentList.
function Quote-Arg([string]$a) {
  if ($a -ne "" -and $a -notmatch '[\s"]') { return $a }
  $sb = New-Object System.Text.StringBuilder
  [void]$sb.Append('"')
  $bs = 0
  foreach ($c in $a.ToCharArray()) {
    if ($c -eq '\') { $bs++; continue }
    if ($c -eq '"') { [void]$sb.Append('\' * (2 * $bs + 1)); [void]$sb.Append('"'); $bs = 0; continue }
    if ($bs -gt 0) { [void]$sb.Append('\' * $bs); $bs = 0 }
    [void]$sb.Append($c)
  }
  if ($bs -gt 0) { [void]$sb.Append('\' * (2 * $bs)) }
  [void]$sb.Append('"')
  return $sb.ToString()
}

# Invoke-Native EXE ARGS [STDINFILE] [STDINTEXT]: runs EXE with exact argv and
# the exact bytes of STDINFILE (or STDINTEXT, UTF-8) on stdin (PowerShell pipes
# would re-encode). Stdin is always redirected, so a child never consumes ours.
function Invoke-Native([string]$exe, [string[]]$argv, [string]$stdinFile = "", [string]$stdinText = "") {
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $exe
  $psi.Arguments = (($argv | ForEach-Object { Quote-Arg $_ }) -join " ")
  $psi.UseShellExecute = $false
  $psi.CreateNoWindow = $true
  $psi.RedirectStandardOutput = $true
  $psi.RedirectStandardError = $true
  $psi.RedirectStandardInput = $true
  $p = [System.Diagnostics.Process]::Start($psi)
  $outTask = $p.StandardOutput.ReadToEndAsync()
  $errTask = $p.StandardError.ReadToEndAsync()
  $b = $null
  if ($stdinFile -ne "") { $b = [System.IO.File]::ReadAllBytes($stdinFile) }
  elseif ($stdinText -ne "") { $b = (New-Object System.Text.UTF8Encoding($false)).GetBytes($stdinText + "`n") }
  if ($null -ne $b) { $p.StandardInput.BaseStream.Write($b, 0, $b.Length) }
  $p.StandardInput.Close()
  $p.WaitForExit()
  return [pscustomobject]@{ Code = $p.ExitCode; Out = $outTask.Result; Err = $errTask.Result }
}

# Set-PrivateAcl DIR: protected DACL, full control for the current user and
# SYSTEM only (inherited by everything below). SIDs, so localized names do not matter.
function Set-PrivateAcl([string]$dir) {
  $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value
  if ($DryRun) { Write-Output "[dry-run] icacls $dir /inheritance:r /grant:r *${user}:(OI)(CI)F *S-1-5-18:(OI)(CI)F"; return }
  $r = Invoke-Native "icacls.exe" @($dir, "/inheritance:r", "/grant:r", "*${user}:(OI)(CI)F", "*S-1-5-18:(OI)(CI)F", "/Q")
  if ($r.Code -ne 0) { Warn "could not restrict $dir to the current user (icacls exit $($r.Code)): $($r.Err.Trim())" }
}
function New-PrivateDir([string]$dir) {
  if (-not (Test-Path -LiteralPath $dir)) { Act "mkdir $dir" { [void](New-Item -ItemType Directory -Force -Path $dir) } }
  Set-PrivateAcl $dir
}

function Is-Reparse([string]$p) {
  $i = Get-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
  if ($null -eq $i) { return $false }
  return [bool]($i.Attributes -band [System.IO.FileAttributes]::ReparsePoint)
}
function Junction-Target([string]$p) {
  $i = Get-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
  if ($null -eq $i -or -not ($i.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) { return "" }
  $t = @($i.Target)
  if ($t.Count -eq 0 -or $null -eq $t[0]) { return "" }
  return ([string]$t[0]) -replace '^\\\\\?\\', '' -replace '^\\\?\?\\', ''
}
# Remove-Link P: removes a junction itself, never what it points to.
function Remove-Link([string]$p) { [System.IO.Directory]::Delete($p, $false) }

# Swap-Junction TARGETDIR LINK: point LINK at TARGETDIR (absolute; junctions
# cannot be relative). Not atomic on Windows: new link, remove old, rename.
function Swap-Junction([string]$target, [string]$link) {
  if ($DryRun) { Write-Output "[dry-run] junction $link -> $target"; return }
  if ((Test-Path -LiteralPath $link) -and -not (Is-Reparse $link)) { Die "$link exists and is not a junction; refusing to replace it" }
  $tmp = "$link.new.$PID"
  if (Test-Path -LiteralPath $tmp) { Remove-Link $tmp }
  [void](New-Item -ItemType Junction -Path $tmp -Target $target)
  if (Test-Path -LiteralPath $link) { Remove-Link $link }
  [System.IO.Directory]::Move($tmp, $link)
}

function Host-Arch {
  $a = $env:PROCESSOR_ARCHITEW6432
  if (-not $a) { $a = $env:PROCESSOR_ARCHITECTURE }
  switch ($a) { "ARM64" { return "arm64" } "AMD64" { return "amd64" } default { return $a.ToLowerInvariant() } }
}
# The target binaries run here: same arch, or amd64 on Windows on ARM (emulated).
function Can-Exec { $h = Host-Arch; return ($T_Arch -eq $h -or ($T_Arch -eq "amd64" -and $h -eq "arm64")) }

# Release-Keys FILE: the pyre-release keys of an allowed-signers file ("type base64", sorted).
function Release-Keys([string]$f) {
  $keys = @()
  foreach ($line in [System.IO.File]::ReadAllLines($f)) {
    $w = @($line.Trim() -split '\s+')
    if ($w.Count -lt 3 -or $w[0] -ne $Namespace) { continue }
    for ($i = 1; $i -lt $w.Count - 1; $i++) { if ($w[$i] -like "ssh-*") { $keys += "$($w[$i]) $($w[$i + 1])"; break } }
  }
  return (($keys | Sort-Object -Unique) -join "`n")
}
function Sums-Lookup([string]$sumsFile, [string]$name) {
  foreach ($line in [System.IO.File]::ReadAllLines($sumsFile)) {
    $w = @($line.Trim() -split '\s+', 2)
    if ($w.Count -ne 2) { continue }
    if ($w[1].TrimStart('*') -eq $name) { return $w[0].ToLowerInvariant() }
  }
  return ""
}
# Verify-Sums DIR SUMSFILE: every listed file under DIR matches; returns the count.
function Verify-Sums([string]$dir, [string]$sumsFile) {
  $n = 0
  foreach ($line in [System.IO.File]::ReadAllLines($sumsFile)) {
    if ($line.Trim() -eq "") { continue }
    $w = @($line.Trim() -split '\s+', 2)
    $rel = $w[1].TrimStart('*') -replace '/', '\'
    $f = Join-Path $dir $rel
    if (-not (Test-Path -LiteralPath $f -PathType Leaf)) { Refuse "checksum: $f missing" }
    $got = Sha256 $f
    if ($got -ne $w[0].ToLowerInvariant()) { Refuse "checksum mismatch for $rel (expected $($w[0]), got $got)" }
    $n++
  }
  if ($n -eq 0) { Die "checksum file $sumsFile is empty" }
  return $n
}
# ssh-keygen with -Y support (OpenSSH >= 8.1), or "".
function Find-SshKeygen {
  $c = Get-Command ssh-keygen.exe -ErrorAction SilentlyContinue
  if ($null -eq $c) { return "" }
  $r = Invoke-Native $c.Source @("-Y")
  if (($r.Out + $r.Err) -match 'option -- .?Y') { return "" }  # "unknown/illegal option -- Y"
  return $c.Source
}

# ---- paths ----
if ($Service -eq "hub") { Die "-Service hub is not supported on Windows: pyre-hub is unix-only (run the hub on macOS or Linux)" 2 }
if ($Zip -eq "" -and $From -eq "") { Die "-Zip FILE or -From DIR is required (Get-Help $($MyInvocation.MyCommand.Path) -Detailed)" 2 }
if ($Zip -ne "" -and $From -ne "") { Die "-Zip and -From are exclusive" 2 }
# Absolute paths: .NET file APIs resolve relative paths against the process
# directory, not the PowerShell location.
function Abs([string]$p) { if ($p -eq "") { return "" }; return $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($p) }
$Zip = Abs $Zip; $From = Abs $From; $Sums = Abs $Sums; $Sig = Abs $Sig; $AllowedSigners = Abs $AllowedSigners
$State = Abs $State; $Credential = Abs $Credential; $Root = Abs $Root
$EnrollTicketFile = Abs $EnrollTicketFile
# -PyredArgsFile FILE: a JSON array of extra pyred run arguments (a string
# array cannot be passed through `powershell -File`; used by cloud nodes).
if ($PyredArgsFile -ne "") {
  if (-not (Test-Path -LiteralPath $PyredArgsFile -PathType Leaf)) { Die "-PyredArgsFile $PyredArgsFile not found" 2 }
  $PyredArg = @($PyredArg) + @((Get-Content -Raw -LiteralPath $PyredArgsFile | ConvertFrom-Json) | ForEach-Object { [string]$_ })
}
# The enrollment ticket is read before any child process runs; never echoed.
$Ticket = ""
if ($EnrollTicketFile -ne "") {
  if (-not (Test-Path -LiteralPath $EnrollTicketFile -PathType Leaf)) { Die "-EnrollTicketFile $EnrollTicketFile not found" 2 }
  $Ticket = [System.IO.File]::ReadAllText($EnrollTicketFile).Trim()
} elseif ($Service -eq "node" -and [Console]::IsInputRedirected) {
  $Ticket = [Console]::In.ReadToEnd().Trim()
}
if ($Ticket -ne "" -and $Service -ne "node") { Die "an enrollment ticket needs -Service node" 2 }
$LocalAppData = $env:LOCALAPPDATA
$AppData = $env:APPDATA
if (-not $LocalAppData -or -not $AppData) { Die "LOCALAPPDATA/APPDATA are not set: run as the owning user in a normal Windows session" }
$Prefix = Join-Path $LocalAppData "Programs\Pyre"
$ConfigDir = Join-Path $AppData "Pyre"
$StateDefault = Join-Path $LocalAppData "Pyre"
if ($State -eq "") { $State = $StateDefault }
$Pin = Join-Path $ConfigDir "release-allowed-signers"
$CheckoutSigners = Join-Path $Here "..\..\packaging\release-allowed-signers"
$CurBin = Join-Path $Prefix "current\bin"

$Work = ""
$exitCode = 0
try {
  # ---- release signature trust ----
  $Trust = ""; $TrustFrom = ""; $PinKey = ""
  if ($AllowedSigners -ne "") {
    if (-not (Test-Path -LiteralPath $AllowedSigners -PathType Leaf)) { Die "-AllowedSigners $AllowedSigners not found" }
    if ((Release-Keys $AllowedSigners) -eq "") { Die "$AllowedSigners has no pyre-release key" }
    if ((Test-Path -LiteralPath (Under-Root $Pin)) -and ((Release-Keys (Under-Root $Pin)) -ne (Release-Keys $AllowedSigners))) {
      Refuse "-AllowedSigners $AllowedSigners differs from the pinned release key $Pin (to rotate the key, replace $Pin deliberately)"
    }
    $Trust = (Resolve-Path -LiteralPath $AllowedSigners).Path; $TrustFrom = "-AllowedSigners"
  } elseif (Test-Path -LiteralPath (Under-Root $Pin)) {
    $Trust = Under-Root $Pin; $TrustFrom = "pinned"
  } elseif ((Test-Path -LiteralPath $CheckoutSigners) -and ((Release-Keys $CheckoutSigners) -ne "")) {
    $Trust = (Resolve-Path -LiteralPath $CheckoutSigners).Path; $TrustFrom = "source checkout"
  }

  if ($Zip -ne "") {
    if (-not (Test-Path -LiteralPath $Zip -PathType Leaf)) { Die "zip $Zip not found" }
    $Zip = (Resolve-Path -LiteralPath $Zip).Path
    $zipDir = Split-Path -Parent $Zip
    $zipName = Split-Path -Leaf $Zip
    if ($Sums -eq "" -and (Test-Path -LiteralPath (Join-Path $zipDir "SHA256SUMS"))) { $Sums = Join-Path $zipDir "SHA256SUMS" }
    $Work = Join-Path ([System.IO.Path]::GetTempPath()) ("pyre-install." + [guid]::NewGuid().ToString("N").Substring(0, 12))
    [void](New-Item -ItemType Directory -Path $Work)
    if ($Sums -eq "") {
      if (-not $AllowUnsigned) { Refuse "no SHA256SUMS next to the zip and no -Sums: its provenance cannot be verified (pass -AllowUnsigned only for a build you made yourself)" }
      Warn "no SHA256SUMS next to the zip and no -Sums: provenance not verified (-AllowUnsigned)"
    } else {
      if (-not (Test-Path -LiteralPath $Sums -PathType Leaf)) { Die "checksum file $Sums not found" }
      if ($Sig -eq "") { $Sig = "$Sums.sig" }
      if (Test-Path -LiteralPath $Sig -PathType Leaf) {
        if ($Trust -eq "") {
          if (-not $AllowUnsigned) { Refuse "the release is signed ($Sig) but no release key is trusted here: pass -AllowedSigners <packaging\release-allowed-signers from a trusted checkout> (it is then pinned), or verify by hand and pass -AllowUnsigned" }
          Warn "the release signature was NOT verified: no trusted release key (-AllowUnsigned)"
        } else {
          # Verify private copies; checksum the zip against exactly the verified file.
          $vs = Join-Path $Work "SHA256SUMS.verified"
          Copy-Item -LiteralPath $Sums -Destination $vs
          Copy-Item -LiteralPath $Sig -Destination "$vs.sig"
          $kg = Find-SshKeygen
          $installed = Under-Root (Join-Path $CurBin "pyred.exe")
          if ($kg -ne "") {
            $r = Invoke-Native $kg @("-Y", "verify", "-f", $Trust, "-I", $Namespace, "-n", $Namespace, "-s", "$vs.sig") $vs
            $how = "ssh-keygen"
          } elseif (Test-Path -LiteralPath $installed) {
            # The installed (previously trusted) pyred verifies; never the candidate's.
            $r = Invoke-Native $installed @("verify-release", "--allowed-signers", $Trust, "--sums", $vs, "--sig", "$vs.sig")
            $how = "installed pyred verify-release"
          } else {
            $r = $null
          }
          if ($null -eq $r) {
            if (-not $AllowUnsigned) { Refuse "cannot verify the release signature: no ssh-keygen with -Y (Windows OpenSSH >= 8.1: Settings > Optional features > OpenSSH Client) and no installed pyred.exe to verify with" }
            Warn "the release signature was NOT verified: no verifier available (-AllowUnsigned)"
          } elseif ($r.Code -ne 0) {
            ($r.Out + $r.Err).Split("`n") | ForEach-Object { if ($_.Trim() -ne "") { [Console]::Error.WriteLine("    $($_.TrimEnd())") } }
            Refuse "release signature verification FAILED for $Sums ($TrustFrom key $Trust, $how): the checksums were modified or signed by another key"
          } else {
            $first = (($r.Out + $r.Err).Split("`n") | Where-Object { $_.Trim() -ne "" } | Select-Object -First 1)
            Note "release signature verified ($how): $first ($TrustFrom)"
            $Sums = $vs
            if ($TrustFrom -ne "pinned" -and -not (Test-Path -LiteralPath (Under-Root $Pin))) { $PinKey = $Trust }
          }
        }
      } else {
        if (-not $AllowUnsigned) { Refuse "the release is not signed (no $Sig): pass -AllowUnsigned only for a build you made yourself" }
        Warn "installing an UNSIGNED release (-AllowUnsigned)"
      }
      $want = Sums-Lookup $Sums $zipName
      if ($want -eq "") { Refuse "$zipName is not listed in $Sums" }
      $got = Sha256 $Zip
      if ($got -ne $want) { Refuse "zip checksum mismatch (expected $want, got $got)" }
      Note "zip checksum verified against SHA256SUMS"
    }
    $ex = Join-Path $Work "x"
    Expand-Archive -LiteralPath $Zip -DestinationPath $ex
    $top = @(Get-ChildItem -LiteralPath $ex -Directory)
    if ($top.Count -ne 1) { Die "zip must have exactly one top-level directory" }
    $From = $top[0].FullName
    if ($Arch -eq "" -and $zipName -match '-windows-(amd64|arm64)\.zip$') { $Arch = $Matches[1] }
  } else {
    if ($TrustFrom -eq "pinned" -or $TrustFrom -eq "-AllowedSigners") {
      if (-not $AllowUnsigned) { Refuse "-From $From cannot be signature-checked and a release key is trusted ($TrustFrom); install from the signed -Zip, or pass -AllowUnsigned" }
      Warn "installing an unverified directory although a release key is trusted (-AllowUnsigned)"
    } else {
      Note "-From ${From}: a directory carries no release signature (local build)"
    }
    if (-not (Test-Path -LiteralPath $From -PathType Container)) { Die "-From $From is not a directory" }
    $From = (Resolve-Path -LiteralPath $From).Path
  }
  $T_Arch = $Arch
  if ($T_Arch -eq "") { $T_Arch = Host-Arch }

  if (Test-Path -LiteralPath (Join-Path $From "bin\pyred.exe")) {
    $SrcBin = Join-Path $From "bin"
  } elseif (Test-Path -LiteralPath (Join-Path $From "pyred.exe")) {
    $SrcBin = $From
  } else {
    Die "no pyred.exe under $From"
  }
  foreach ($b in $Bins) { if (-not (Test-Path -LiteralPath (Join-Path $SrcBin $b))) { Die "missing $SrcBin\$b" } }
  if ((Test-Path -LiteralPath (Join-Path $From "SHA256SUMS")) -and $SrcBin -eq (Join-Path $From "bin")) {
    $n = Verify-Sums $From (Join-Path $From "SHA256SUMS")
    Note "release contents verified ($n files, $From\SHA256SUMS)"
  }
  if ($Version -eq "" -and (Test-Path -LiteralPath (Join-Path $From "VERSION"))) { $Version = ([System.IO.File]::ReadAllText((Join-Path $From "VERSION"))).Trim() }
  if ($Version -eq "" -and (Can-Exec)) {
    $r = Invoke-Native (Join-Path $SrcBin "pyred.exe") @("version", "--json")
    if ($r.Code -eq 0 -and $r.Out -match '"version"\s*:\s*"([^"]*)"') { $Version = $Matches[1] }
  }
  if ($Version -eq "") { Die "cannot determine the version: pass -Version" }
  if (-not (Valid-Version $Version)) { Die "invalid version '$Version'" }
  $rootNote = ""
  if ($Root -ne "") { $rootNote = " (fake root $Root)" }
  Note "Pyre $Version for windows/$T_Arch -> $Prefix$rootNote"

  # ---- refuse to clobber non-Pyre files ----
  $refused = $false
  if ((Test-Path -LiteralPath (Under-Root $Prefix)) -and -not (Test-Path -LiteralPath (Under-Root (Join-Path $Prefix ".pyre-install")))) {
    # A bootstrap install leaves bin\pyred.exe and node-credential.json here; anything else is not ours.
    foreach ($e in @(Get-ChildItem -LiteralPath (Under-Root $Prefix) -Force)) {
      if ($e.Name -ne "bin" -and $e.Name -ne "node-credential.json") {
        [Console]::Error.WriteLine("refusing: $Prefix contains $($e.Name) and no .pyre-install marker"); $refused = $true
      }
    }
  }
  if ($refused) { Die "nothing was changed; move the files above aside or remove them deliberately" 3 }

  # ---- pin the release key that verified this release ----
  New-PrivateDir (Under-Root $ConfigDir)
  if ($PinKey -ne "") {
    Act "pin release key: copy $PinKey $Pin" { Copy-Item -LiteralPath $PinKey -Destination (Under-Root $Pin) -Force }
    Note "pinned the release key in $Pin ($TrustFrom); upgrades now require releases signed by it"
  }

  # ---- install the version directory ----
  $VDir = Join-Path $Prefix $Version
  if (Is-Reparse (Under-Root $VDir)) {
    Note "$Version was pruned ($VDir -> $(Junction-Target (Under-Root $VDir))); reinstalling it"
    Act "remove junction $VDir" { Remove-Link (Under-Root $VDir) }
  }
  if (Test-Path -LiteralPath (Under-Root $VDir) -PathType Container) {
    foreach ($b in $Bins) {
      $inst = Under-Root (Join-Path $VDir "bin\$b")
      if (-not (Test-Path -LiteralPath $inst) -or (Sha256 (Join-Path $SrcBin $b)) -ne (Sha256 $inst)) {
        Die "$VDir exists with different binaries; versions are immutable (remove it with uninstall.ps1 or use a new version)" 3
      }
    }
    Note "$Version already installed (identical binaries)"
  } else {
    New-PrivateDir (Under-Root $Prefix)
    $Stage = Join-Path (Under-Root $Prefix) ".staging-$Version-$PID"
    Act "stage $Stage" {
      [void](New-Item -ItemType Directory -Force -Path (Join-Path $Stage "bin"))
      $lines = @()
      foreach ($b in $Bins) {
        Copy-Item -LiteralPath (Join-Path $SrcBin $b) -Destination (Join-Path $Stage "bin\$b")
        $h = Sha256 (Join-Path $Stage "bin\$b")
        if ($h -ne (Sha256 (Join-Path $SrcBin $b))) { Die "copy of $b does not match its source" }
        $lines += "$h  bin/$b"
      }
      [System.IO.File]::WriteAllText((Join-Path $Stage "SHA256SUMS"), (($lines -join "`n") + "`n"))
      [System.IO.File]::WriteAllText((Join-Path $Stage "VERSION"), "$Version`n")
    }
    Act "rename $Stage -> $VDir" { [System.IO.Directory]::Move($Stage, (Under-Root $VDir)) }
    Note "installed $VDir"
  }
  $markerFile = Under-Root (Join-Path $Prefix ".pyre-install")
  if (-not $DryRun -and -not (Test-Path -LiteralPath $markerFile)) {
    [System.IO.File]::WriteAllText($markerFile, "${Marker}: per-user Pyre installation (scripts/install/install.ps1)`nDelete with scripts/install/uninstall.ps1.`n")
  }

  # ---- health check of the new binaries ----
  if ((Can-Exec) -and -not $DryRun) {
    $r = Invoke-Native (Under-Root (Join-Path $VDir "bin\pyred.exe")) @("version", "--json")
    if ($r.Code -ne 0) { Die "pyred $Version failed its health check (version --json): $($r.Err.Trim())" }
    Note "health: pyred version --json -> $($r.Out.Trim())"
    $r = Invoke-Native (Under-Root (Join-Path $VDir "bin\pyre.exe")) @("version")
    if ($r.Code -ne 0) { Die "pyre $Version failed its health check: $($r.Err.Trim())" }
  } else {
    Note "health check skipped (dry run, or windows/$T_Arch binaries cannot run on this host)"
  }

  # ---- activate ----
  $Cur = Under-Root (Join-Path $Prefix "current")
  $Prev = ""
  $t = Junction-Target $Cur
  if ($t -ne "") { $Prev = Split-Path -Leaf $t }
  $switched = $false
  if (-not $NoActivate) {
    if ($Prev -eq $Version) {
      Note "current already -> $Version"
    } else {
      if ($Prev -ne "" -and -not $DryRun) { [System.IO.File]::WriteAllText((Under-Root (Join-Path $Prefix ".previous")), "$Prev`n") }
      Swap-Junction (Under-Root $VDir) $Cur
      $switched = $true
      $was = ""
      if ($Prev -ne "") { $was = " (was $Prev)" }
      Note "current -> $Version$was"
    }
    if (-not $NoPrune) {
      # Prune: every version other than current and .previous becomes the junction
      # "<version> -> current", so hook paths of sessions started under it keep working.
      $prevFile = Under-Root (Join-Path $Prefix ".previous")
      $keepPrev = ""
      if (Test-Path -LiteralPath $prevFile) { $keepPrev = ([System.IO.File]::ReadAllText($prevFile)).Trim() }
      $n = 0
      foreach ($d in @(Get-ChildItem -LiteralPath (Under-Root $Prefix) -Directory -Force -ErrorAction SilentlyContinue)) {
        $v = $d.Name
        if (Is-Reparse $d.FullName) { continue }
        if (-not (Valid-Version $v) -or $v -eq $Version -or $v -eq $keepPrev) { continue }
        if (-not (Test-Path -LiteralPath (Join-Path $d.FullName "VERSION")) -and -not (Test-Path -LiteralPath (Join-Path $d.FullName "bin\pyred.exe"))) { continue }
        $aside = Join-Path (Under-Root $Prefix) ".pruned-$v.$PID"
        if ($DryRun) { Write-Output "[dry-run] prune $v (junction $($d.FullName) -> current)"; continue }
        try { [System.IO.Directory]::Move($d.FullName, $aside) } catch { Warn "cannot prune ${v}: $($_.Exception.Message) (a process may still run from it); kept"; continue }
        [void](New-Item -ItemType Junction -Path $d.FullName -Target $Cur)
        Remove-Item -LiteralPath $aside -Recurse -Force -ErrorAction SilentlyContinue
        if (Test-Path -LiteralPath $aside) { Warn "could not delete $aside completely (files in use); remove it later" }
        $n++
        Note "pruned $v (left as $Prefix\$v -> current, so paths into it keep working)"
      }
      if ($n -eq 0) { Note "nothing to prune" }
    }
  } else {
    $cv = $Prev; if ($cv -eq "") { $cv = "unset" }
    Note "staged only (-NoActivate); current stays $cv"
  }

  # ---- PATH ----
  $userPath = ""
  if ($Root -eq "") { $userPath = [string](Get-Item "HKCU:\Environment").GetValue("Path", "", "DoNotExpandEnvironmentNames") }
  $onPath = @($userPath -split ';' | Where-Object { $_.TrimEnd('\') -ieq $CurBin }).Count -gt 0
  if ($EditPath -and $Root -eq "") {
    if ($onPath) { Note "$CurBin already on the user PATH" }
    else {
      $new = $CurBin
      if ($userPath.Trim(';') -ne "") { $new = $userPath.TrimEnd(';') + ";" + $CurBin }
      Act "add $CurBin to the user PATH (HKCU\Environment)" {
        [Microsoft.Win32.Registry]::SetValue("HKEY_CURRENT_USER\Environment", "Path", $new, [Microsoft.Win32.RegistryValueKind]::ExpandString)
        # A no-op user-variable change broadcasts WM_SETTINGCHANGE for new shells.
        [Environment]::SetEnvironmentVariable("PYRE_PATH_REFRESH", $null, "User")
      }
      Note "added $CurBin to the user PATH (new shells pick it up; uninstall.ps1 removes it)"
    }
  } elseif (-not $onPath -and $Root -eq "") {
    Note "$CurBin is not on the user PATH. Add it yourself, or re-run with -EditPath"
  }

  # ---- harness prerequisites (warn only) ----
  if ($Root -eq "") {
    if ($null -eq (Get-Command claude -ErrorAction SilentlyContinue)) { Warn "claude not found on PATH: Claude Code runs will be unavailable (install it natively: https://code.claude.com/docs)" }
    if ($null -eq (Get-Command codex -ErrorAction SilentlyContinue)) { Warn "codex not found on PATH: Codex runs will be unavailable" }
    $gitBash = $env:CLAUDE_CODE_GIT_BASH_PATH
    if (-not $gitBash) {
      $git = Get-Command git.exe -ErrorAction SilentlyContinue
      if ($null -ne $git) { $gitBash = Join-Path (Split-Path -Parent (Split-Path -Parent $git.Source)) "bin\bash.exe" }
    }
    if (-not $gitBash -or -not (Test-Path -LiteralPath $gitBash)) {
      foreach ($c in @("$env:ProgramFiles\Git\bin\bash.exe", "$LocalAppData\Programs\Git\bin\bash.exe")) { if (Test-Path -LiteralPath $c) { $gitBash = $c; break } }
    }
    if (-not $gitBash -or -not (Test-Path -LiteralPath $gitBash)) { Warn "Git for Windows (bash.exe) not found: Claude Code on Windows needs it (install Git for Windows, or set CLAUDE_CODE_GIT_BASH_PATH)" }
    # Node.js toolchain (Electron development): PATH, nvm-windows, MSI/winget location.
    $nodeDirs = @()
    if ($env:NVM_SYMLINK) { $nodeDirs += $env:NVM_SYMLINK }
    if ($env:ProgramFiles) { $nodeDirs += (Join-Path $env:ProgramFiles "nodejs") }
    foreach ($t in @("node", "npm", "npx")) {
      $found = $null -ne (Get-Command $t -ErrorAction SilentlyContinue)
      foreach ($d in $nodeDirs) { if ((Test-Path -LiteralPath (Join-Path $d "$t.exe")) -or (Test-Path -LiteralPath (Join-Path $d "$t.cmd"))) { $found = $true } }
      if (-not $found) { Warn "$t not found (PATH, %NVM_SYMLINK%, %ProgramFiles%\nodejs): Node/Electron builds in runs will fail" }
    }
    $npmGlobal = Join-Path $AppData "npm"
    if (-not (Test-Path -LiteralPath $npmGlobal)) { Note "npm global bin $npmGlobal does not exist yet (the first npm install -g creates it)" }
    $vsOK = $false
    $pf86 = [Environment]::GetEnvironmentVariable("ProgramFiles(x86)")
    if ($pf86) {
      $vswhere = Join-Path $pf86 "Microsoft Visual Studio\Installer\vswhere.exe"
      if (Test-Path -LiteralPath $vswhere) {
        $vr = Invoke-Native $vswhere @("-products", "*", "-requires", "Microsoft.VisualStudio.Component.VC.Tools.x86.x64", "-property", "installationPath")
        $vsOK = ($vr.Code -eq 0 -and $vr.Out.Trim() -ne "")
      }
    }
    if (-not $vsOK) { Warn "Visual Studio Build Tools with the C++ workload not found (vswhere): native Node modules (node-gyp, Electron rebuilds) cannot compile" }
  }

  # ---- service ----
  if ($Service -eq "none") {
    Note "no service requested (-Service node to install the logon task)"
    exit 0
  }
  New-PrivateDir (Under-Root $State)
  $svcExe = Join-Path $CurBin "pyred.exe"
  $stateCred = Under-Root (Join-Path $State "node-credential.json")
  if ($Ticket -ne "") {
    $enrollArgs = @("enroll", "--ticket-stdin", "--state", $State)
    if ($HubUrl -ne "") { $enrollArgs += @("--hub", $HubUrl) }
    if (Test-Path -LiteralPath $stateCred) {
      Note "$State\node-credential.json already exists: enrollment skipped (the ticket was not used)"
    } elseif ($DryRun -or $Root -ne "" -or -not (Can-Exec)) {
      Write-Output "[not run] $svcExe $($enrollArgs -join ' ')  (ticket on stdin)"
    } else {
      Write-Output "+ $svcExe $($enrollArgs -join ' ')  (ticket on stdin)"
      $r = Invoke-Native $svcExe $enrollArgs "" $Ticket
      if ($r.Out.Trim() -ne "") { Write-Output $r.Out.TrimEnd() }
      if ($r.Code -ne 0) { Die "pyred enroll failed (exit $($r.Code)): $($r.Err.Trim())" }
    }
  } elseif ($HubUrl -ne "" -and -not (Test-Path -LiteralPath $stateCred)) {
    Warn "-HubUrl given but no enrollment ticket on stdin or in -EnrollTicketFile: nothing was enrolled"
  }
  $Ticket = ""
  $runArgs = @()
  if ($Credential -eq "" -and -not (Test-Path -LiteralPath (Under-Root (Join-Path $State "node-credential.json"))) -and (Test-Path -LiteralPath (Under-Root (Join-Path $Prefix "node-credential.json")))) {
    $Credential = Join-Path $Prefix "node-credential.json"  # written by `pyre enroll-node --bootstrap`
    Note "using the bootstrap credential $Credential"
  }
  if ($Credential -ne "") { $runArgs += @("--credential", $Credential) }
  if ($Name -ne "") { $runArgs += @("--name", $Name) }
  if ($SshAlias -ne "") { $runArgs += @("--ssh-alias", $SshAlias) }
  if ($MaxRuns -gt 0) { $runArgs += @("--max-runs", [string]$MaxRuns) }
  if ($DevMock) { $runArgs += "--dev-mock" }
  if ($NoIntegrations) { $runArgs += "--no-integrations" }
  foreach ($a in $PyredArg) { if ($a -ne "") { $runArgs += $a } }
  if ($Credential -eq "" -and -not (Test-Path -LiteralPath (Under-Root (Join-Path $State "node-credential.json")))) {
    Warn "no node credential yet: pyred will run standalone until you enroll it (pyre enroll-node, then pyred enroll)"
  }
  $installArgs = @("service", "install", "--no-start", "--state", $State) + @("--") + $runArgs
  $display = "$svcExe " + (($installArgs | ForEach-Object { Quote-Arg $_ }) -join " ")
  if ($DryRun -or $Root -ne "" -or -not (Can-Exec)) {
    Write-Output "[service-manager, not run] $display"
    if ($Restart) { Write-Output "[service-manager, not run] $svcExe service stop; $svcExe service start" }
    Note "service manager not invoked (fake root, foreign architecture or -DryRun)"
  } else {
    Write-Output "+ $display"
    $r = Invoke-Native $svcExe $installArgs
    if ($r.Out.Trim() -ne "") { Write-Output $r.Out.TrimEnd() }
    if ($r.Code -ne 0) { Die "pyred service install failed (exit $($r.Code)): $($r.Err.Trim())" }
    if ($Restart -and $switched) {
      Write-Output "+ $svcExe service stop"
      $r = Invoke-Native $svcExe @("service", "stop")
      if ($r.Code -ne 0) { Warn "service stop: $($r.Err.Trim())" }
    } elseif ($switched) {
      Note "a running pyred keeps the old version until restarted (-Restart, or: pyred service stop; pyred service start)"
    }
    Write-Output "+ $svcExe service start"
    $r = Invoke-Native $svcExe @("service", "start")
    if ($r.Code -ne 0) { Die "pyred service start failed (exit $($r.Code)): $($r.Err.Trim())" }
    Note "the node task runs at logon in your interactive session; for unattended GUI testing configure auto-logon (docs/operations.md, Windows nodes)"
  }
  Note "done"
} finally {
  if ($Work -ne "" -and (Test-Path -LiteralPath $Work)) { Remove-Item -LiteralPath $Work -Recurse -Force -ErrorAction SilentlyContinue }
}
exit $exitCode
