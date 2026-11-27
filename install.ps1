# Install or update GitHub CLI Throttle on Windows (PowerShell 5.1 or newer):
#
#   irm https://raw.githubusercontent.com/Qiming-Liu/gh-throttle/gh-throttle/install.ps1 | iex
#
# Run the same command again to update. If the official GitHub CLI is
# installed, it offers to uninstall it, and only does so after you answer y.
#
# Environment:
#   GH_THROTTLE_VERSION=2.106.0      install this release instead of the latest
#   GH_THROTTLE_INSTALL_DIR=DIR      where it goes (default %LOCALAPPDATA%\Programs\gh-throttle)
#   GH_THROTTLE_NO_MODIFY_PATH=1     leave the user PATH alone

& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'Qiming-Liu/gh-throttle'
    $installDir = $env:GH_THROTTLE_INSTALL_DIR
    if (-not $installDir) { $installDir = Join-Path $env:LOCALAPPDATA 'Programs\gh-throttle' }
    $binDir = Join-Path $installDir 'bin'
    $target = Join-Path $binDir 'gh.exe'

    # Invoke-Native runs a program quietly and returns its exit code. Windows
    # PowerShell 5.1 turns redirected stderr into errors, which 'Stop' would
    # make fatal, so it runs with 'Continue'.
    function Invoke-Native([string]$exe, [string[]]$arguments) {
        $ErrorActionPreference = 'Continue'
        & $exe @arguments *> $null
        return $LASTEXITCODE
    }

    # Test-Yes is true only when the user types y or Y. Without a console to
    # ask (CI, redirected input) the answer is no.
    function Test-Yes([string]$question) {
        if ([Console]::IsInputRedirected) { return $false }
        try { $answer = Read-Host "$question [y/N]" } catch { return $false }
        return ($answer -ceq 'y' -or $answer -ceq 'Y')
    }

    function Get-LatestTag {
        $url = "https://github.com/$repo/releases/latest"
        $req = [Net.HttpWebRequest]::Create($url)
        $req.AllowAutoRedirect = $false
        $req.UserAgent = 'gh-throttle-installer'
        $resp = $req.GetResponse()
        try { $location = $resp.Headers['Location'] } finally { $resp.Close() }
        $tag = ($location -split '/')[-1]
        if ($tag -notlike 'v*-throttle') { throw "could not find the latest release at $url" }
        return $tag
    }

    # Get-UninstallCommand returns the command that removes the official gh at
    # $path, if a known package manager installed it.
    function Get-UninstallCommand([string]$path) {
        if ($path -like '*\scoop\*' -and (Get-Command scoop -ErrorAction SilentlyContinue)) {
            return 'scoop uninstall gh'
        }
        if ($path -like '*\chocolatey\*' -and (Get-Command choco -ErrorAction SilentlyContinue)) {
            return 'choco uninstall gh -y'
        }
        if (Get-Command winget -ErrorAction SilentlyContinue) {
            if ((Invoke-Native 'winget' @('list', '--id', 'GitHub.cli', '--exact', '--accept-source-agreements')) -eq 0) {
                return 'winget uninstall --id GitHub.cli --exact'
            }
        }
        return $null
    }

    # --- platform ---
    $cpu = $env:PROCESSOR_ARCHITEW6432
    if (-not $cpu) { $cpu = $env:PROCESSOR_ARCHITECTURE }
    switch ($cpu) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "unsupported CPU $cpu" }
    }

    # --- release ---
    if ($env:GH_THROTTLE_VERSION) {
        $tag = 'v' + ($env:GH_THROTTLE_VERSION -replace '^v', '' -replace '-throttle$', '') + '-throttle'
    } else {
        $tag = Get-LatestTag
    }
    $version = $tag.Substring(1)
    $name = "gh_${version}_windows_$arch"
    Write-Host "Installing GitHub CLI Throttle $version (windows/$arch)"

    $tmp = Join-Path ([IO.Path]::GetTempPath()) ("gh-throttle-" + [Guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmp | Out-Null
    try {
        $base = "https://github.com/$repo/releases/download/$tag"
        $zip = Join-Path $tmp "$name.zip"
        $sums = Join-Path $tmp 'checksums.txt'
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$name.zip" -OutFile $zip
        Invoke-WebRequest -UseBasicParsing -Uri "$base/gh_${version}_checksums.txt" -OutFile $sums
        $want = $null
        foreach ($line in Get-Content $sums) {
            $parts = $line -split '\s+'
            if ($parts.Count -ge 2 -and $parts[1] -eq "$name.zip") { $want = $parts[0] }
        }
        if (-not $want) { throw "$name.zip is not in the checksums file" }
        $got = (Get-FileHash -Algorithm SHA256 $zip).Hash
        if ($got -ne $want) { throw "checksum mismatch for $name.zip" }
        Expand-Archive -Path $zip -DestinationPath $tmp -Force

        # --- other gh on PATH ---
        $others = Get-Command gh -All -CommandType Application -ErrorAction SilentlyContinue |
            ForEach-Object { $_.Source } |
            Where-Object { $_ -and ($_ -ne $target) } |
            Select-Object -Unique
        $kept = @()
        foreach ($g in $others) {
            if ((Invoke-Native $g @('throttle', '--help')) -eq 0) {
                Write-Warning "another copy of GitHub CLI Throttle is at $g; remove it if you don't need it"
                continue
            }
            Write-Host "Found the official GitHub CLI at $g"
            $cmd = Get-UninstallCommand $g
            if (-not $cmd) {
                Write-Warning "can't tell how $g was installed; please uninstall it yourself"
                $kept += $g
            } elseif (Test-Yes "Uninstall it with '$cmd'?") {
                $global:LASTEXITCODE = 0
                Invoke-Expression $cmd
                if ($LASTEXITCODE -ne 0) {
                    Write-Warning "uninstall failed; please remove $g yourself"
                    $kept += $g
                }
            } else {
                $kept += $g
            }
        }

        # --- install ---
        New-Item -ItemType Directory -Force -Path $binDir | Out-Null
        try {
            Copy-Item -Force (Join-Path $tmp "$name\bin\gh.exe") $target
        } catch {
            throw "could not replace $target; close any running gh and try again"
        }
        Copy-Item -Force (Join-Path $tmp "$name\LICENSE"), (Join-Path $tmp "$name\LICENSE-ghx") $installDir
        if ((Invoke-Native $target @('--version')) -ne 0) { throw "$target does not run" }
        Write-Host "Installed $target"
    } finally {
        Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    }

    # --- PATH ---
    if ($env:GH_THROTTLE_NO_MODIFY_PATH -ne '1') {
        $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $entries = @($userPath -split ';' | Where-Object { $_ -and ($_.TrimEnd('\') -ne $binDir.TrimEnd('\')) })
        $newPath = (@($binDir) + $entries) -join ';'
        if ($newPath -ne $userPath) {
            [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
            Write-Host "Put $binDir first on your user PATH"
        }
        $env:Path = $binDir + ';' + (($env:Path -split ';' | Where-Object { $_ -and ($_.TrimEnd('\') -ne $binDir.TrimEnd('\')) }) -join ';')
    }
    foreach ($g in $kept) {
        Write-Warning "$g is still installed. Windows puts the system PATH before the user PATH, so it may run instead of GitHub CLI Throttle."
    }

    Write-Host ''
    Write-Host 'Done. Open a new terminal, then:'
    Write-Host '  gh auth login      # if you haven''t signed in yet'
    Write-Host '  gh throttle status'
}
