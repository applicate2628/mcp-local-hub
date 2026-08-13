#Requires -Version 5.1
param([Parameter(Mandatory=$true)][string]$OutputPath)
$ErrorActionPreference = 'Stop'
$root = $PSScriptRoot
$vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
$vs = (& $vswhere -latest -products * -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 -property installationPath).Trim()
$vcVersion = (Get-Content -LiteralPath (Join-Path $vs 'VC\Auxiliary\Build\Microsoft.VCToolsVersion.default.txt')).Trim()
$vc = Join-Path $vs "VC\Tools\MSVC\$vcVersion"
$sdkRoot = Join-Path ${env:ProgramFiles(x86)} 'Windows Kits\10'
$sdkVersion = (Get-ChildItem -LiteralPath (Join-Path $sdkRoot 'Include') -Directory | Where-Object { Test-Path -LiteralPath (Join-Path $_.FullName 'um\Windows.h') } | Sort-Object { [version]$_.Name } -Descending | Select-Object -First 1).Name
$cl = Join-Path $vc 'bin\Hostx64\x64\cl.exe'; $link = Join-Path $vc 'bin\Hostx64\x64\link.exe'
$includes = @(Join-Path $vc 'include'; Join-Path $sdkRoot "Include\$sdkVersion\ucrt"; Join-Path $sdkRoot "Include\$sdkVersion\shared"; Join-Path $sdkRoot "Include\$sdkVersion\um")
$obj = [IO.Path]::ChangeExtension($OutputPath, '.obj')
$flags = @('/nologo','/c','/TC','/O2','/Oi-','/GS-','/GR-','/Zl','/W4','/WX','/guard:cf','/DUNICODE','/D_UNICODE','/DCST_TEST_FRONTEND_E2E')
foreach($path in $includes){$flags += "/I$path"}
& $cl @flags "/Fo$obj" (Join-Path $root 'mcphub_cst_runtime.c'); if($LASTEXITCODE){throw "cl failed: $LASTEXITCODE"}
& $link /NOLOGO /NODEFAULTLIB /ENTRY:mcphub_cst_entry /SUBSYSTEM:WINDOWS /MACHINE:X64 /MANIFEST:NO /INCREMENTAL:NO /OPT:REF /OPT:ICF /DYNAMICBASE /HIGHENTROPYVA /NXCOMPAT /CETCOMPAT /GUARD:CF /DEPENDENTLOADFLAG:0x800 /BREPRO /RELEASE "/OUT:$OutputPath" $obj (Join-Path $sdkRoot "Lib\$sdkVersion\um\x64\kernel32.lib"); if($LASTEXITCODE){throw "link failed: $LASTEXITCODE"}
Remove-Item -LiteralPath $obj -Force
