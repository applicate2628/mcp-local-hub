param(
    [Parameter(Mandatory=$true)][string]$TaskRoot,
    [Parameter(Mandatory=$true)][string]$CargoHome,
    [Parameter(Mandatory=$true)][string]$CCompiler,
    [string]$Run = 'consumer',
    [string]$FfiRef = 'a7a4c2d19c5a572ae771ab9b9a1511ae2cf9a718'
)
$ErrorActionPreference = 'Stop'
if ($Run -notmatch '^[a-zA-Z0-9_-]+$' -or $FfiRef -notmatch '^[a-f0-9]{40}$') { throw 'Invalid run/ref' }
if (-not (Test-Path -LiteralPath $CargoHome -PathType Container) -or -not (Test-Path -LiteralPath $CCompiler -PathType Leaf)) { throw 'Existing offline toolchain/cache required' }
$crate = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$repo = (Get-Item $crate).Parent.Parent.FullName
$task = [IO.Path]::GetFullPath($TaskRoot)
$workspace = Join-Path $task ('consumer-' + $Run)
$results = Join-Path $task ('evidence\' + $Run)
if ((Test-Path $workspace) -or (Test-Path $results)) { throw 'Do not overwrite an earlier consumer run' }
New-Item -ItemType Directory -Path $workspace,$results | Out-Null
$native = Join-Path $workspace 'experiments\rust-process-owner'
New-Item -ItemType Directory -Path $native | Out-Null
foreach ($part in @('src','tests','Cargo.toml','Cargo.lock')) { Copy-Item -LiteralPath (Join-Path $crate $part) -Destination $native -Recurse }
$archive = Join-Path $workspace 'ffi.tar'
& git -C $repo archive --format=tar --output=$archive $FfiRef -- experiments/rust-process-owner/ffi/src experiments/rust-process-owner/ffi/go experiments/rust-process-owner/ffi/include experiments/rust-process-owner/ffi/Cargo.toml experiments/rust-process-owner/ffi/Cargo.lock
if ($LASTEXITCODE -ne 0) { throw 'Pinned FFI archive unavailable' }
& tar -xf $archive -C $workspace
if ($LASTEXITCODE -ne 0) { throw 'Pinned FFI extraction failed' }
$ffi = Join-Path $native 'ffi'
$env:TEMP = Join-Path $task 'tmp'; $env:TMP=$env:TEMP; $env:GOTMPDIR=$env:TEMP
$env:CARGO_HOME=$CargoHome; $env:CARGO_TARGET_DIR=Join-Path $task 'target-consumer'
$env:CARGO_NET_OFFLINE='true'; $env:RUSTUP_OFFLINE='1'
$env:GOTOOLCHAIN='local'; $env:GOPROXY='off'; $env:GOSUMDB='off'; $env:GOTELEMETRY='off'
$env:GOCACHE=Join-Path $task 'go-build-cache'; $env:GOMODCACHE=Join-Path $task 'go-mod-cache'; $env:GOPATH=Join-Path $task 'go-path'
$env:CGO_ENABLED='1'; $env:CC=$CCompiler; $env:GOMAXPROCS='4'; $env:GOEXPERIMENT=''; $env:MCPHUB_FFI_FAULT=''
New-Item -ItemType Directory -Path $env:TEMP,$env:GOCACHE,$env:GOMODCACHE,$env:GOPATH -Force | Out-Null
$utf8=New-Object Text.UTF8Encoding($false)
$steps=New-Object 'Collections.Generic.List[object]'
function Write-Text([string]$Path,[string]$Text) {
    $text=$Text.Replace($task,'<task-root>').Replace($CargoHome,'<cargo-cache>').Replace($CCompiler,'<existing-c-compiler>')
    $text=[regex]::Replace($text,'(?i)C:\\Users\\[^\\\s"''<>]+','<user-profile>')
    [IO.File]::WriteAllText($Path,(($text -replace "`r`n","`n") -replace '[ \t]+(?=\n|$)','').TrimEnd()+"`n",$utf8)
}
function Digest([string]$Path) {
    $sha=[Security.Cryptography.SHA256]::Create()
    try { return ([BitConverter]::ToString($sha.ComputeHash([IO.File]::ReadAllBytes($Path)))).Replace('-','').ToLowerInvariant() }
    finally { $sha.Dispose() }
}
function Check([string]$Name,[string]$Root,[string]$Tool,[string[]]$Arguments) {
    Push-Location $Root
    try {
        $ErrorActionPreference='Continue'
        $output=& $Tool @Arguments 2>&1; $code=$LASTEXITCODE
        $ErrorActionPreference='Stop'
        $text=($output | ForEach-Object { $_.ToString() }) -join "`n"
        $steps.Add([pscustomobject]@{name=$Name;command=@($Tool)+$Arguments;actual=$code;utc=[DateTime]::UtcNow.ToString('o')})
        $file=Join-Path $results 'run.log'
        $prior=if(Test-Path $file){[IO.File]::ReadAllText($file)}else{''}
        Write-Text $file ($prior+"## $Name`n"+$text)
        Write-Text (Join-Path $results 'steps.json') (ConvertTo-Json -InputObject @($steps.ToArray()) -Depth 5)
        Write-Output "$Name exit=$code"
        if ($code -ne 0) { throw ('Consumer check failed: '+$Name) }
        if ($Name -like 'go-*-race') {
            $top=[regex]::Matches($text,'(?m)^--- PASS:').Count
            $nested=[regex]::Matches($text,'(?m)^[ \t]+--- PASS:').Count
            $skips=[regex]::Matches($text,'(?m)^[ \t]*--- SKIP:').Count
            if ($top -eq 0 -or $skips -ne 0) { throw 'No normal tests or unexpected skips' }
            Write-Text (Join-Path $results ($Name+'-counts.json')) (ConvertTo-Json @{topLevel=$top;nested=$nested;skipped=$skips})
        }
    } finally { $ErrorActionPreference='Stop'; Pop-Location }
}
$hashes=@(Get-ChildItem -LiteralPath $native -File -Recurse | Sort-Object FullName | ForEach-Object { [pscustomobject]@{path=$_.FullName.Substring($native.Length+1).Replace('\','/');sha256=(Digest $_.FullName)} })
Write-Text (Join-Path $results 'source-hashes.json') (ConvertTo-Json -InputObject $hashes -Depth 4)
Write-Text (Join-Path $results 'inputs.json') (ConvertTo-Json @{nativeGitHead=((& git -C $repo rev-parse HEAD)-join'');ffiCommit=$FfiRef;archiveSha256=(Digest $archive);runnerSha256=(Digest $PSCommandPath);go=((& go version)-join'');rustc=((& rustc --version)-join'')})
Check 'child' $native 'cargo' @('build','--offline','--locked','--bin','owner_child')
Check 'abi' $ffi 'cargo' @('test','--offline','--locked')
Check 'debug-dll' $ffi 'cargo' @('build','--offline','--locked')
Check 'release-dll' $ffi 'cargo' @('build','--release','--offline','--locked')
$env:MCPHUB_RX02_FFI_CHILD=Join-Path $env:CARGO_TARGET_DIR 'debug\owner_child.exe'
foreach ($profile in @('debug','release')) {
    $env:MCPHUB_RX02_FFI_DLL=Join-Path $env:CARGO_TARGET_DIR ($profile+'\mcphub_owner_ffi_spike.dll')
    Check ('go-'+$profile+'-race') (Join-Path $ffi 'go') 'go' @('test','-v','-race','.','-count=1','-timeout=90s')
}
$env:GOEXPERIMENT='cgocheck2'
try { Check 'go-cgocheck2' (Join-Path $ffi 'go') 'go' @('test','.','-count=1','-timeout=90s') }
finally { $env:GOEXPERIMENT='' }
Check 'go-vet' (Join-Path $ffi 'go') 'go' @('vet','.')
Write-Text (Join-Path $results 'COMPLETED') 'Existing frozen FFI consumer passed with the new native owner; this is not a new foreign identity API or product admission.'
