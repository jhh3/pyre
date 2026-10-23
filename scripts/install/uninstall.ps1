<#
.SYNOPSIS
Remove a per-user Pyre installation on Windows (the counterpart of uninstall.sh).

.DESCRIPTION
  uninstall.ps1 [-Purge [-IncludeProfiles] [-Yes]] [-State DIR] [-KeepPath] [-Root DIR] [-DryRun]

Stops and unregisters the node logon task (`pyred service stop`, `pyred
service uninstall`), removes the version directories, the `current`
junction, .previous, the bundled tools (tools\, install.ps1 -WithTools) and
the .pyre-install marker under %LOCALAPPDATA%\Programs\Pyre, and the PATH
entries install.ps1 -EditPath added (unless -KeepPath). Refused (exit 3) when the prefix has no .pyre-install marker.

Data is kept unless -Purge: the node state %LOCALAPPDATA%\Pyre (database,
outbox, run records, logs, node credential) and the client config
%APPDATA%\Pyre\client.json. -Purge prints exactly what it would delete and
deletes only with -Yes. Account profiles (<state>\profiles) and the shared
transcript store (<state>\shared) are kept even with -Purge unless
-IncludeProfiles. %APPDATA%\Pyre is never deleted as a directory (it holds the
pinned release key).

Stopping pyred does not stop runs: their terminal hosts keep running until
they exit. Junctions are removed as links; what they point to is never followed.
#>
[CmdletBinding()]
param(
  [switch]$Purge,
  [switch]$IncludeProfiles,
  [switch]$Yes,
  [string]$State = "",
  [switch]$KeepPath,
  [string]$Root = "",
  [switch]$DryRun
)
Set-StrictMode -Version 2.0
$ErrorActionPreference = "Stop"
$Marker = "pyre-managed"

function Note([string]$m) { Write-Output "==> $m" }
function Warn([string]$m) { [Console]::Error.WriteLine("warning: $m") }
function Die([string]$m, [int]$code = 1) { [Console]::Error.WriteLine("error: $m"); exit $code }
function Abs([string]$p) { if ($p -eq "") { return "" }; return $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($p) }
function Under-Root([string]$p) { if ($Root -eq "") { return $p }; return (Join-Path $Root ($p -replace '^[A-Za-z]:', '')) }
function Is-Reparse([string]$p) {
  $i = Get-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
  if ($null -eq $i) { return $false }
  return [bool]($i.Attributes -band [System.IO.FileAttributes]::ReparsePoint)
}
# Remove-TreeSafe P: deletes P; junctions/symlinks (and AF_UNIX socket files)
# are removed as links and never followed.
function Remove-TreeSafe([string]$p) {
  $i = Get-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
  if ($null -eq $i) { return }
  if ($DryRun) { Write-Output "[dry-run] remove $p"; return }
  if ($i.Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
    if ($i.PSIsContainer) { [System.IO.Directory]::Delete($i.FullName, $false) } else { [System.IO.File]::Delete($i.FullName) }
    return
  }
  if ($i.PSIsContainer) {
    foreach ($c in @(Get-ChildItem -LiteralPath $i.FullName -Force)) { Remove-TreeSafe $c.FullName }
    [System.IO.Directory]::Delete($i.FullName, $false)
  } else {
    $i.Attributes = [System.IO.FileAttributes]::Normal
    [System.IO.File]::Delete($i.FullName)
  }
}
function Run-Pyred([string]$exe, [string[]]$a) {
  if ($DryRun -or $Root -ne "") { Write-Output "[service-manager, not run] $exe $($a -join ' ')"; return }
  Write-Output "+ $exe $($a -join ' ')"
  & $exe @a
  if ($LASTEXITCODE -ne 0) { Warn "$exe $($a -join ' ') exited $LASTEXITCODE" }
}

$State = Abs $State; $Root = Abs $Root
$LocalAppData = $env:LOCALAPPDATA
$AppData = $env:APPDATA
if (-not $LocalAppData -or -not $AppData) { Die "LOCALAPPDATA/APPDATA are not set: run as the owning user in a normal Windows session" }
$Prefix = Join-Path $LocalAppData "Programs\Pyre"
$ConfigDir = Join-Path $AppData "Pyre"
if ($State -eq "") { $State = Join-Path $LocalAppData "Pyre" }
$CurBin = Join-Path $Prefix "current\bin"
$ToolsBin = Join-Path $Prefix "tools\outfit\bin"  # install.ps1 -WithTools -EditPath

$P = Under-Root $Prefix
if (Test-Path -LiteralPath $P) {
  if (-not (Test-Path -LiteralPath (Join-Path $P ".pyre-install"))) { Die "$Prefix has no .pyre-install marker; not a Pyre installation, nothing removed" 3 }
  $m = [System.IO.File]::ReadAllText((Join-Path $P ".pyre-install"))
  if (-not $m.Contains($Marker)) { Die "$Prefix\.pyre-install is not a Pyre marker; nothing removed" 3 }
}

# ---- node task ----
$exe = Under-Root (Join-Path $CurBin "pyred.exe")
if (Test-Path -LiteralPath $exe) {
  Run-Pyred $exe @("service", "stop")
  Run-Pyred $exe @("service", "uninstall")
} else {
  Note "no installed pyred.exe: if a 'Pyre' scheduled task remains, remove it in Task Scheduler (taskschd.msc)"
}

# ---- PATH ----
if (-not $KeepPath -and $Root -eq "") {
  $userPath = [string](Get-Item "HKCU:\Environment").GetValue("Path", "", "DoNotExpandEnvironmentNames")
  $parts = @($userPath -split ';' | Where-Object { $_ -ne "" })
  $keep = @($parts | Where-Object { $_.TrimEnd('\') -ine $CurBin -and $_.TrimEnd('\') -ine $ToolsBin })
  if ($keep.Count -ne $parts.Count) {
    if ($DryRun) { Write-Output "[dry-run] remove $CurBin and $ToolsBin from the user PATH" } else {
      [Microsoft.Win32.Registry]::SetValue("HKEY_CURRENT_USER\Environment", "Path", ($keep -join ';'), [Microsoft.Win32.RegistryValueKind]::ExpandString)
      [Environment]::SetEnvironmentVariable("PYRE_PATH_REFRESH", $null, "User")
    }
    Note "removed Pyre's directories from the user PATH"
  }
}

# ---- binaries ----
if (Test-Path -LiteralPath $P) {
  # Links first (current and pruned-version aliases), then real directories.
  foreach ($e in @(Get-ChildItem -LiteralPath $P -Force | Where-Object { Is-Reparse $_.FullName })) { Remove-TreeSafe $e.FullName }
  foreach ($e in @(Get-ChildItem -LiteralPath $P -Force -ErrorAction SilentlyContinue)) {
    if ($e.Name -eq "node-credential.json") { Note "kept $Prefix\node-credential.json (bootstrap credential; delete it yourself or use -Purge)"; continue }
    try { Remove-TreeSafe $e.FullName } catch { Warn "could not remove $($e.FullName): $($_.Exception.Message) (a pyred or pyre process may still run from it)" }
  }
  if (@(Get-ChildItem -LiteralPath $P -Force -ErrorAction SilentlyContinue).Count -eq 0) { Remove-TreeSafe $P }
  Note "removed $Prefix"
} else {
  Note "$Prefix not present"
}

# ---- data ----
if (-not $Purge) {
  Note "kept node state $State and client config $ConfigDir (-Purge -Yes deletes them)"
  exit 0
}
$targets = @()
$S = Under-Root $State
if (Test-Path -LiteralPath $S) {
  foreach ($e in @(Get-ChildItem -LiteralPath $S -Force)) {
    if (-not $IncludeProfiles -and ($e.Name -eq "profiles" -or $e.Name -eq "shared")) { Note "kept $($e.FullName) (account profiles / transcripts; -IncludeProfiles deletes them)"; continue }
    $targets += $e.FullName
  }
}
foreach ($f in @((Join-Path $ConfigDir "client.json"), (Join-Path $Prefix "node-credential.json"))) {
  if (Test-Path -LiteralPath (Under-Root $f)) { $targets += (Under-Root $f) }
}
if ($targets.Count -eq 0) { Note "nothing to purge"; exit 0 }
Write-Output "-Purge would delete:"
$targets | ForEach-Object { Write-Output "    $_" }
if (-not $Yes) { Note "not deleted: re-run with -Purge -Yes"; exit 0 }
foreach ($t in $targets) { Remove-TreeSafe $t }
if ($IncludeProfiles -and (Test-Path -LiteralPath $S) -and @(Get-ChildItem -LiteralPath $S -Force).Count -eq 0) { Remove-TreeSafe $S }
Note "purged"
