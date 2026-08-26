#Requires -Version 5.1
[CmdletBinding()]
param([switch]$Clean, [switch]$Unsigned)
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$image = Join-Path $root 'mcphub-cst-runtime.exe'
$manifest = Join-Path $root 'cst-native-runtime-manifest-v1.json'
$buildRoot = Join-Path $root '.build'
if ($Clean -and (Test-Path -LiteralPath $buildRoot)) { Remove-Item -LiteralPath $buildRoot -Recurse -Force }
New-Item -ItemType Directory -Path $buildRoot -Force | Out-Null

$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
if (-not (Test-Path -LiteralPath $vswhere)) { throw 'vswhere.exe unavailable' }
$vs = (& $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath).Trim()
if (-not $vs) { throw 'pinned-capable Visual Studio installation unavailable' }
$vcVersion = (Get-Content -LiteralPath (Join-Path $vs 'VC\Auxiliary\Build\Microsoft.VCToolsVersion.default.txt')).Trim()
$vc = Join-Path $vs "VC\Tools\MSVC\$vcVersion"
$sdkRoot = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10'
$sdkVersion = (Get-ChildItem -LiteralPath (Join-Path $sdkRoot 'Include') -Directory | Where-Object { Test-Path -LiteralPath (Join-Path $_.FullName 'um\Windows.h') } | Sort-Object { [version]$_.Name } -Descending | Select-Object -First 1).Name
if (-not $sdkVersion) { throw 'Windows SDK unavailable' }
$cl = Join-Path $vc 'bin\Hostx64\x64\cl.exe'
$link = Join-Path $vc 'bin\Hostx64\x64\link.exe'
$dumpbin = Join-Path $vc 'bin\Hostx64\x64\dumpbin.exe'
$pyCommand = Get-Command py.exe -ErrorAction SilentlyContinue
if (-not $pyCommand) { throw 'Python launcher unavailable' }
$py = $pyCommand.Source
$kernel32 = Join-Path $sdkRoot "Lib\$sdkVersion\um\x64\kernel32.lib"
$include = @(Join-Path $vc 'include'; Join-Path $sdkRoot "Include\$sdkVersion\ucrt"; Join-Path $sdkRoot "Include\$sdkVersion\shared"; Join-Path $sdkRoot "Include\$sdkVersion\um")
foreach ($path in @($cl,$link,$dumpbin,$py,$kernel32)+$include) { if (-not (Test-Path -LiteralPath $path)) { throw "toolchain input unavailable: $path" } }

$compileFlags = @('/nologo','/c','/TC','/O2','/Oi-','/GS-','/GR-','/Zl','/W4','/WX','/guard:cf','/DUNICODE','/D_UNICODE')
foreach ($path in $include) { $compileFlags += "/I$path" }
$linkFlags = @('/NOLOGO','/NODEFAULTLIB','/ENTRY:mcphub_cst_entry','/SUBSYSTEM:WINDOWS','/MACHINE:X64','/MANIFEST:NO','/INCREMENTAL:NO','/OPT:REF','/OPT:ICF','/DYNAMICBASE','/HIGHENTROPYVA','/NXCOMPAT','/CETCOMPAT','/GUARD:CF','/DEPENDENTLOADFLAG:0x800','/BREPRO','/RELEASE')

function Invoke-Build([string]$name) {
    $dir = Join-Path $buildRoot $name
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    $obj = Join-Path $dir 'mcphub_cst_runtime.obj'
    $out = Join-Path $dir 'mcphub-cst-runtime.exe'
    & $cl @compileFlags "/Fo$obj" (Join-Path $root 'mcphub_cst_runtime.c') | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { throw "cl failed: $LASTEXITCODE" }
    & $link @linkFlags "/OUT:$out" $obj $kernel32 | ForEach-Object { Write-Host $_ }
    if ($LASTEXITCODE -ne 0) { throw "link failed: $LASTEXITCODE" }
    return $out
}

$one = Invoke-Build 'one'
$two = Invoke-Build 'two'
$hashOne = (Get-FileHash -Algorithm SHA256 -LiteralPath $one).Hash.ToLowerInvariant()
$hashTwo = (Get-FileHash -Algorithm SHA256 -LiteralPath $two).Hash.ToLowerInvariant()
if ($hashOne -ne $hashTwo) { throw "unsigned builds differ: $hashOne != $hashTwo" }
Copy-Item -LiteralPath $one -Destination $image -Force
$disassembly = (& $dumpbin /DISASM:NOBYTES $image | Where-Object { $_ -match '^\s*[0-9A-Fa-f]+:' }) -join "`n"
$sha = [Security.Cryptography.SHA256]::Create()
$disassemblyHash = ([BitConverter]::ToString($sha.ComputeHash([Text.Encoding]::UTF8.GetBytes($disassembly)))).Replace('-','').ToLowerInvariant()
$sourceHash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $root 'mcphub_cst_runtime.c')).Hash.ToLowerInvariant()
$buildScriptHash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $root 'build.ps1')).Hash.ToLowerInvariant()
$verifierHash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $root 'verify_cst_native_pe.py')).Hash.ToLowerInvariant()
$closureBuilderHash = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $root 'build_package_load_closure.py')).Hash.ToLowerInvariant()
$facts = & $py -3 (Join-Path $root 'verify_cst_native_pe.py') $image --json
if ($LASTEXITCODE -ne 0) { throw 'independent PE verification failed' }
$parsed = $facts | ConvertFrom-Json
$record = [ordered]@{
    schema='mcphub.cst.native-runtime-manifest.v1'; unsigned_builds_byte_identical=$true
    runtime_image_sha256=$hashOne; source_sha256=$sourceHash; pre_revocation_disassembly_sha256=$disassemblyHash
    input_hashes=[ordered]@{source_sha256=$sourceHash;build_script_sha256=$buildScriptHash;verifier_sha256=$verifierHash;closure_builder_sha256=$closureBuilderHash}
    entry_symbol='mcphub_cst_entry'; direct_imports=@('KERNEL32.dll'); pe=$parsed
    roles=[ordered]@{
        frontend=[ordered]@{inherited_handles=@('stdin','stdout','stderr','capability-read');revoked_before_package_code=$true}
        worker=[ordered]@{inherited_handles=@('stdin','stdout','stderr','source-root','workspace-root');revoked_before_package_code=$true}
    }
    toolchain=[ordered]@{
        discovery='vswhere-latest-complete-vc-x64 plus highest installed Windows SDK'
        msvc_tools_version=$vcVersion;windows_sdk_version=$sdkVersion
        compiler_version=(Get-Item -LiteralPath $cl).VersionInfo.FileVersion
        linker_version=(Get-Item -LiteralPath $link).VersionInfo.FileVersion
        compiler_flags=@('/nologo','/c','/TC','/O2','/Oi-','/GS-','/GR-','/Zl','/W4','/WX','/guard:cf','/DUNICODE','/D_UNICODE','/I<MSVC>/include','/I<WindowsSDK>/ucrt','/I<WindowsSDK>/shared','/I<WindowsSDK>/um')
        linker_flags=$linkFlags
    }
    package_load=[ordered]@{state='default-off';required_receipt='ProvisionedPackageIdentityV1';failure_id='native_loader_invalid'}
    package_load_closure=[ordered]@{
        schema='mcphub.cst.package-load-closure.v1';state='unprovisioned'
        resolver='independent PE normal/delay import traversal';ordered_package_rows=@();target_os_rows=@()
        admission='fail-closed until a complete exact package and target-OS closure is provisioned'
    }
    signed_structure=[ordered]@{status='target-unfulfilled';phase='X1/X2'}
}
$manifestJson = $record | ConvertTo-Json -Depth 12
$manifestUtf8 = [System.Text.UTF8Encoding]::new($false)
[System.IO.File]::WriteAllText($manifest, ($manifestJson -replace "`r`n", "`n") + "`n", $manifestUtf8)
Remove-Item -LiteralPath $buildRoot -Recurse -Force
Write-Host "W2 unsigned deterministic build PASS $hashOne"
