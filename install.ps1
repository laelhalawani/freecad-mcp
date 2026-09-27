# Installs freecad-mcp from GitHub releases and runs its setup wizard.
# Cancelling the wizard undoes everything this script changed.
#
#   irm https://github.com/laelhalawani/freecad-mcp/releases/latest/download/install.ps1 | iex
#
# To pass parameters (a specific release, or flags for the wizard):
#   & ([scriptblock]::Create((irm <url>))) -Version v0.2.1 -ConfigureArgs "--yes"
#
# The whole script is one script block: with `irm ... | iex` it runs in the
# caller's own session, and this keeps its variables, functions and
# preference settings out of that session and never calls `exit` in it.
& {
  param(
    [string]$Owner = "laelhalawani",
    [string]$Repo = "freecad-mcp",
    [string]$Bin = "freecad-mcp",
    [string]$Version = "latest",
    [string[]]$ConfigureArgs = @()
  )
  $ErrorActionPreference = "Stop"
  # Windows PowerShell's progress bar slows downloads down many times over.
  $ProgressPreference = "SilentlyContinue"
  # `freecad-mcp configure` exits with 3 when the wizard is left before it
  # changed anything.
  $exitCancelled = 3

  # --- user PATH, read and written in the registry as stored ---------------
  # [Environment]::GetEnvironmentVariable expands %VAR% entries and
  # SetEnvironmentVariable writes REG_SZ, which would turn a REG_EXPAND_SZ
  # PATH into fixed paths; the registry API keeps both as they are.
  function Get-UserPath {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey("Environment")
    try {
      if ($key.GetValueNames() -notcontains "Path") {
        return @{ Value = ""; Kind = [Microsoft.Win32.RegistryValueKind]::ExpandString }
      }
      return @{
        Value = $key.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        Kind  = $key.GetValueKind("Path")
      }
    } finally { $key.Close() }
  }
  function Set-UserPath([string]$value, $kind) {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey("Environment", $true)
    try { $key.SetValue("Path", $value, $kind) } finally { $key.Close() }
    # Tell Explorer and new terminals, as SetEnvironmentVariable would.
    try {
      if (-not ("FreecadMcpInstaller.NativeMethods" -as [type])) {
        Add-Type -Namespace FreecadMcpInstaller -Name NativeMethods -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("user32.dll", SetLastError = true, CharSet = System.Runtime.InteropServices.CharSet.Unicode)]
public static extern System.IntPtr SendMessageTimeout(System.IntPtr hWnd, uint Msg, System.UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out System.UIntPtr lpdwResult);
'@
      }
      $result = [System.UIntPtr]::Zero
      [void][FreecadMcpInstaller.NativeMethods]::SendMessageTimeout([System.IntPtr]0xffff, 0x1a, [System.UIntPtr]::Zero, "Environment", 2, 5000, [ref]$result)
    } catch { }
  }
  function Test-SameDir([string]$a, [string]$b) {
    return $a.Trim().TrimEnd("\") -ieq $b.Trim().TrimEnd("\")
  }

  # A 32-bit PowerShell on 64-bit Windows sees x86 here and the machine's
  # own architecture in PROCESSOR_ARCHITEW6432.
  $machineArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
  switch ($machineArch) {
    { $_ -in 'AMD64', 'x64' } { $arch = 'amd64' }
    'ARM64'                   { $arch = 'arm64' }
    default { Write-Host "  Unsupported architecture: $machineArch" -ForegroundColor Red; return }
  }

  # "--yes --clients cursor" given as one string is split into its flags.
  # The array form ("--yes", "--token", "a b") is passed as it is, so a
  # value containing spaces survives.
  if ($ConfigureArgs.Count -eq 1) {
    $ConfigureArgs = @($ConfigureArgs[0] -split '\s+' | Where-Object { $_ })
  }

  $asset = "$Bin-windows-$arch.exe"
  if ($Version -eq "latest") {
    $base = "https://github.com/$Owner/$Repo/releases/latest/download"
  } else {
    $base = "https://github.com/$Owner/$Repo/releases/download/$Version"
  }
  $localAppData = if ($env:LOCALAPPDATA) { $env:LOCALAPPDATA } else { Join-Path $env:USERPROFILE "AppData\Local" }
  $installRoot = Join-Path $localAppData $Repo
  $installDir = Join-Path $installRoot "bin"
  $target = Join-Path $installDir "$Bin.exe"

  # Record what exists now, so a cancelled setup can put it back.
  $createdRoot = -not (Test-Path $installRoot)
  $createdDir = -not (Test-Path $installDir)
  function Undo-Directories {
    if ($createdDir -and (Test-Path $installDir) -and -not (Get-ChildItem $installDir -Force)) {
      Remove-Item $installDir -Force -ErrorAction SilentlyContinue
    }
    if ($createdRoot -and (Test-Path $installRoot) -and -not (Get-ChildItem $installRoot -Force)) {
      Remove-Item $installRoot -Force -ErrorAction SilentlyContinue
    }
  }

  New-Item -ItemType Directory -Force -Path $installDir | Out-Null

  Write-Host "  $Bin installer"
  Write-Host "  Downloading $asset ($Version)..."
  try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -Uri "$base/$asset" -OutFile "$target.new" -UseBasicParsing
  } catch {
    Write-Host "  Download failed: $_" -ForegroundColor Red
    Remove-Item "$target.new" -Force -ErrorAction SilentlyContinue
    Undo-Directories
    return
  }

  # Verify the SHA256 checksum; install nothing that cannot be verified.
  $checksumUrl = "$base/SHA256SUMS.txt"
  $checksums = $null
  try {
    $checksums = (Invoke-WebRequest -Uri $checksumUrl -UseBasicParsing).Content
    # GitHub serves release assets as application/octet-stream, for which
    # Windows PowerShell returns the content as bytes rather than text.
    if ($checksums -is [byte[]]) { $checksums = [System.Text.Encoding]::UTF8.GetString($checksums) }
  } catch { }
  $problem = $null
  if (-not $checksums) {
    $problem = "Could not fetch $checksumUrl to verify the download; nothing was installed.`n  Please check your connection and try again."
  } else {
    $pattern = ' \*?' + [regex]::Escape($asset) + '\s*$'
    $line = $checksums -split "`n" | ForEach-Object { $_.TrimEnd("`r") } | Where-Object { $_ -match $pattern } | Select-Object -First 1
    if (-not $line) {
      $problem = "$checksumUrl lists no checksum for $asset; nothing was installed."
    } else {
      $expectedHash = ($line -split '\s+')[0].ToLower()
      $actualHash = (Get-FileHash -Path "$target.new" -Algorithm SHA256).Hash.ToLower()
      if ($expectedHash -ne $actualHash) { $problem = "SHA256 mismatch; nothing was installed." }
    }
  }
  if ($problem) {
    Write-Host "  $problem" -ForegroundColor Red
    Remove-Item "$target.new" -Force -ErrorAction SilentlyContinue
    Undo-Directories
    return
  }

  # Keep a previous version until setup finishes. Its name must not match
  # "<bin>.exe.old-*": the program deletes those files when it starts.
  # A backup is deleted after setup, which fails while an AI client still
  # runs it; drop those left by earlier runs now.
  Get-ChildItem -LiteralPath $installDir -Filter "$Bin.exe.bak-*" -Force -ErrorAction SilentlyContinue |
    Remove-Item -Force -ErrorAction SilentlyContinue
  $backup = $null
  if (Test-Path $target) {
    $backup = "$target.bak-$([System.Guid]::NewGuid().ToString('N').Substring(0, 8))"
    try {
      Move-Item $target $backup -Force
    } catch {
      Write-Host "  Could not replace $target. Close any running $Bin and retry." -ForegroundColor Red
      Remove-Item "$target.new" -Force -ErrorAction SilentlyContinue
      return
    }
  }
  function Restore-Backup {
    Remove-Item $target -Force -ErrorAction SilentlyContinue
    if ($backup -and (Test-Path $backup)) { Move-Item $backup $target -Force }
  }
  try {
    Move-Item "$target.new" $target -Force
  } catch {
    Write-Host "  Could not install $target." -ForegroundColor Red
    Remove-Item "$target.new" -Force -ErrorAction SilentlyContinue
    Restore-Backup
    Undo-Directories
    return
  }

  # Put the install directory on the user PATH, remembering whether we did.
  $addedPath = $false
  $userPath = Get-UserPath
  if (-not (@($userPath.Value -split ";") | Where-Object { Test-SameDir $_ $installDir })) {
    $newValue = if ($userPath.Value) { "$installDir;$($userPath.Value)" } else { $installDir }
    Set-UserPath $newValue $userPath.Kind
    $env:Path = "$installDir;$env:Path"
    $addedPath = $true
  }
  function Undo-Path {
    if (-not $addedPath) { return }
    # Remove only our entry, keeping any change made to PATH meanwhile.
    $current = Get-UserPath
    $kept = @($current.Value -split ";" | Where-Object { -not (Test-SameDir $_ $installDir) })
    Set-UserPath ($kept -join ";") $current.Kind
    $env:Path = (@($env:Path -split ";" | Where-Object { -not (Test-SameDir $_ $installDir) })) -join ";"
  }

  # Run the setup wizard, in a terminal only: without one it cannot ask,
  # and `configure` would register every detected client unasked.
  $interactive = $false
  try { $interactive = -not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected } catch { }
  # The spellings Go's flag package reads as --yes; it is case-sensitive.
  $yesValues = foreach ($dash in "--", "-") {
    foreach ($suffix in "", "=1", "=t", "=T", "=true", "=TRUE", "=True") { "${dash}yes$suffix" }
  }
  $unattended = [bool]($ConfigureArgs | Where-Object { $_ -cin $yesValues })
  if (-not $interactive -and -not $unattended) {
    if ($backup) { Remove-Item $backup -Force -ErrorAction SilentlyContinue }
    Write-Host "  Installed $Bin to $target."
    Write-Host "  Not running in a terminal. Finish setup in one with: $Bin configure"
    return
  }
  try {
    & $target configure @ConfigureArgs
    $code = $LASTEXITCODE
  } catch {
    Write-Host "  Could not run $target`: $_" -ForegroundColor Red
    $code = $exitCancelled
  }

  if ($code -eq $exitCancelled) {
    # Put everything back as it was.
    Restore-Backup
    Undo-Path
    Undo-Directories
    # The wizard already said "Setup cancelled"; say what was restored.
    if ($backup) {
      Write-Host "  The previously installed $Bin was kept."
    } else {
      Write-Host "  $Bin was not installed."
    }
    return
  }

  if ($backup) { Remove-Item $backup -Force -ErrorAction SilentlyContinue }
  if ($addedPath) { Write-Host "  Added $installDir to your PATH." }
  if ($code -ne 0) {
    Write-Host "  Setup did not finish (exit code $code). $Bin is installed at $target." -ForegroundColor Yellow
    Write-Host "  Run ``$Bin configure`` to finish, or ``$Bin uninstall --all`` to remove it." -ForegroundColor Yellow
    return
  }
  Write-Host "  Installed $Bin to $target"
} @args
