$ErrorActionPreference='Stop'
$taskRoot='D:\dev\ChatGPT\mcphub-ffi-identity-20261115'
$logRoot=Join-Path $taskRoot 'evidence\repository-gates'
@($logRoot,(Join-Path $taskRoot 'tmp\repository'),(Join-Path $taskRoot 'appdata'),(Join-Path $taskRoot 'localappdata')) | ForEach-Object {[IO.Directory]::CreateDirectory($_)|Out-Null}
$env:TEMP=Join-Path $taskRoot 'tmp\repository';$env:TMP=$env:TEMP;$env:GOTMPDIR=$env:TEMP
$env:APPDATA=Join-Path $taskRoot 'appdata';$env:LOCALAPPDATA=Join-Path $taskRoot 'localappdata'
$env:GOCACHE=Join-Path $taskRoot 'go-cache';$env:GOMODCACHE=Join-Path $taskRoot 'go-mod-cache';$env:GOPATH=Join-Path $taskRoot 'go-path'
$env:GOTOOLCHAIN='local';$env:GOPROXY='file:///C:/Users/<USER-REDACTED>/go/pkg/mod/cache/download';$env:GOSUMDB='off';$env:GOTELEMETRY='off';$env:CGO_ENABLED='1';$env:CC='c:\msys64\ucrt64\bin\gcc.exe'
$env:MCPHUB_GUI_SPAWN_TESTS=''
Set-Location (Join-Path $taskRoot 'source')
$records=New-Object Collections.Generic.List[object]
foreach($step in @(
 @{name='build';arguments=@('build','./...')},
 @{name='vet';arguments=@('vet','./...')},
 @{name='test';arguments=@('test','-json','-count=1','-timeout','5m','./...')},
 @{name='state-tag';arguments=@('test','-json','-tags=test_state_path_env','-count=1','-timeout','5m','./internal/api/','./internal/cli/')}
)){
 $started=[DateTime]::UtcNow.ToString('o');$arguments=$step.arguments
 $old=$ErrorActionPreference;$ErrorActionPreference='Continue'
 & go @arguments 2>&1 | Out-File -Encoding utf8 (Join-Path $logRoot ($step.name+'.log'))
 $code=$LASTEXITCODE;$ErrorActionPreference=$old
 $records.Add([ordered]@{name=$step.name;program='go';arguments=$arguments;exitCode=$code;startedUtc=$started;finishedUtc=[DateTime]::UtcNow.ToString('o');log=$step.name+'.log'})
 $json=ConvertTo-Json -InputObject @($records.ToArray()) -Depth 5
 [IO.File]::WriteAllText((Join-Path $logRoot 'steps.json'),($json -replace '\r\n',[string][char]10)+[char]10,(New-Object Text.UTF8Encoding($false)))
 Write-Output ($step.name+' exit='+$code)
}
Write-Output 'REPOSITORY_GATES_RECORDED'
