<#
.SYNOPSIS
  Give one Mac its own revocable client key on the PC hub proxy.

.DESCRIPTION
  hub-add-device.ps1 -Device <d> [-Apply] [-OutFile <p>] [-Revoke]

  Dry run unless -Apply. Generates a key on this PC that reads
  dev.<device>.<64 hex>, adds it to the client keys (access.api-keys) of the
  proxy config and writes it, readable by this user only, to -OutFile when
  given, else to <KeyDir>\<device>.key. The hub dashboard's Sessions screen
  names the device from the key. The config is backed up first and swapped in
  one step; the proxy reloads client keys by itself, no restart.
  -Revoke removes the device's key from the config and deletes
  <KeyDir>\<device>.key. Never prints a key. Windows PowerShell 5.1.
  -Config and -KeyDir override the defaults. Refuses any path inside
  $env:USERPROFILE\.cli-proxy-api, which holds the logins. See docs/hub-routing.md.

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File hub-add-device.ps1 -Device macbook-air
  powershell -NoProfile -ExecutionPolicy Bypass -File hub-add-device.ps1 -Device macbook-air -Apply
  powershell -NoProfile -ExecutionPolicy Bypass -File hub-add-device.ps1 -Device macbook-air -Apply -OutFile C:\Users\User\Downloads\macbook-air.key
  powershell -NoProfile -ExecutionPolicy Bypass -File hub-add-device.ps1 -Device macbook-air -Revoke -Apply
#>
[CmdletBinding()]
param(
    # -cmatch: ValidatePattern ignores case, and the hub only names lower case devices.
    [Parameter(Mandatory = $true)]
    [ValidateScript({ $_ -cmatch '^[a-z0-9][a-z0-9-]{0,62}$' })]
    [string]$Device,
    [string]$OutFile = '',
    [switch]$Apply,
    [switch]$Revoke,
    [string]$Config = 'C:\Users\User\cliproxy\config.yaml',
    [string]$KeyDir = 'C:\Users\User\cliproxy\hub-keys'
)
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Fail([string]$why) { Write-Error "hub-add-device: $why" -ErrorAction Continue; exit 1 }
function Indent([string]$l) { return $l.Length - $l.TrimStart(' ').Length }
function IsSkip([string]$l) { return $l -match '^\s*(#.*)?$' }
function Restrict([string]$path, [string]$grant) {
    & icacls $path /inheritance:r /grant:r "$($env:USERNAME):$grant" | Out-Null
    return ($LASTEXITCODE -eq 0)
}
# Full path; .NET resolves relative paths against its own working directory,
# not PowerShell's, and GetFullPath folds any "..".
function FullPath([string]$p) { return [System.IO.Path]::GetFullPath($ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($p)) }

if ($Revoke -and $OutFile) { Fail '-OutFile only applies when adding a key; nothing written' }
$Config = FullPath $Config
$KeyDir = FullPath $KeyDir
$keyFile = Join-Path $KeyDir "$Device.key"
$target = $keyFile
if ($OutFile) { $target = FullPath $OutFile }
# The proxy's .cli-proxy-api dir holds the logins: this script never goes there.
$authHome = [System.IO.Path]::GetFullPath($env:USERPROFILE + '\.cli-proxy-api').TrimEnd('\')
foreach ($p in @($Config, $KeyDir, $target)) {
    $q = $p.TrimEnd('\')
    if ($q -ieq $authHome -or $q.StartsWith($authHome + '\', [StringComparison]::OrdinalIgnoreCase)) {
        Fail "$p is inside $authHome, which holds the logins; pick another path; nothing written"
    }
}
if (-not (Test-Path -LiteralPath $Config -PathType Leaf)) { Fail "config $Config not found; nothing written" }
if ($OutFile) {
    $parentDir = [System.IO.Path]::GetDirectoryName($target)
    if (-not $parentDir -or -not (Test-Path -LiteralPath $parentDir -PathType Container)) { Fail "the folder for -OutFile $target does not exist; nothing written" }
    if (Test-Path -LiteralPath $target) { Fail "-OutFile $target already exists; remove it or pick another path; nothing written" }
}

$utf8 = New-Object System.Text.UTF8Encoding $false
$text = [System.IO.File]::ReadAllText($Config)
$nl = if ($text.Contains("`r`n")) { "`r`n" } else { "`n" }
$lines = New-Object System.Collections.Generic.List[string]
foreach ($l in ($text -split "`r?`n")) { $lines.Add($l) }
if ($text.EndsWith("`n")) { $lines.RemoveAt($lines.Count - 1) }

# The client key list: access.api-keys in a v8 config, top-level api-keys in
# an older one. In v8 a top-level api-keys holds upstream keys: never touch it.
$v8 = [bool]($lines | Where-Object { $_ -match '^config-version:\s*8\s*$' })
$parentPattern = if ($v8) { '^access:\s*(#.*)?$' } else { '^api-keys:\s*(#.*)?$' }
$parent = -1
for ($i = 0; $i -lt $lines.Count; $i++) { if ($lines[$i] -match $parentPattern) { $parent = $i; break } }
if ($parent -lt 0) { Fail "no $(if ($v8) { 'access.api-keys' } else { 'api-keys' }) block in $Config; nothing written" }
$list = $parent
if ($v8) {
    $list = -1
    for ($i = $parent + 1; $i -lt $lines.Count; $i++) {
        if (IsSkip $lines[$i]) { continue }
        if ((Indent $lines[$i]) -eq 0) { break }
        if ($lines[$i] -match '^  api-keys:\s*(#.*)?$') { $list = $i; break }
    }
    if ($list -lt 0) { Fail "access has no block list api-keys in $Config; nothing written" }
}
$first = -1
for ($i = $list + 1; $i -lt $lines.Count; $i++) { if (-not (IsSkip $lines[$i])) { $first = $i; break } }
if ($first -lt 0 -or $lines[$first] -notmatch '^\s*- ' -or (Indent $lines[$first]) -lt (Indent $lines[$list])) {
    Fail "the client key list in $Config is not a block list; nothing written"
}
$itemIndent = Indent $lines[$first]
$end = $lines.Count
for ($i = $first + 1; $i -lt $lines.Count; $i++) {
    if (IsSkip $lines[$i]) { continue }
    $ind = Indent $lines[$i]
    if ($ind -lt $itemIndent -or ($ind -eq $itemIndent -and $lines[$i] -notmatch '^\s*- ')) { $end = $i; break }
}
while ($end - 1 -gt $first -and (IsSkip $lines[$end - 1])) { $end-- }

$marker = "dev.$Device."
$mine = @()
for ($i = $first; $i -lt $end; $i++) { if ($lines[$i].Contains($marker)) { $mine += $i } }

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$backup = "$Config.bak-hubdev-$stamp"

# Same directory, then one Replace: the config is never half written, and the
# original becomes the backup with its permissions.
function Save-Config {
    $tmp = "$Config.hubdev-$PID.tmp"
    $out = ($lines -join $nl)
    if ($text.EndsWith("`n")) { $out += $nl }
    try {
        [System.IO.File]::WriteAllText($tmp, $out, $utf8)
        [System.IO.File]::Replace($tmp, $Config, $backup)
    } catch {
        if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Force }
        return $_.Exception.Message
    }
    return ''
}

if ($Revoke) {
    $hasFile = Test-Path -LiteralPath $keyFile -PathType Leaf
    if ($mine.Count -eq 0 -and -not $hasFile) { Write-Output "no key for $Device in $Config and no $keyFile; nothing to revoke"; exit 0 }
    if (-not $Apply) {
        if ($mine.Count -gt 0) { Write-Output "dry run: would remove $($mine.Count) key line(s) for $Device from $Config (lines $(($mine | ForEach-Object { $_ + 1 }) -join ', ')), backup $backup" }
        if ($hasFile) { Write-Output "dry run: would delete $keyFile" }
        Write-Output 'run again with -Apply to do it'
        exit 0
    }
    if ($mine.Count -gt 0) {
        for ($j = $mine.Count - 1; $j -ge 0; $j--) { $lines.RemoveAt($mine[$j]) }
        $err = Save-Config
        if ($err) { Fail "could not write $Config ($err); the key is still live" }
        Write-Output "revoked: removed the key for $Device from $Config"
        Write-Output "backup: $backup"
        Write-Output 'the proxy reloads client keys by itself; that Mac now gets 401 from the hub and falls back to its own logins'
    }
    if ($hasFile) {
        Remove-Item -LiteralPath $keyFile -Force
        Write-Output "deleted: $keyFile"
    }
    exit 0
}

if ($mine.Count -gt 0) { Fail "$Device already has a key in $Config (line $($mine[0] + 1)); revoke it first with -Revoke -Apply; nothing written" }
if (-not $Apply) {
    Write-Output "dry run: would add a new key for $Device to $Config after line $end, backup $backup, and write the key to $target"
    Write-Output 'run again with -Apply to do it'
    exit 0
}
$bytes = New-Object byte[] 32
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
$rng.Dispose()
$key = $marker + (($bytes | ForEach-Object { $_.ToString('x2') }) -join '')

# Key file first: if the config write then fails, the key was never live. The
# file is created empty and restricted before the key goes in; truncating it
# keeps that ACL.
if (-not $OutFile -and -not (Test-Path -LiteralPath $KeyDir)) {
    New-Item -ItemType Directory -Path $KeyDir | Out-Null
    if (-not (Restrict $KeyDir '(OI)(CI)F')) { Fail "could not restrict $KeyDir to $($env:USERNAME); nothing written to the config" }
}
[System.IO.File]::WriteAllText($target, '', $utf8)
if (-not (Restrict $target 'F')) {
    Remove-Item -LiteralPath $target -Force
    Fail "could not restrict $target to $($env:USERNAME); nothing written"
}
[System.IO.File]::WriteAllText($target, $key, $utf8)

$lines.Insert($end, (' ' * $itemIndent) + '- "' + $key + '"')
$err = Save-Config
if ($err) {
    Remove-Item -LiteralPath $target -Force
    Fail "could not write $Config ($err); the key file was removed and no key is live"
}
$sha = [System.Security.Cryptography.SHA256]::Create()
$fingerprint = (($sha.ComputeHash($utf8.GetBytes($key)) | ForEach-Object { $_.ToString('x2') }) -join '').Substring(0, 12)
$sha.Dispose()

Write-Output "added: a key for $Device to $Config (fingerprint $fingerprint, the first 12 hex of its SHA-256)"
Write-Output "backup: $backup"
Write-Output "key file: $target (readable by $($env:USERNAME) only)"
Write-Output 'the proxy reloads client keys by itself, no restart'
if ($OutFile) {
    Write-Output 'send that file to the Mac, then delete it here'
} else {
    # scp paths on this PC resolve against the profile directory.
    $remote = $target
    if ($remote.StartsWith($env:USERPROFILE + '\', [StringComparison]::OrdinalIgnoreCase)) { $remote = $remote.Substring($env:USERPROFILE.Length + 1) }
    Write-Output "next, on the Mac: scp 'jacks-desktop:$($remote -replace '\\', '/')' ~/.cli-proxy-api/hub.key; chmod 600 ~/.cli-proxy-api/hub.key"
}
