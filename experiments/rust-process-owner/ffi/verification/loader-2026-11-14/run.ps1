# Owned native loader qualification. Does not install, push or change ordinary DLLs.
[CmdletBinding()]
param(
    [Parameter(Mandatory=$true)][string]$LabRoot,
    [Parameter(Mandatory=$true)][string]$CCompiler,
    [Parameter(Mandatory=$true)][ValidatePattern('^[A-Za-z0-9][A-Za-z0-9._-]*$')][string]$RunName,
    [ValidateSet('none','leak-module','accept-version','default-search')][string]$Mutation='none'
)
$ErrorActionPreference='Stop'
$utf8=[Text.UTF8Encoding]::new($false)
$LabRoot=[IO.Path]::GetFullPath($LabRoot)
if(-not $LabRoot.StartsWith('D:\dev\ChatGPT\',[StringComparison]::OrdinalIgnoreCase)){throw 'Approved private lab required'}
$ffi=(Get-Item $PSScriptRoot).Parent.Parent.FullName
$repo=(Get-Item $ffi).Parent.Parent.Parent.FullName
$report=Join-Path (Split-Path $PSScriptRoot -Parent) ('runs\loader-'+$RunName)
$work=Join-Path $LabRoot ('qualification-loader-'+$RunName)
if((Test-Path $report)-or(Test-Path $work)){throw 'New run name required; prior evidence must not be overwritten'}
if(-not(Test-Path $CCompiler -PathType Leaf)){throw 'Existing C compiler required; no download'}
New-Item -ItemType Directory -Path $report,$work|Out-Null
$env:TEMP=Join-Path $LabRoot 'tmp';if(-not(Test-Path $env:TEMP)){New-Item -ItemType Directory $env:TEMP|Out-Null}
$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP;$env:GOCACHE='D:\dev\ChatGPT\go-build-cache'
$env:GOTOOLCHAIN='local';$env:GOPROXY='off';$env:GOSUMDB='off';$env:GOTELEMETRY='off'
$env:GOMAXPROCS='4';$env:CGO_ENABLED='1';$env:CC=$CCompiler;$env:GOEXPERIMENT='';$env:GIT_OPTIONAL_LOCKS='0'
function Hash([string]$p){$h=[Security.Cryptography.SHA256]::Create();try{[BitConverter]::ToString($h.ComputeHash([IO.File]::ReadAllBytes($p))).Replace('-','').ToLowerInvariant()}finally{$h.Dispose()}}
function Clean([string]$t){$t=$t.Replace($LabRoot,'<LAB>').Replace($repo,'<REPOSITORY>').Replace("`r`n","`n");$t=[regex]::Replace($t,'(?i)C:\\Users\\[^\\\s"''<>]+','<USERPROFILE>');return [regex]::Replace($t,'(?m)[ \t]+$','').TrimEnd()+"`n"}
function Text([string]$p,[string]$t){[IO.File]::WriteAllText($p,(Clean $t),$utf8)}
$steps=[Collections.Generic.List[object]]::new()
function Check([string]$name,[string]$tool,[string[]]$arguments,[int]$expected=0,[string]$marker=''){
    $ErrorActionPreference='Continue';$lines=& $tool @arguments 2>&1;$exit=$LASTEXITCODE;$ErrorActionPreference='Stop'
    $body=(@($lines|ForEach-Object{$_.ToString()})-join "`n")
    $matched=($exit-eq $expected)-and(($marker-eq '')-or $body.Contains($marker))
    $header=@("Step: $name",('Command: '+$tool+' '+($arguments-join ' ')),('UTC: '+[DateTime]::UtcNow.ToString('o')),"Expected exit: $expected","Actual exit: $exit","Matched: $matched",'')-join "`n"
    [IO.File]::AppendAllText((Join-Path $report 'run.log'),(Clean ($header+$body+"`n")),$utf8)
    $steps.Add([ordered]@{name=$name;command=($tool+' '+($arguments-join ' '));expected=$expected;actual=$exit;required_marker=$marker;matched=$matched})
    Text (Join-Path $report 'steps.json') ($steps.ToArray()|ConvertTo-Json -Depth 4)
    if($name-eq 'loader-counts' -and $exit-eq 0){
        $events=@($lines|ForEach-Object{try{ConvertFrom-Json $_.ToString()}catch{}})
        $counts=[ordered]@{top_level_pass=@($events|Where-Object{$_.Action-eq 'pass' -and $_.Test -and $_.Test-notlike '*/*'}).Count;nested_pass=@($events|Where-Object{$_.Action-eq 'pass' -and $_.Test-like '*/*'}).Count;skipped=@($events|Where-Object{$_.Action-eq 'skip'}).Count}
        Text (Join-Path $report 'counts.json') ($counts|ConvertTo-Json)
    }
    Write-Output "$name exit=$exit expected=$expected matched=$matched"
    if(-not $matched){$lines|Select-Object -Last 25;throw ('Unexpected result: '+$name)}
}
$sourceFiles=@(Get-ChildItem (Join-Path $ffi 'go'),(Join-Path $ffi 'include'),$PSScriptRoot -File -Recurse|Where-Object{$_.Extension-in @('.go','.h','.c','.ps1')})+@(Get-Item (Join-Path $ffi 'go\go.mod'))
$source=@($sourceFiles|ForEach-Object{[ordered]@{path=$_.FullName.Substring($repo.Length+1).Replace('\','/');sha256=(Hash $_.FullName)}})
Text (Join-Path $report 'source-hashes.json') ($source|ConvertTo-Json -Depth 4)
$fixtures=Join-Path $work 'fixtures';New-Item -ItemType Directory -Path $fixtures|Out-Null
$include=Join-Path $ffi 'include'
$common=@('-std=c11','-Wall','-Wextra','-Werror','-O2','-shared','-nostdlib','-Wl,--entry,DllMain','-I',$include)
$normalDLLs=@('debug','release'|ForEach-Object{[ordered]@{path=(Join-Path $LabRoot ("target\$_\mcphub_owner_ffi_spike.dll"));sha256=(Hash (Join-Path $LabRoot ("target\$_\mcphub_owner_ffi_spike.dll")))}})
$goRoot=Join-Path $ffi 'go'
try{
    Check 'exit-control' $env:ComSpec @('/d','/c','exit 7') 7
    Check 'source-head' 'git' @('-C',$repo,'rev-parse','HEAD')
    Check 'go-version' 'go' @('version')
    Check 'compiler-version' $CCompiler @('--version')
    Check 'build-valid' $CCompiler ($common+@((Join-Path $PSScriptRoot 'fixture.c'),'-o',(Join-Path $fixtures 'valid.dll')))
    foreach($i in 0..8){Check ("build-missing-$i") $CCompiler ($common+@("-DOMIT_EXPORT=$i",(Join-Path $PSScriptRoot 'fixture.c'),'-o',(Join-Path $fixtures "missing-$i.dll")))}
    foreach($case in @(@{name='0';value='0u'},@{name='2';value='2u'},@{name='max';value='4294967295u'})){
        Check ('build-version-'+$case.name) $CCompiler ($common+@(('-DFIXTURE_VERSION='+$case.value),(Join-Path $PSScriptRoot 'fixture.c'),'-o',(Join-Path $fixtures ('version-'+$case.name+'.dll'))))
    }
    Check 'build-init-refused' $CCompiler ($common+@('-DREFUSE_INITIALIZATION',(Join-Path $PSScriptRoot 'fixture.c'),'-o',(Join-Path $fixtures 'init-refused.dll')))
    [IO.File]::WriteAllText((Join-Path $fixtures 'not-a-library.dll'),'This is an owned non-executable test file.',$utf8)
    $unicode='unicode-'+[char]0x03bb+'-'+[char]::ConvertFromUtf32(0x1F980)
    foreach($dir in @('dependency-only','with-dependency','missing-dependency',$unicode)){New-Item -ItemType Directory -Path (Join-Path $fixtures $dir)|Out-Null}
    $dep=Join-Path $fixtures 'dependency-only\mcphub_owned_loader_dependency.dll'
    $import=Join-Path $work 'lib-owned-dependency.a'
    Check 'build-dependency' $CCompiler ($common+@((Join-Path $PSScriptRoot 'dependency.c'),('-Wl,--out-implib,'+$import),'-o',$dep))
    $needs=Join-Path $fixtures 'missing-dependency\needs-dependency.dll'
    Check 'build-needs-dependency' $CCompiler ($common+@('-DREQUIRE_DEPENDENCY',(Join-Path $PSScriptRoot 'fixture.c'),$import,'-o',$needs))
    Copy-Item $needs (Join-Path $fixtures 'with-dependency\needs-dependency.dll')
    Copy-Item $dep (Join-Path $fixtures 'with-dependency\mcphub_owned_loader_dependency.dll')
    Copy-Item (Join-Path $fixtures 'valid.dll') (Join-Path (Join-Path $fixtures $unicode) 'valid.dll')
    $artifact=@(Get-ChildItem $fixtures -File -Recurse|ForEach-Object{[ordered]@{path=$_.FullName.Substring($fixtures.Length+1).Replace('\','/');sha256=(Hash $_.FullName)}})
    Text (Join-Path $report 'fixture-hashes.json') ($artifact|ConvertTo-Json -Depth 4)
    if($Mutation-ne 'none'){
        Copy-Item (Join-Path $ffi 'go') $work -Recurse
        Copy-Item (Join-Path $ffi 'include') $work -Recurse
        $goRoot=Join-Path $work 'go';$bridge=Join-Path $goRoot 'bridge_windows.go';$code=[IO.File]::ReadAllText($bridge)
        switch($Mutation){
            'leak-module' {$before='FreeLibrary(b->module); free(b); return NULL;';$after='free(b); return NULL; // deliberate leaked module reference';$pattern='^TestLoaderRejectsEveryMissingExport/stdio$';$marker='unexpected live module reference'}
            'accept-version' {$before='if (version()!=1)';$after='if (version()!=version())';$pattern='^TestLoaderRejectsVersionAndDoesNotRetainModule$';$marker='incompatible fixture accepted'}
            'default-search' {$before='LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR|LOAD_LIBRARY_SEARCH_SYSTEM32);';$after='0); // deliberate default dependency lookup';$pattern='^TestLoaderSearchDoesNotFallBackToWorkingDirectoryOrPath$';$marker='incompatible fixture accepted'}
        }
        if(($code.Split([string[]]@($before),[StringSplitOptions]::None).Length-1)-ne 1){throw 'Mutation anchor changed; do not guess'}
        [IO.File]::WriteAllText($bridge,$code.Replace($before,$after),$utf8)
        Text (Join-Path $report 'mutation.json') ([ordered]@{name=$Mutation;before=$before;after=$after;mutated_sha256=(Hash $bridge)}|ConvertTo-Json)
    }
    $env:MCPHUB_FFI_LOADER_FIXTURES=$fixtures
    Push-Location $goRoot
    try{
        if($Mutation-eq 'none'){
            Check 'loader-race' 'go' @('test','-tags','ffi_loader_faults','.','-run','^TestLoader','-count=3','-race','-timeout=90s','-v')
            Check 'loader-vet' 'go' @('vet','-tags','ffi_loader_faults','.')
            $env:GOEXPERIMENT='cgocheck2'
            Check 'loader-cgocheck2' 'go' @('test','-tags','ffi_loader_faults','.','-run','^TestLoader','-count=1','-timeout=90s')
            $env:GOEXPERIMENT=''
            Check 'loader-counts' 'go' @('test','-tags','ffi_loader_faults','.','-run','^TestLoader','-count=1','-json','-timeout=90s')
        }else{Check 'negative-loader' 'go' @('test','-tags','ffi_loader_faults','.','-run',$pattern,'-count=1','-timeout=30s','-v') 1 $marker}
    }finally{Pop-Location}
    foreach($entry in $source){if((Hash (Join-Path $repo $entry.path))-ne $entry.sha256){throw 'Original source changed during qualification'}}
    foreach($entry in $artifact){if((Hash (Join-Path $fixtures $entry.path))-ne $entry.sha256){throw 'Fixture bytes changed during qualification'}}
    foreach($entry in $normalDLLs){if((Hash $entry.path)-ne $entry.sha256){throw 'Ordinary DLL changed during qualification'}}
    Text (Join-Path $report 'normal-artifact-hashes.json') ($normalDLLs|ConvertTo-Json -Depth 4)
    Text (Join-Path $report 'COMPLETED.txt') "All expected loader outcomes matched. Mutation=$Mutation. Ordinary source and Rust DLLs unchanged; no product admission/trust, universal DLL unload, heap-leak or cross-platform claim."
}finally{
    $env:GOEXPERIMENT='';$env:MCPHUB_FFI_LOADER_FIXTURES=''
    $sums=@(Get-ChildItem $report -File|Where-Object{$_.Name-ne 'SHA256SUMS.txt'}|Sort-Object Name|ForEach-Object{(Hash $_.FullName)+'  '+$_.Name})
    Text (Join-Path $report 'SHA256SUMS.txt') ($sums-join "`n")
}
