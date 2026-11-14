#Requires -Version 5.1
param([Parameter(Mandatory=$true)][string]$OutputPath)
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$programFilesX86 = ${env:ProgramFiles(x86)}
if ([string]::IsNullOrWhiteSpace($programFilesX86)) {
    throw 'CST test frontend requires the Windows ProgramFiles(x86) toolchain location.'
}
$vswhere = Join-Path $programFilesX86 'Microsoft Visual Studio\Installer\vswhere.exe'
if (-not (Test-Path -LiteralPath $vswhere -PathType Leaf)) {
    throw "CST test frontend requires vswhere.exe: $vswhere"
}
$vsOutput = @(& $vswhere -latest -products '*' -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath)
if ($LASTEXITCODE -ne 0) {
    throw "CST test frontend Visual C++ discovery failed: vswhere exit code $LASTEXITCODE"
}
$vsPaths = @($vsOutput | ForEach-Object { ([string]$_).Trim() } | Where-Object { $_ -ne '' })
if ($vsPaths.Count -ne 1) {
    throw 'CST test frontend requires one installed Visual C++ x64 toolchain; vswhere returned no unique installation with Microsoft.VisualStudio.Component.VC.Tools.x86.x64.'
}
$vs = $vsPaths[0]
if (-not (Test-Path -LiteralPath $vs -PathType Container)) {
    throw "CST test frontend Visual C++ installation directory is missing: $vs"
}
$vcVersionPath = Join-Path $vs 'VC\Auxiliary\Build\Microsoft.VCToolsVersion.default.txt'
if (-not (Test-Path -LiteralPath $vcVersionPath -PathType Leaf)) {
    throw "CST test frontend Visual C++ toolchain version file is missing: $vcVersionPath"
}
$vcVersionRaw = Get-Content -LiteralPath $vcVersionPath -Raw
if ([string]::IsNullOrWhiteSpace($vcVersionRaw)) {
    throw "CST test frontend Visual C++ toolchain version is empty: $vcVersionPath"
}
$vcVersion = $vcVersionRaw.Trim()
$vc = Join-Path $vs "VC\Tools\MSVC\$vcVersion"
if (-not (Test-Path -LiteralPath $vc -PathType Container)) {
    throw "CST test frontend selected Visual C++ toolchain directory is missing: $vc"
}
$sdkRoot = Join-Path $programFilesX86 'Windows Kits\10'
$sdkIncludes = Join-Path $sdkRoot 'Include'
if (-not (Test-Path -LiteralPath $sdkIncludes -PathType Container)) {
    throw "CST test frontend requires Windows 10 SDK headers: $sdkIncludes"
}
$sdk = Get-ChildItem -LiteralPath $sdkIncludes -Directory |
    Where-Object { Test-Path -LiteralPath (Join-Path $_.FullName 'um\Windows.h') -PathType Leaf } |
    Sort-Object { [version]$_.Name } -Descending | Select-Object -First 1
if ($null -eq $sdk) {
    throw "CST test frontend requires an installed Windows SDK version containing um\Windows.h: $sdkIncludes"
}
$sdkVersion = $sdk.Name
$cl = Join-Path $vc 'bin\Hostx64\x64\cl.exe'; $link = Join-Path $vc 'bin\Hostx64\x64\link.exe'
$includes = @(Join-Path $vc 'include'; Join-Path $sdkRoot "Include\$sdkVersion\ucrt"; Join-Path $sdkRoot "Include\$sdkVersion\shared"; Join-Path $sdkRoot "Include\$sdkVersion\um")
$kernel32 = Join-Path $sdkRoot "Lib\$sdkVersion\um\x64\kernel32.lib"
$source = Join-Path $root 'mcphub_cst_runtime.c'
foreach ($path in @($cl, $link, $kernel32, $source)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "CST test frontend required compiler, linker, SDK library, or source file is missing: $path"
    }
}
foreach ($path in $includes) {
    if (-not (Test-Path -LiteralPath $path -PathType Container)) {
        throw "CST test frontend required include directory is missing: $path"
    }
}
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    throw 'CST test frontend requires a nonempty output path.'
}
$OutputPath = [IO.Path]::GetFullPath($OutputPath)
$outputDirectory = [IO.Path]::GetDirectoryName($OutputPath)
if (-not (Test-Path -LiteralPath $outputDirectory -PathType Container)) {
    throw "CST test frontend requires an existing private output directory: $outputDirectory"
}
$obj = [IO.Path]::ChangeExtension($OutputPath, '.obj')
$flags = @('/nologo','/c','/TC','/O2','/Oi-','/GS-','/GR-','/Zl','/W4','/WX','/guard:cf','/DUNICODE','/D_UNICODE','/DCST_TEST_FRONTEND_E2E')
foreach($path in $includes){$flags += "/I$path"}
& $cl @flags "/Fo$obj" $source; if($LASTEXITCODE){throw "cl failed: $LASTEXITCODE"}
& $link /NOLOGO /NODEFAULTLIB /ENTRY:mcphub_cst_entry /SUBSYSTEM:WINDOWS /MACHINE:X64 /MANIFEST:NO /INCREMENTAL:NO /OPT:REF /OPT:ICF /DYNAMICBASE /HIGHENTROPYVA /NXCOMPAT /CETCOMPAT /GUARD:CF /DEPENDENTLOADFLAG:0x800 /BREPRO /RELEASE "/OUT:$OutputPath" $obj $kernel32; if($LASTEXITCODE){throw "link failed: $LASTEXITCODE"}
$trashDirectory = Join-Path $outputDirectory ('trash-cst-test-frontend-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $trashDirectory | Out-Null
Move-Item -LiteralPath $obj -Destination (Join-Path $trashDirectory ([IO.Path]::GetFileName($obj)))
