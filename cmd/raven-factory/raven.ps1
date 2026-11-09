# Raven minion, build __BUILD_ID__: a Logos minion as a PowerShell script
# (Windows PowerShell 5.1, PowerShell 7). It checks in through the HTTP
# channel (reverse-call) and does two things, hello and echo.
#
# Its messages are PowerShell's own CLIXML (PSSerializer): Raven, its
# factory, translates them for core (raven.clixml). Its suite, raven.raven,
# seals nothing: what it sends is the CLIXML itself. It is a test minion for
# core's codec mechanics, and protects nothing.
#
#   -Trace     print each exchange
#   -Seconds   stop after this many seconds (0: run until stopped)
param([switch]$Trace, [int]$Seconds = 0)

$ErrorActionPreference = 'Stop'

$RavenBuild = '__BUILD_ID__'
$RavenVersion = __VERSION__
$RavenGreeting = __GREETING__
$OpHello = 1
$OpEcho = 2
$RavenOps = @(__OPS__)
$RavenConns = @(
  __CONNS__
)

function Write-Trace([string]$Text) {
  if ($Trace) { [Console]::Error.WriteLine("raven: $Text") }
}

# A UUID's 16 bytes in the order of its text form (not Guid.ToByteArray's).
function Get-UuidBytes([string]$Text) {
  $h = $Text -replace '-', ''
  $b = New-Object byte[] 16
  for ($i = 0; $i -lt 16; $i++) { $b[$i] = [Convert]::ToByte($h.Substring($i * 2, 2), 16) }
  return , $b
}

function New-RandomBytes([int]$Count) {
  $b = New-Object byte[] $Count
  $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
  $rng.GetBytes($b)
  $rng.Dispose()
  return , $b
}

# A UUID's text form from its 16 bytes.
function Format-Uuid([byte[]]$B) {
  $h = -join ($B | ForEach-Object { $_.ToString('x2') })
  return '{0}-{1}-{2}-{3}-{4}' -f $h.Substring(0, 8), $h.Substring(8, 4), $h.Substring(12, 4), $h.Substring(16, 4), $h.Substring(20)
}

function Test-SameBytes($A, $B) {
  if ($null -eq $A -or $null -eq $B) { return $false }
  return [Convert]::ToBase64String([byte[]]$A) -eq [Convert]::ToBase64String([byte[]]$B)
}

# The host, as a hello says it.
function Get-RavenOs {
  if ($PSVersionTable.PSEdition -ne 'Core') { return 'windows' }
  if ($IsWindows) { return 'windows' }
  if ($IsMacOS) { return 'darwin' }
  return 'linux'
}

function Get-RavenArch {
  $a = $null
  try { $a = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() } catch { $a = $env:PROCESSOR_ARCHITECTURE }
  switch -regex ("$a") {
    '^(X64|AMD64)$' { return 'amd64' }
    '^(Arm64|ARM64)$' { return 'arm64' }
    '^(X86|x86)$' { return '386' }
    '^(Arm|ARM)$' { return 'arm' }
  }
  return "$a".ToLowerInvariant()
}

# The machine id: its source's number and its value (docs: Sessions,
# Machine identity).
function Get-RavenMachine([string]$Os) {
  try {
    if ($Os -eq 'windows') {
      $g = (Get-ItemProperty -Path 'HKLM:\SOFTWARE\Microsoft\Cryptography' -Name MachineGuid).MachineGuid
      if ($g) { return @(5, "$g".ToLowerInvariant()) }
    } elseif ($Os -eq 'linux' -and (Test-Path '/etc/machine-id')) {
      $m = (Get-Content -Raw '/etc/machine-id').Trim()
      if ($m) { return @(4, $m.ToLowerInvariant()) }
    }
  } catch { }
  return @(8, [Environment]::MachineName.ToLowerInvariant())
}

function Get-RavenUser([string]$Os) {
  if ($Os -eq 'windows') {
    try { return [System.Security.Principal.WindowsIdentity]::GetCurrent().Name } catch { }
  }
  return [Environment]::UserName
}

function New-Hello {
  $os = Get-RavenOs
  $machine = Get-RavenMachine $os
  $proc = [System.Diagnostics.Process]::GetCurrentProcess().ProcessName
  return @{
    0  = 3
    1  = [int64]$PID
    2  = $os
    3  = Get-RavenArch
    4  = [Environment]::MachineName
    5  = Get-RavenUser $os
    7  = $proc
    10 = "raven $RavenVersion"
    11 = [int64]$machine[0]
    15 = [string]$machine[1]
    12 = @{ powershell = $PSVersionTable.PSVersion.ToString(); edition = "$($PSVersionTable.PSEdition)" }
    14 = @{ 1 = [int64]$script:IntervalMs; 2 = [int64]$script:Jitter }
    16 = [int64]$script:Conn.Place
  }
}

# A certificate check that takes any certificate, for a connection that
# does not verify TLS: compiled once, as a callback from another thread has
# no runspace for a script block.
$script:AnyCert = $null
function Get-AnyCertCallback {
  if ($null -eq $script:AnyCert) {
    Add-Type -TypeDefinition @'
public static class RavenAnyCert {
  public static bool Check(object sender, System.Security.Cryptography.X509Certificates.X509Certificate c,
    System.Security.Cryptography.X509Certificates.X509Chain chain, System.Net.Security.SslPolicyErrors errors) { return true; }
}
'@
    $script:AnyCert = [System.Delegate]::CreateDelegate([System.Net.Security.RemoteCertificateValidationCallback], [RavenAnyCert].GetMethod('Check'))
  }
  return $script:AnyCert
}

# Send-Body posts body to the connection's URL at index and returns what
# came back: the HTTP channel's 200 body, else an exception.
function Send-Body($Conn, [int]$Index, [byte[]]$Body) {
  $url = $Conn.Urls[$Index].TrimEnd('/') + $Conn.Path
  $req = [System.Net.HttpWebRequest]::Create($url)
  $req.Method = $Conn.Method
  $req.ContentType = 'application/octet-stream'
  $req.Timeout = $Conn.TimeoutMs
  $req.ReadWriteTimeout = $Conn.TimeoutMs
  if ($Conn.UserAgent) { $req.UserAgent = $Conn.UserAgent }
  foreach ($k in $Conn.Headers.Keys) {
    try { $req.Headers[$k] = $Conn.Headers[$k] } catch { Write-Trace "header $k not set: $($_.Exception.Message)" }
  }
  if (-not $Conn.TlsVerify -and $url.StartsWith('https:')) { $req.ServerCertificateValidationCallback = Get-AnyCertCallback }
  $req.ContentLength = $Body.Length
  $s = $req.GetRequestStream()
  $s.Write($Body, 0, $Body.Length)
  $s.Close()
  $resp = $req.GetResponse()
  try {
    $ms = New-Object System.IO.MemoryStream
    $resp.GetResponseStream().CopyTo($ms)
    return , $ms.ToArray()
  } finally { $resp.Close() }
}

# Invoke-Exchange sends items under the id Under and returns core's reply,
# a hashtable, or $null when core had nothing to say. The HTTP channel's
# body is offset(1) ‖ part 1 ‖ id(16) ‖ part 2, the payload the CLIXML.
function Invoke-Exchange([byte[]]$Under, [object[]]$Items) {
  $id = New-RandomBytes 16
  $msg = @{ 0 = 3; 1 = $id; 3 = $Items }
  $text = [System.Management.Automation.PSSerializer]::Serialize($msg, 32)
  $payload = [System.Text.Encoding]::UTF8.GetBytes($text)
  $offset = Get-Random -Minimum 0 -Maximum ([Math]::Min(256, $payload.Length + 1))
  $ms = New-Object System.IO.MemoryStream
  $ms.WriteByte([byte]$offset)
  $ms.Write($payload, 0, $offset)
  $ms.Write($Under, 0, 16)
  $ms.Write($payload, $offset, $payload.Length - $offset)
  $body = $ms.ToArray()

  $answer = $null
  $err = $null
  for ($i = 0; $i -lt $script:Conn.Urls.Count -and $null -eq $answer; $i++) {
    try { $answer = Send-Body $script:Conn $i $body } catch { $err = $_.Exception.Message }
  }
  if ($null -eq $answer) { throw "no reply: $err" }
  if ($answer.Length -lt 17) { throw "a reply of $($answer.Length) bytes" }
  $at = [int]$answer[0]
  if ($at + 17 -gt $answer.Length) { throw 'a reply whose offset is past it' }
  $rid = New-Object byte[] 16
  [Array]::Copy($answer, 1 + $at, $rid, 0, 16)
  if (-not (Test-SameBytes $rid $Under)) { throw 'a reply under another id' }
  $len = $answer.Length - 17
  if ($len -eq 0) { return $null }
  $data = New-Object byte[] $len
  [Array]::Copy($answer, 1, $data, 0, $at)
  [Array]::Copy($answer, 17 + $at, $data, $at, $len - $at)
  $reply = [System.Management.Automation.PSSerializer]::Deserialize([System.Text.Encoding]::UTF8.GetString($data))
  if (-not (Test-SameBytes $reply[2] $id)) { throw 'a reply to another message' }
  return $reply
}

# Invoke-Task runs a task and returns its result's out, or throws.
function Invoke-Task($Task) {
  $cmd = [int]$Task[2]
  if ($RavenOps -notcontains $cmd) { throw "command $cmd is not in this build" }
  switch ($cmd) {
    $OpHello { return $RavenGreeting }
    $OpEcho {
      $taskArgs = $Task[3]
      if ($null -eq $taskArgs -or $null -eq $taskArgs[1]) { throw 'echo: no text' }
      return [string]$taskArgs[1]
    }
  }
  throw "command $cmd is not Raven's"
}

$script:ConnIndex = 0
$script:Conn = $RavenConns[0]
$script:IntervalMs = $script:Conn.IntervalMs
$script:Jitter = $script:Conn.Jitter
$buildId = Get-UuidBytes $RavenBuild
$sid = $null
$needHello = $false
# Items sent and not yet taken: acks and results go again until an exchange
# goes through (a reply, or core's empty one: it took them and had nothing
# to say). One that fails (no answer, a 404) sends them again.
$pending = New-Object System.Collections.ArrayList
$stopAt = if ($Seconds -gt 0) { [DateTime]::UtcNow.AddSeconds($Seconds) } else { [DateTime]::MaxValue }

Write-Trace "build $RavenBuild, $($RavenConns.Count) connection(s), PowerShell $($PSVersionTable.PSVersion)"
while ([DateTime]::UtcNow -lt $stopAt) {
  $soon = $false
  $waitMs = $script:IntervalMs
  if ($null -eq $sid) {
    # Under the build id the only item core takes is one hello.
    $under = $buildId
    $items = [object[]]@(, (New-Hello))
  } else {
    $under = $sid
    $list = New-Object System.Collections.ArrayList
    if ($needHello) { [void]$list.Add((New-Hello)) }
    [void]$list.Add(@{ 0 = 7; 1 = 4 })
    foreach ($p in $pending) { [void]$list.Add($p) }
    $items = $list.ToArray()
  }
  try {
    $reply = Invoke-Exchange $under $items
    Write-Trace "sent $($items.Count) item(s) under $(Format-Uuid $under), reply: $(if ($null -eq $reply) { 'nothing' } else { "$(@($reply[3]).Count) item(s)" })"
    # Taken: what was pending went through.
    $pending.Clear()
    $needHello = $false
  } catch {
    Write-Trace "exchange failed: $($_.Exception.Message)"
    if ($RavenConns.Count -gt 1) {
      $script:ConnIndex = ($script:ConnIndex + 1) % $RavenConns.Count
      $script:Conn = $RavenConns[$script:ConnIndex]
      $needHello = $null -ne $sid
    }
    $reply = $null
    $items = $null
  }
  if ($null -ne $reply) {
    foreach ($it in @($reply[3])) {
      if ($null -eq $it) { continue }
      switch ([int]$it[0]) {
        4 {
          # hello_response: the session's id.
          $sid = [byte[]]$it[1]
          $soon = $true
          Write-Trace "session $(Format-Uuid $sid)"
        }
        8 {
          # task: run it now; its ack and result go in the next message.
          $task = [byte[]]$it[1]
          [void]$pending.Add(@{ 0 = 11; 1 = [object[]]@(, $task) })
          try {
            $out = Invoke-Task $it
            [void]$pending.Add(@{ 0 = 10; 1 = $task; 2 = 0; 3 = $true; 4 = $true; 5 = [System.Text.Encoding]::UTF8.GetBytes([string]$out) })
          } catch {
            [void]$pending.Add(@{ 0 = 10; 1 = $task; 2 = 0; 3 = $true; 4 = $false; 6 = $_.Exception.Message })
          }
          $soon = $true
        }
        5 {
          # sleep_update
          $script:IntervalMs = [int64]$it[1]
          if ($null -ne $it[2]) { $script:Jitter = [int]$it[2] }
        }
        6 { $needHello = $true; $soon = $true }
        16 { $waitMs = [Math]::Min($waitMs, [int64]$it[1]) }
        13 {
          $code = [int]$it[1]
          Write-Trace "err $code`: $($it[2])"
          if ($code -eq 5 -or $code -eq 6) {
            # The session is closed, or another process took it: start over.
            $sid = $null
            $pending.Clear()
            $soon = $true
          }
        }
      }
    }
  }
  if (-not $soon) {
    $j = [int]($waitMs * $script:Jitter / 100)
    $ms = $waitMs + (Get-Random -Minimum (-$j) -Maximum ($j + 1))
    # In doubles: Windows PowerShell 5.1 takes Min(int, double) as Int32's,
    # and the time left without -Seconds overflows it.
    $left = ($stopAt - [DateTime]::UtcNow).TotalMilliseconds
    Start-Sleep -Milliseconds ([int][Math]::Max([double]0, [Math]::Min([double]$ms, [double]$left)))
  }
}
Write-Trace 'stopped'
