# Installs rustle-mcp and asks which of Claude Code, Codex and Cursor to add it to:
#
#   irm https://raw.githubusercontent.com/vibeslop/rustle-mcp/main/install.ps1 | iex
#
# Run it again to update. RUSTLE_INSTALL_DIR picks the folder (default
# %LOCALAPPDATA%\Programs\rustle-mcp); RUSTLE_VERSION a release (default the latest).
$ErrorActionPreference = 'Stop'
# Windows PowerShell redraws its progress bar for every block it downloads, which makes a
# download many times slower.
$ProgressPreference = 'SilentlyContinue'

$repo = 'vibeslop/rustle-mcp'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64' -or $env:PROCESSOR_ARCHITEW6432 -eq 'ARM64') { 'arm64' } else { 'amd64' }
$base = if ($env:RUSTLE_VERSION) { "https://github.com/$repo/releases/download/v$($env:RUSTLE_VERSION.TrimStart('v'))" } else { "https://github.com/$repo/releases/latest/download" }
$dir = if ($env:RUSTLE_INSTALL_DIR) { $env:RUSTLE_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\rustle-mcp' }
$name = "rustle-mcp_windows_$arch"
$exe = Join-Path $dir 'rustle-mcp.exe'

$work = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $work | Out-Null
try {
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$name.zip" -OutFile "$work\$name.zip"
    Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile "$work\checksums.txt"
    $line = Get-Content "$work\checksums.txt" | Where-Object { $_.EndsWith(" $name.zip") } | Select-Object -First 1
    $actual = (Get-FileHash -Algorithm SHA256 "$work\$name.zip").Hash.ToLower()
    if (-not $line -or $line.Split(' ')[0] -ne $actual) { throw 'rustle-mcp install: the download does not match its checksum' }
    Expand-Archive -Path "$work\$name.zip" -DestinationPath $work
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    # Windows can't overwrite a running program, but can rename it: a client's server keeps
    # running the old copy, and the next start runs the new one.
    if (Test-Path $exe) {
        Remove-Item "$exe.old" -Force -ErrorAction SilentlyContinue
        Move-Item $exe "$exe.old" -Force
    }
    Copy-Item "$work\$name\rustle-mcp.exe" $exe
} finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
Write-Host "Installed rustle-mcp $(& $exe --version) to $exe"

if (-not (Get-Command ffmpeg -ErrorAction SilentlyContinue)) {
    Write-Host 'Frames and sound need ffmpeg (winget install ffmpeg); transcripts and search work without it.'
}
& $exe setup @args
