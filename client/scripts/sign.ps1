# Signs Windows executables with Authenticode when a code signing certificate is configured.
# Without one it does nothing, so unsigned builds keep working.
#
#   Option 1, a .pfx file (OV/EV certificate exported with its private key):
#     WINDOWS_SIGNING_PFX_BASE64   the .pfx, base64 encoded
#     WINDOWS_SIGNING_PFX_PASSWORD its password
#   Option 2, any other signer (HSM, cloud key, Azure Trusted Signing CLI, ...):
#     WINDOWS_SIGN_COMMAND         a command line; {file} is replaced by the path to sign
#
#   WINDOWS_SIGNING_TIMESTAMP_URL  RFC 3161 time stamp server (default: DigiCert)
#
# Usage: pwsh client/scripts/sign.ps1 dist/agent/*.exe
param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Files)
$ErrorActionPreference = "Stop"

$ts = if ($env:WINDOWS_SIGNING_TIMESTAMP_URL) { $env:WINDOWS_SIGNING_TIMESTAMP_URL } else { "http://timestamp.digicert.com" }
$paths = $Files | ForEach-Object { Get-ChildItem $_ } | ForEach-Object { $_.FullName }

if ($env:WINDOWS_SIGN_COMMAND) {
  foreach ($f in $paths) {
    $cmd = $env:WINDOWS_SIGN_COMMAND.Replace("{file}", "`"$f`"")
    Write-Host "Signing $f with WINDOWS_SIGN_COMMAND"
    cmd /c $cmd
    if ($LASTEXITCODE -ne 0) { throw "signing failed for $f" }
  }
  exit 0
}

if (-not $env:WINDOWS_SIGNING_PFX_BASE64) {
  Write-Host "No code signing certificate configured; leaving the executables unsigned."
  exit 0
}

$signtool = Get-ChildItem "${env:ProgramFiles(x86)}\Windows Kits\10\bin\*\x64\signtool.exe" | Sort-Object FullName -Descending | Select-Object -First 1
if (-not $signtool) { throw "signtool.exe not found (install the Windows SDK)" }
$pfx = Join-Path $env:RUNNER_TEMP "signing.pfx"
[IO.File]::WriteAllBytes($pfx, [Convert]::FromBase64String($env:WINDOWS_SIGNING_PFX_BASE64))
try {
  foreach ($f in $paths) {
    Write-Host "Signing $f"
    & $signtool.FullName sign /fd SHA256 /td SHA256 /tr $ts /f $pfx /p $env:WINDOWS_SIGNING_PFX_PASSWORD /d "RHIS Remote" $f
    if ($LASTEXITCODE -ne 0) { throw "signtool failed for $f" }
    & $signtool.FullName verify /pa $f
    if ($LASTEXITCODE -ne 0) { throw "signature check failed for $f" }
  }
} finally {
  Remove-Item $pfx -Force -ErrorAction SilentlyContinue
}
