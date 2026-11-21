$ErrorActionPreference = 'Stop';
$utf8 = [Text.UTF8Encoding]::new($false, $true);
$oldDir = 'D:\dev\ChatGPT\mcp-r17-native-qualification-20261119\d55a6763eb064445a46ef592d52a9e6f'; $newDir = 'D:\dev\ChatGPT\mcp-r17-native-qualification-20261119\68c2db3e983a41f9bded8e068e88bab8';
function Read-JSON([string]$path) { return ([IO.File]::ReadAllText($path, $utf8) | ConvertFrom-Json) };
function Principal-SID([string]$value) { try { if ($value.StartsWith('S-1-')) { return ([Security.Principal.SecurityIdentifier]::new($value)).Value }; return ([Security.Principal.NTAccount]::new($value)).Translate([Security.Principal.SecurityIdentifier]).Value } catch { return $null } };
$oldIntent = Read-JSON ($oldDir + '\intent.json'); $newIntent = Read-JSON ($newDir + '\intent.json');
$names = @($oldIntent.names) + @($newIntent.names);
if ($names.Count -ne 4 -or @($names | Select-Object -Unique).Count -ne 4) { throw 'Expected four distinct fixture names.' };
$service = New-Object -ComObject 'Schedule.Service'; $service.Connect(); $folder = $service.GetFolder('\');
$currentTasks = @($folder.GetTasks(1));
$counts = @(foreach ($name in $names) { [pscustomobject]@{ name = $name; count = @($currentTasks | Where-Object { $_.Name -ceq $name }).Count } });
$principalChecks = @(foreach ($label in @('status-visible','export-visible','status-hidden','export-hidden','list-visible-only')) { $response = Read-JSON ($newDir + '\' + $label + '.json'); $row = if ($label -ceq 'list-visible-only') { @($response.tasks)[0] } else { $response.task }; [pscustomobject]@{ check = $label; principal_matches_intent = ((Principal-SID ([string]$row.owner)) -ceq [string]$newIntent.principal_sid) } });
$observation = [ordered]@{ observed_utc = [DateTime]::UtcNow.ToString('o'); scheduler_query = 'GetTasks(1)'; mutates_scheduler = $false; names = $counts; all_four_absent = (@($counts | Where-Object { $_.count -ne 0 }).Count -eq 0); current_principal_matches_old_intent = ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value -ceq [string]$oldIntent.principal_sid); current_principal_matches_new_intent = ([Security.Principal.WindowsIdentity]::GetCurrent().User.Value -ceq [string]$newIntent.principal_sid); response_principals = $principalChecks };
$bytes = $utf8.GetBytes(($observation | ConvertTo-Json -Depth 6));
$stream = [IO.File]::Open(($newDir + '\native-v2-observation.json'), [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None);
try { $stream.Write($bytes, 0, $bytes.Length); $stream.Flush($true) } finally { $stream.Dispose() };
$observation | ConvertTo-Json -Depth 6;
if (!$observation.all_four_absent -or !$observation.current_principal_matches_old_intent -or !$observation.current_principal_matches_new_intent -or @($principalChecks | Where-Object { !$_.principal_matches_intent }).Count -ne 0) { exit 1 };

