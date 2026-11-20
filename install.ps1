$ErrorActionPreference = "Stop"
$repo = "NetCore-Technologies/boxr"

if ($env:BOXR_VERSION) {
    $version = $env:BOXR_VERSION
} else {
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest"
    $version = $release.tag_name
}
if (-not $version) {
    throw "Could not determine the latest boxr release."
}

$asset = "boxr-$version-windows-amd64.zip"
$checksums = "boxr-$version-checksums.txt"
$baseUrl = "https://github.com/$repo/releases/download/$version"
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ([System.Guid]::NewGuid().ToString())
$installDir = if ($env:BOXR_INSTALL_DIR) { $env:BOXR_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\boxr" }

New-Item -ItemType Directory -Path $tempDir -Force | Out-Null
try {
    $archivePath = Join-Path $tempDir $asset
    $checksumPath = Join-Path $tempDir $checksums
    Invoke-WebRequest -Uri "$baseUrl/$asset" -OutFile $archivePath
    Invoke-WebRequest -Uri "$baseUrl/$checksums" -OutFile $checksumPath

    $checksumLine = Get-Content $checksumPath | Where-Object { $_ -match ("\s+" + [regex]::Escape($asset) + "$" ) } | Select-Object -First 1
    if (-not $checksumLine) {
        throw "Release checksum manifest does not contain $asset"
    }
    $expectedHash = $checksumLine.Substring(0, 64).ToLowerInvariant()
    $actualHash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $expectedHash) {
        throw "SHA-256 verification failed for $asset"
    }

    $extractDir = Join-Path $tempDir "extract"
    Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Move-Item -Path (Join-Path $extractDir "boxr-windows-amd64.exe") -Destination (Join-Path $installDir "boxr.exe") -Force

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $entries = @($userPath -split ";" | Where-Object { $_ })
    if ($entries -notcontains $installDir) {
        $newPath = (@($entries) + $installDir) -join ";"
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
    }
    if (($env:Path -split ";") -notcontains $installDir) {
        $env:Path = "$installDir;$env:Path"
    }

    Write-Host "Installed boxr $version to $installDir"
    Write-Host "Open a new terminal, then run: boxr version"
    Write-Host "Note: container execution requires a Linux kernel."
} finally {
    Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
