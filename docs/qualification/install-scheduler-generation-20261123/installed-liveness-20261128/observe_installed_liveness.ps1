param(
    [Parameter(Mandatory=$true)][string]$OutputPath
)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$result = [ordered]@{
    schema = 'installed-liveness-readonly-observation-v1'
    status = 'STARTED'
    task_name = '\mcp-local-hub-liveness'
    started_utc = [DateTime]::UtcNow.ToString('o')
    scheduler_mutation_attempted = $false
    product_launch_attempted = $false
    raw_xml_persisted = $false
    reads = @()
    com_release = @()
}
$com = [System.Collections.Generic.List[object]]::new()
function TextField($Node, [string]$XPath, $Namespaces) {
    $nodes = @($Node.SelectNodes($XPath, $Namespaces))
    return [ordered]@{
        count = $nodes.Count
        value = $(if ($nodes.Count -eq 1) { $nodes[0].InnerText } else { $null })
    }
}
function EnabledField($Node, [string]$XPath, $Namespaces) {
    $field = TextField $Node $XPath $Namespaces
    $effective = $null
    if ($field.count -eq 0) { $effective = $true }
    elseif ($field.count -eq 1) {
        switch ($field.value.Trim()) {
            { $_ -ceq '' -or $_ -ceq 'true' -or $_ -ceq '1' } { $effective = $true }
            { $_ -ceq 'false' -or $_ -ceq '0' } { $effective = $false }
        }
    }
    return [ordered]@{ count=$field.count; value=$field.value; effective=$effective }
}
function ObserveTask($Task) {
    $read = [ordered]@{ started_utc=[DateTime]::UtcNow.ToString('o') }
    $xmlText = [string]$Task.Xml
    $read.registered_enabled = [bool]$Task.Enabled
    $read.registered_state = [int]$Task.State
    $bytes = [Text.Encoding]::UTF8.GetBytes($xmlText)
    $hasher = [Security.Cryptography.SHA256]::Create()
    try { $read.xml_com_utf8_sha256 = ([BitConverter]::ToString($hasher.ComputeHash($bytes))).Replace('-','').ToLowerInvariant() }
    finally { $hasher.Dispose() }
    $read.xml_com_utf8_bytes = $bytes.Length
    $settings = [Xml.XmlReaderSettings]::new()
    $settings.DtdProcessing = [Xml.DtdProcessing]::Prohibit
    $settings.XmlResolver = $null
    $stringReader = [IO.StringReader]::new($xmlText)
    $xmlReader = $null
    try {
        $xmlReader = [Xml.XmlReader]::Create($stringReader, $settings)
        $document = [Xml.XmlDocument]::new()
        $document.XmlResolver = $null
        $document.PreserveWhitespace = $true
        $document.Load($xmlReader)
    } finally {
        if ($null -ne $xmlReader) { $xmlReader.Dispose() }
        $stringReader.Dispose()
    }
    $ns = [Xml.XmlNamespaceManager]::new($document.NameTable)
    $ns.AddNamespace('t','http://schemas.microsoft.com/windows/2004/02/mit/task')
    $read.task_count = @($document.SelectNodes('/t:Task',$ns)).Count
    $read.trigger_types = @($document.SelectNodes('/t:Task/t:Triggers/*',$ns) | ForEach-Object { $_.LocalName })
    $read.calendar = @($document.SelectNodes('/t:Task/t:Triggers/t:CalendarTrigger',$ns) | ForEach-Object {
        [ordered]@{
            start_boundary = (TextField $_ 't:StartBoundary' $ns)
            enabled = (EnabledField $_ 't:Enabled' $ns)
            interval = (TextField $_ 't:Repetition/t:Interval' $ns)
            stop_at_duration_end = (TextField $_ 't:Repetition/t:StopAtDurationEnd' $ns)
            days_interval = (TextField $_ 't:ScheduleByDay/t:DaysInterval' $ns)
        }
    })
    $read.logon = @($document.SelectNodes('/t:Task/t:Triggers/t:LogonTrigger',$ns) | ForEach-Object {
        [ordered]@{ enabled=(EnabledField $_ 't:Enabled' $ns) }
    })
    $read.settings_enabled = EnabledField $document '/t:Task/t:Settings/t:Enabled' $ns
    $read.execution_time_limit = TextField $document '/t:Task/t:Settings/t:ExecutionTimeLimit' $ns
    $read.multiple_instances_policy = TextField $document '/t:Task/t:Settings/t:MultipleInstancesPolicy' $ns
    $read.action_count = @($document.SelectNodes('/t:Task/t:Actions/t:Exec',$ns)).Count
    $read.completed_utc = [DateTime]::UtcNow.ToString('o')
    return $read
}
$exitCode = 0
try {
    $service = New-Object -ComObject 'Schedule.Service'
    $com.Add($service)
    $service.Connect()
    $folder = $service.GetFolder('\')
    $com.Add($folder)
    $task = $folder.GetTask('mcp-local-hub-liveness')
    $com.Add($task)
    $result.reads += ,(ObserveTask $task)
    $result.reads += ,(ObserveTask $task)
    $result.two_read_xml_equal = $result.reads[0].xml_com_utf8_sha256 -eq $result.reads[1].xml_com_utf8_sha256
    $result.two_read_enabled_equal = $result.reads[0].registered_enabled -eq $result.reads[1].registered_enabled
    $result.status = 'OBSERVED_INSTALLED'
} catch {
    $exception = $_.Exception
    $chain = @()
    while ($null -ne $exception) {
        $chain += [ordered]@{
            type = $exception.GetType().FullName
            hresult_hex = ('0x{0:X8}' -f ($exception.HResult -band 0xffffffffL))
        }
        $exception = $exception.InnerException
    }
    $result.exception_chain = $chain
    $result.status = 'OBSERVATION_FAILED'
    $exitCode = 1
} finally {
    for ($i=$com.Count-1; $i -ge 0; $i--) {
        try {
            $remaining = [Runtime.InteropServices.Marshal]::FinalReleaseComObject($com[$i])
            $result.com_release += [ordered]@{ reverse_index=$i; released=$true; remaining=$remaining }
        } catch {
            $result.com_release += [ordered]@{ reverse_index=$i; released=$false; type=$_.Exception.GetType().FullName }
            $exitCode = 1
        }
    }
    $result.completed_utc = [DateTime]::UtcNow.ToString('o')
    $result.exit_code = $exitCode
    $json = ($result | ConvertTo-Json -Depth 16) + [Environment]::NewLine
    $stream = [IO.File]::Open($OutputPath,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try {
        $outputBytes = [Text.UTF8Encoding]::new($false).GetBytes($json)
        $stream.Write($outputBytes,0,$outputBytes.Length)
        $stream.Flush($true)
    } finally { $stream.Dispose() }
}
[Console]::Out.WriteLine($json)
exit $exitCode
