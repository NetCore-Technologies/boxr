$repo = "NetCore-Technologies/boxr"
$release = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"
$version = $release.tag_name
$url = "https://github.com/$repo/releases/download/$version/boxr-$version-windows-amd64.zip"
Write-Host "Downloading boxr $version..."
Invoke-WebRequest -Uri $url -OutFile "boxr.zip"
Expand-Archive -Path "boxr.zip" -DestinationPath "$env:ProgramFiles\boxr" -Force
Write-Host "Done! boxr installed to $env:ProgramFiles\boxr"
