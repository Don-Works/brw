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

$RepoRoot = Split-Path -Parent $PSScriptRoot
$WorkDir = Join-Path $RepoRoot "dist/package/windows-$Arch"
$StageDir = Join-Path $WorkDir "image"
if ([System.IO.Path]::IsPathRooted($OutDir)) {
  $OutAbs = $OutDir
} else {
  $OutAbs = Join-Path $RepoRoot $OutDir
}

Remove-Item -Recurse -Force $WorkDir -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force -Path `
  (Join-Path $StageDir "bin"), `
  (Join-Path $StageDir "share/doc"), `
  $OutAbs | Out-Null

$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = $Arch

Push-Location $RepoRoot
foreach ($CommandName in @("brwd", "brwctl", "brwcheck", "brw-devtools-mcp")) {
  $Output = Join-Path $StageDir "bin/$CommandName.exe"
  & go build -trimpath -ldflags="-s -w" -o $Output "./cmd/$CommandName"
}
Pop-Location

Copy-Item -Recurse -Force (Join-Path $RepoRoot "extension") (Join-Path $StageDir "share/extension")
Copy-Item -Recurse -Force (Join-Path $RepoRoot "tests") (Join-Path $StageDir "share/tests")
Copy-Item -Force (Join-Path $RepoRoot "LICENSE") (Join-Path $StageDir "share/doc/LICENSE")
Copy-Item -Force (Join-Path $RepoRoot "README.md") (Join-Path $StageDir "share/doc/README.md")

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

Remove-Item -Force (Join-Path $OutAbs "brw_${Version}_windows_${Arch}.wixpdb") -ErrorAction SilentlyContinue
