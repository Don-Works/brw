<#
.SYNOPSIS
  Build the brw Windows MSI, Authenticode signed when a certificate is configured.

.DESCRIPTION
  Signing is driven entirely by the environment. With none of it set the script
  produces an unsigned MSI, which is what an unreleased local build wants.

    WINDOWS_SIGN_THUMBPRINT       SHA-1 thumbprint of an already-installed cert
    WINDOWS_CERTIFICATE_PATH      .pfx to import instead (removed again afterwards)
    WINDOWS_CERTIFICATE_PASSWORD  password for that .pfx
    WINDOWS_SIGN_DLIB             signtool /dlib for a cloud signing service
    WINDOWS_SIGN_DMDF             signtool /dmdf metadata file for that service
    WINDOWS_TIMESTAMP_URL         RFC 3161 timestamp authority
    BRW_SIGNING_REPORT            file to append a Markdown signing summary to

  Since June 2023 a publicly trusted code-signing key may not leave hardware, so
  a newly bought certificate arrives as a USB token or a cloud service rather
  than a .pfx. The /dlib pair is how those services plug into signtool.
#>
param(
  [Parameter(Mandatory = $true)]
  [string] $Version,

  [ValidateSet("amd64", "arm64")]
  [string] $Arch = "amd64",

  [string] $OutDir = "dist/release"
)

$ErrorActionPreference = "Stop"

if ($Version.StartsWith("v")) {
  throw "Version must not include the leading v: $Version"
}

if ($Version -notmatch "^([0-9]+\.[0-9]+\.[0-9]+)([.-].*)?$") {
  throw "Version must start with x.y.z: $Version"
}
$MsiVersion = $Matches[1]

# A native tool that fails sets $LASTEXITCODE but does not trip
# $ErrorActionPreference, so every invocation has to be checked by hand.
function Assert-LastExitCode {
  param([string] $What)
  if ($LASTEXITCODE -ne 0) {
    throw "$What failed with exit code $LASTEXITCODE"
  }
}

function Write-Banner {
  param([string[]] $Lines)
  Write-Host ""
  Write-Host "=================================================================="
  foreach ($Line in $Lines) {
    Write-Host "  $Line"
  }
  Write-Host "=================================================================="
  Write-Host ""
}

function Resolve-SignTool {
  $OnPath = Get-Command signtool.exe -ErrorAction SilentlyContinue
  if ($OnPath) {
    return $OnPath.Source
  }
  $Roots = @(
    "${env:ProgramFiles(x86)}\Windows Kits\10\bin",
    "${env:ProgramFiles}\Windows Kits\10\bin"
  ) | Where-Object { $_ -and (Test-Path $_) }
  $Candidates = foreach ($Root in $Roots) {
    Get-ChildItem -Path $Root -Recurse -Filter signtool.exe -ErrorAction SilentlyContinue
  }
  # Newest SDK first; the x64 build signs arm64 payloads just as well, so host
  # architecture only decides which copy is reachable, never what it can sign.
  $Best = $Candidates |
    Where-Object { $_.FullName -match "\\x64\\signtool\.exe$" } |
    Sort-Object FullName -Descending |
    Select-Object -First 1
  if (-not $Best) {
    $Best = $Candidates | Sort-Object FullName -Descending | Select-Object -First 1
  }
  if (-not $Best) {
    throw "signtool.exe not found. Install the Windows SDK signing tools."
  }
  return $Best.FullName
}

$SignThumbprint = $env:WINDOWS_SIGN_THUMBPRINT
$CertificatePath = $env:WINDOWS_CERTIFICATE_PATH
$SignDlib = $env:WINDOWS_SIGN_DLIB
$SignDmdf = $env:WINDOWS_SIGN_DMDF
$TimestampUrl = if ($env:WINDOWS_TIMESTAMP_URL) { $env:WINDOWS_TIMESTAMP_URL } else { "http://timestamp.digicert.com" }
$SigningReport = $env:BRW_SIGNING_REPORT
$ImportedThumbprint = $null

if ($SignDlib) {
  if (-not (Test-Path $SignDlib)) {
    throw "WINDOWS_SIGN_DLIB does not exist"
  }
  if (-not $SignDmdf -or -not (Test-Path $SignDmdf)) {
    throw "WINDOWS_SIGN_DMDF is required with WINDOWS_SIGN_DLIB"
  }
}

$SignMode = if ($SignDlib -or $SignThumbprint -or $CertificatePath) { "authenticode" } else { "unsigned" }

if ($SignMode -eq "unsigned") {
  Write-Banner @(
    "brw Windows package: UNSIGNED",
    "",
    "No code-signing certificate is configured, so the MSI carries no",
    "Authenticode signature. SmartScreen will warn on download and block",
    "the install for anyone without an override.",
    "",
    "See docs/release-signing.md to turn on Authenticode signing."
  )
}

$SignTool = $null

function Invoke-Sign {
  param([string[]] $Paths)
  if ($SignMode -ne "authenticode") {
    return
  }
  # /tr with /td sha256 is the RFC 3161 timestamp: without it every signature
  # expires with the certificate instead of staying valid for what it signed.
  $Arguments = @(
    "sign",
    "/fd", "sha256",
    "/tr", $TimestampUrl,
    "/td", "sha256"
  )
  if ($SignDlib) {
    $Arguments += @("/dlib", $SignDlib, "/dmdf", $SignDmdf)
  } else {
    $Arguments += @("/sha1", $SignThumbprint)
  }
  $Arguments += @("/v") + $Paths
  & $SignTool @Arguments
  Assert-LastExitCode "signtool sign"
}

$RepoRoot = Split-Path -Parent $PSScriptRoot
$WorkDir = Join-Path $RepoRoot "dist/package/windows-$Arch"
$StageDir = Join-Path $WorkDir "image"
if ([System.IO.Path]::IsPathRooted($OutDir)) {
  $OutAbs = $OutDir
} else {
  $OutAbs = Join-Path $RepoRoot $OutDir
}

try {
  # Importing inside the try is what guarantees the finally below takes the
  # private key back out of the certificate store on every exit path.
  if ($SignMode -eq "authenticode") {
    $SignTool = Resolve-SignTool
    if (-not $SignDlib -and -not $SignThumbprint) {
      if (-not (Test-Path $CertificatePath)) {
        throw "WINDOWS_CERTIFICATE_PATH does not exist"
      }
      if (-not $env:WINDOWS_CERTIFICATE_PASSWORD) {
        throw "WINDOWS_CERTIFICATE_PASSWORD is required with WINDOWS_CERTIFICATE_PATH"
      }
      $Secure = ConvertTo-SecureString $env:WINDOWS_CERTIFICATE_PASSWORD -AsPlainText -Force
      $Imported = Import-PfxCertificate -FilePath $CertificatePath -CertStoreLocation Cert:\CurrentUser\My -Password $Secure
      $SignThumbprint = $Imported.Thumbprint
      $ImportedThumbprint = $Imported.Thumbprint
    }
  }

  Remove-Item -Recurse -Force $WorkDir -ErrorAction SilentlyContinue
  New-Item -ItemType Directory -Force -Path `
    (Join-Path $StageDir "bin"), `
    (Join-Path $StageDir "share/doc"), `
    $OutAbs | Out-Null

  $env:CGO_ENABLED = "0"
  $env:GOOS = "windows"
  $env:GOARCH = $Arch

  $Executables = @()
  Push-Location $RepoRoot
  try {
    foreach ($CommandName in @("brw", "brwd", "brwctl", "brwcheck", "brw-devtools-mcp")) {
      $Output = Join-Path $StageDir "bin/$CommandName.exe"
      & go build -trimpath -ldflags="-s -w -X github.com/Don-Works/brw/internal/mcp.Version=$Version -X github.com/Don-Works/brw/internal/cli.Version=$Version" -o $Output "./cmd/$CommandName"
      Assert-LastExitCode "go build ./cmd/$CommandName"
      $Executables += $Output
    }
  } finally {
    Pop-Location
  }

  Copy-Item -Recurse -Force (Join-Path $RepoRoot "extension") (Join-Path $StageDir "share/extension")
  Copy-Item -Recurse -Force (Join-Path $RepoRoot "tests") (Join-Path $StageDir "share/tests")
  Copy-Item -Recurse -Force (Join-Path $RepoRoot "skills") (Join-Path $StageDir "share/skills")
  Copy-Item -Force (Join-Path $RepoRoot "LICENSE") (Join-Path $StageDir "share/doc/LICENSE")
  Copy-Item -Force (Join-Path $RepoRoot "README.md") (Join-Path $StageDir "share/doc/README.md")

  # Sign the payload before WiX embeds it: an MSI signature covers the package,
  # not the files it lays down, so an unsigned brwd.exe stays unsigned on disk
  # and every SmartScreen and AppLocker decision after install sees no publisher.
  Invoke-Sign -Paths $Executables

  $Wix = Get-Command wix -ErrorAction SilentlyContinue
  if (-not $Wix) {
    throw "WiX is required. Install with: dotnet tool install --global wix"
  }

  $WixArch = if ($Arch -eq "amd64") { "x64" } else { "arm64" }
  $MsiPath = Join-Path $OutAbs "brw_${Version}_windows_${Arch}.msi"

  & wix build `
    (Join-Path $RepoRoot "packaging/windows/brw.wxs") `
    -arch $WixArch `
    -d "Version=$MsiVersion" `
    -d "SourceDir=$StageDir" `
    -out $MsiPath
  Assert-LastExitCode "wix build"

  Remove-Item -Force (Join-Path $OutAbs "brw_${Version}_windows_${Arch}.wixpdb") -ErrorAction SilentlyContinue

  Invoke-Sign -Paths @($MsiPath)

  Write-Host "brw-signing-mode: msi=$SignMode arch=$Arch"

  if ($SigningReport) {
    $ReportDir = Split-Path -Parent $SigningReport
    if ($ReportDir) {
      New-Item -ItemType Directory -Force -Path $ReportDir | Out-Null
    }
    $Lines = @("### Windows ($Arch .msi)", "")
    if ($SignMode -eq "authenticode") {
      $Lines += "- Installer and payload executables: Authenticode signed, SHA-256 digest, RFC 3161 timestamp from ``$TimestampUrl``."
    } else {
      $Lines += "- Installer and payload executables: **unsigned**. SmartScreen warns on download."
    }
    $Lines += ""
    Add-Content -Path $SigningReport -Value $Lines
  }
} finally {
  if ($ImportedThumbprint) {
    Remove-Item -Force "Cert:\CurrentUser\My\$ImportedThumbprint" -ErrorAction SilentlyContinue
  }
}
