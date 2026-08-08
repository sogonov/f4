# FISH+ remote helper, protocol version 1, PowerShell backend.
#
# Windows counterpart of helper.sh. Same wire protocol on the outside;
# .NET on the inside. The client that talks to us is the same Go program
# that speaks to /bin/sh; nothing on that side knows Windows.
#
# Paths on the wire are POSIX-shaped (Cygwin convention):
#   /c/Users/foo   <->   C:\Users\foo
#   //server/share <->   \\server\share
#   /              <->   virtual root, lists local drives as directories
# Every translation is done inside this file; the request stream stays
# byte-for-byte compatible with what helper.sh produces.
#
# See WINDOWS_PORT.md in this directory for the full design notes and
# the rationale behind every feature announced in the banner.

$F4TOKEN = '__F4_TOKEN__'
$F4PROTO = 1

# ---------------------------------------------------------------------
# Setup: force byte-safe, LF-only, prompt-free I/O.
# The wire protocol requires exact byte counts on both directions and
# LF line endings; anything else desynchronizes the session.
# ---------------------------------------------------------------------
$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'
$WarningPreference     = 'SilentlyContinue'
$VerbosePreference     = 'SilentlyContinue'
$DebugPreference       = 'SilentlyContinue'
$InformationPreference = 'SilentlyContinue'
$ConfirmPreference     = 'None'

# PSReadLine hooks TAB, cursor keys and buffer paints; even a NonInteractive
# session can pull it in through a profile. Nothing here reads from the
# console line editor.
if (Get-Module PSReadLine -ErrorAction SilentlyContinue) {
    Remove-Module PSReadLine -Force -ErrorAction SilentlyContinue
}

# The encoding the console host decodes stdin with. It has to be read
# before anything else touches the console, because it is the key that
# turns a line the host handed us back into the bytes that arrived on the
# wire: the host decodes stdin with its own code page (cp866, cp1252, ...)
# and never asks us. Reversing that decoding is what keeps a UTF-8 path
# byte-exact. [Console]::InputEncoding is deliberately NOT set: the host's
# line reader captured its encoding at startup and ignores later changes,
# so setting it would only desynchronize this key from reality.
$F4HostEnc = [Console]::InputEncoding

# UTF-8 without BOM on stdout; a BOM at the start of the banner would
# ruin the terminator's "starts a line" property.
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
try {
    [Console]::OutputEncoding = $utf8NoBom
    $OutputEncoding           = $utf8NoBom
} catch { }

# Silence the console host's own writer. Two things go through it that
# would corrupt the wire: the prompt it paints between commands, and the
# echo of every line it reads for us when stdin is a pipe. Nothing in this
# helper prints through it — every byte we emit goes to the raw stdout
# stream below — so muting it costs nothing and removes both hazards.
try { [Console]::SetOut([System.IO.TextWriter]::Null) } catch { }

# Raw byte streams. Everything binary — the "#<n>" frame during read,
# the base64 payload line during write — goes through these to bypass
# PS's text pipeline entirely. Wrap stdout in a buffered stream so the
# terminator is emitted as a single Write, not a syscall per character.
#
# stdin is deliberately NOT opened here. Merely opening it costs bytes:
# the stream buffers whatever has arrived, and those bytes then belong to
# neither reader — the console host cannot see them and we never ask for
# them once the host turns out to be the one doing the reading. It is
# opened on first use instead, which only happens where stdin is ours.
$F4RawOut   = [Console]::OpenStandardOutput()
$F4Out      = New-Object System.IO.BufferedStream($F4RawOut, 65536)
$F4LF       = [byte]0x0A
$F4NlBytes  = [byte[]]@($F4LF)

# ---------------------------------------------------------------------
# Path translation and safety guard.
# ---------------------------------------------------------------------

# Decodes a wire path to a Windows path. The empty string, the virtual
# root "/", and single-drive paths like "/c/" are handled explicitly so
# the caller does not have to.
function Convert-PosixToWin([string]$p) {
    if ($null -eq $p) { return $null }
    if ($p -eq '' -or $p -eq '/') { return '' }
    # UNC: //server/share/rest -> \\server\share\rest
    if ($p.StartsWith('//')) {
        $rest = $p.Substring(2).Replace('/', '\')
        return '\\' + $rest
    }
    if (-not $p.StartsWith('/')) {
        # Relative paths never enter helper.sh either; every wire path
        # comes from a pwd or an enum, both of which return absolutes.
        return $p.Replace('/', '\')
    }
    # /c/foo/bar or /c
    $tail = $p.Substring(1)
    $slash = $tail.IndexOf('/')
    if ($slash -lt 0) {
        # /c -> C:\
        if ($tail.Length -eq 1) { return $tail.ToUpper() + ':\' }
        # /something-not-a-drive at top level; not addressable on Windows
        return $tail.Replace('/', '\')
    }
    $drive = $tail.Substring(0, $slash)
    $rest  = $tail.Substring($slash + 1)
    if ($drive.Length -eq 1) {
        $win = $drive.ToUpper() + ':\' + $rest.Replace('/', '\')
        return $win
    }
    # Not a drive-shaped first segment: treat as-is with separator swap.
    return $p.Substring(1).Replace('/', '\')
}

# Encodes a Windows path as a wire path. The result never ends in a
# separator (except the virtual root itself), which matches what
# helper.sh's tools produce.
function Convert-WinToPosix([string]$w) {
    if ($null -eq $w -or $w -eq '') { return '/' }
    if ($w.StartsWith('\\')) {
        # \\server\share\rest
        $tail = $w.Substring(2).Replace('\', '/')
        return '//' + $tail.TrimEnd('/')
    }
    if ($w.Length -ge 2 -and $w[1] -eq ':') {
        $drive = [char]::ToLower($w[0])
        $rest  = if ($w.Length -gt 2) { $w.Substring(2).TrimStart('\').Replace('\', '/') } else { '' }
        if ($rest -eq '') { return '/' + $drive }
        return '/' + $drive + '/' + $rest.TrimEnd('/')
    }
    # Path without a drive letter, no UNC — probably came from an odd
    # cmdlet result. Fall back to naive translation.
    return '/' + $w.Replace('\', '/').TrimStart('/').TrimEnd('/')
}

# The virtual root is not backed by a filesystem: only "/" is virtual;
# every other path resolves to something real.
function Test-VirtualRoot([string]$p) { return $p -eq '' -or $p -eq '/' }

# Enumerates the fixed drives on this host. Removable/network drives are
# included as well so a user who plugged a stick in can see it under /d;
# the cost is one extra stat on the wire when the drive is absent.
function Get-RootEntries {
    $result = New-Object System.Collections.Generic.List[hashtable]
    foreach ($d in [System.IO.DriveInfo]::GetDrives()) {
        if (-not $d.IsReady) {
            # An unready drive still shows up: the panel can display it,
            # and enum-ing into it will fail cleanly with "not a directory".
        }
        $name = $d.Name.TrimEnd('\', ':').ToLower()
        if ($name.Length -eq 0) { continue }
        $result.Add(@{
            Name  = $name
            IsDir = $true
            Size  = 0L
            MTime = [DateTimeOffset]::FromUnixTimeSeconds(0)
            ATime = [DateTimeOffset]::FromUnixTimeSeconds(0)
            CTime = [DateTimeOffset]::FromUnixTimeSeconds(0)
            Mode  = 0x41ED   # 040755 dir
        })
    }
    return $result
}

# f4_safe_target port: absolute path, no dot-dot segments, and not the
# virtual root (which is meaningful for enum but not for a destructive
# op). Called AFTER the wire path has been translated to Windows.
function Test-SafeTarget([string]$winPath) {
    if ([string]::IsNullOrEmpty($winPath)) { return $false }
    $lower = $winPath.ToLower()
    if ($lower -like '*\..\*' -or $lower -like '*/../*') { return $false }
    if ($lower.EndsWith('\..') -or $lower.EndsWith('/..')) { return $false }
    # Absolute: either drive-letter (X:\) or UNC (\\srv\share).
    if ($winPath.Length -ge 3 -and $winPath[1] -eq ':' -and ($winPath[2] -eq '\' -or $winPath[2] -eq '/')) { return $true }
    if ($winPath.StartsWith('\\')) { return $true }
    return $false
}

# ---------------------------------------------------------------------
# Wire I/O primitives.
# Every emit that touches stdout goes through these; nothing else prints.
# ---------------------------------------------------------------------

$script:F4ID = 0

function Write-BytesRaw([byte[]]$b) {
    if ($null -eq $b -or $b.Length -eq 0) { return }
    $F4Out.Write($b, 0, $b.Length)
}

function Write-LineBytes([byte[]]$b) {
    if ($null -ne $b -and $b.Length -gt 0) {
        $F4Out.Write($b, 0, $b.Length)
    }
    $F4Out.Write($F4NlBytes, 0, 1)
}

function Write-Line([string]$s) {
    if ($null -eq $s) { $s = '' }
    $bytes = $utf8NoBom.GetBytes($s)
    Write-LineBytes $bytes
}

function Write-Frame([byte[]]$data) {
    $n = if ($null -eq $data) { 0 } else { $data.Length }
    Write-Line ("#" + $n)
    if ($n -gt 0) { Write-BytesRaw $data }
}

# Reduce every disturbing byte in an error message to a plain space so
# the client always parses it as one line.
function Format-Flat([string]$s) {
    if ([string]::IsNullOrEmpty($s)) { return '' }
    $sb = New-Object System.Text.StringBuilder $s.Length
    foreach ($c in $s.ToCharArray()) {
        switch ($c) {
            "`r" { [void]$sb.Append(' ') }
            "`n" { [void]$sb.Append(' ') }
            "`t" { [void]$sb.Append(' ') }
            default { [void]$sb.Append($c) }
        }
    }
    return $sb.ToString()
}

function Write-End([string]$status, [string]$msg) {
    $line = ".$F4TOKEN $($script:F4ID) $status"
    if (-not [string]::IsNullOrEmpty($msg)) {
        $line = $line + ' ' + (Format-Flat $msg)
    }
    Write-Line $line
    $F4Out.Flush()
}

function Write-Ok { Write-End 'ok' $null }
function Write-Err([string]$msg) { Write-End 'err' $msg }

# Reads one LF-terminated line from stdin as raw bytes, decoded UTF-8.
# Returns $null on EOF. The buffered read matches what helper.sh sees
# from "IFS= read -r".
#
# Which of the two paths below is usable is decided by the host, not by us.
# A console host with a redirected stdin keeps a read pending on that handle
# for its own line reader, and it wins every race: bytes that arrive after
# the helper started are consumed by the host and a stream read here blocks
# forever. So the host's reader is asked first, and the raw stream is used
# only where the host refuses to read at all (-NonInteractive), which is the
# standalone case where nothing competes for stdin anyway.
$script:F4LineBuf = New-Object System.IO.MemoryStream
$script:F4ReadMode = $null      # 'host' | 'stream'

# One line through the host's reader, converted back to the bytes that
# arrived. Returns $null on EOF; the host strips the line terminator
# itself, CR included.
function Read-HostLineBytes {
    $line = $host.UI.ReadLine()
    if ($null -eq $line) { return $null }
    return $F4HostEnc.GetBytes($line)
}

# Opens stdin the first time a stream read needs it. See the note at the
# top: opening it eagerly would swallow bytes the console host is meant
# to hand us.
$script:F4In = $null
function Get-StdIn {
    if ($null -eq $script:F4In) { $script:F4In = [Console]::OpenStandardInput() }
    return $script:F4In
}

function Read-StreamLineBytes {
    $in = Get-StdIn
    $script:F4LineBuf.SetLength(0)
    $one = New-Object 'byte[]' 1
    while ($true) {
        $n = $in.Read($one, 0, 1)
        if ($n -le 0) {
            if ($script:F4LineBuf.Length -eq 0) { return $null }
            break
        }
        if ($one[0] -eq 0x0A) { break }
        if ($one[0] -eq 0x0D) { continue }   # tolerate CRLF
        $script:F4LineBuf.WriteByte($one[0])
    }
    return $script:F4LineBuf.ToArray()
}

function Read-LineBytes {
    if ($null -eq $script:F4ReadMode) {
        # The probe doubles as the first read: a host that answers has
        # already consumed the line, so it must not be read twice.
        try {
            $b = Read-HostLineBytes
            $script:F4ReadMode = 'host'
            return $b
        } catch {
            $script:F4ReadMode = 'stream'
        }
    }
    if ($script:F4ReadMode -eq 'host') { return Read-HostLineBytes }
    return Read-StreamLineBytes
}

function Read-Line {
    $b = Read-LineBytes
    if ($null -eq $b) { return $null }
    return $utf8NoBom.GetString($b)
}

# Reads a path line: base64-decode iff prefixed with '~'; otherwise
# take the bytes verbatim. Returns the wire path (POSIX-shape); the
# caller passes it to Convert-PosixToWin when it needs the OS form.
function Read-PathLine {
    $line = Read-Line
    if ($null -eq $line) { throw 'eof' }
    if ($line.StartsWith('~')) {
        try {
            $raw = [Convert]::FromBase64String($line.Substring(1))
            return $utf8NoBom.GetString($raw)
        } catch {
            throw 'bad base64 path'
        }
    }
    return $line
}

# Read the exact requested number of bytes from stdin.
#
# Only reachable while stdin belongs to us. Once the console host is doing
# the reading there is no byte-exact path left — the host hands out whole
# decoded lines and nothing smaller — so a raw payload is refused instead
# of being half-read. The client never asks for one: the banner announces
# write:b64, and wmode accepts nothing else.
function Read-ExactBytes([int]$n) {
    if ($script:F4ReadMode -eq 'host') {
        throw 'raw payloads need byte-exact stdin, which this host does not give; use b64'
    }
    if ($n -le 0) { return [byte[]]@() }
    $in = Get-StdIn
    $buf = New-Object 'byte[]' $n
    $off = 0
    while ($off -lt $n) {
        $r = $in.Read($buf, $off, $n - $off)
        if ($r -le 0) { throw 'eof during payload' }
        $off += $r
    }
    return $buf
}

# Numeric-argument helpers matching helper.sh's f4_num, plus a small
# bounds check for the many "count" arguments in the wire grammar.
function Test-IsUInt([string]$s) {
    if ([string]::IsNullOrEmpty($s)) { return $false }
    foreach ($c in $s.ToCharArray()) {
        if ($c -lt '0' -or $c -gt '9') { return $false }
    }
    return $true
}
function Parse-UInt([string]$s) {
    if (-not (Test-IsUInt $s)) { throw "not a number: $s" }
    return [int64]$s
}

# ---------------------------------------------------------------------
# Entry synthesis: turn a Windows FileSystemInfo into a stat-shaped
# line the client's parseStatEntry expects.
# Format: "%f %s %Y %X %Z %u %g %n"   (mode in hex)
# ---------------------------------------------------------------------

# 040000 dir, 100000 reg, 120000 lnk in octal; here in hex.
$F4MODE_DIR = 0x4000
$F4MODE_REG = 0x8000
$F4MODE_LNK = 0xA000
$F4PERM_DEF = 0x1ED     # 0755
$F4PERM_RO  = 0x1A4     # 0644

function Get-EntryMode {
    param([System.IO.FileSystemInfo]$fi)
    $attr = $fi.Attributes
    $type = if ($attr -band [System.IO.FileAttributes]::ReparsePoint) { $F4MODE_LNK }
            elseif ($attr -band [System.IO.FileAttributes]::Directory)  { $F4MODE_DIR }
            else { $F4MODE_REG }
    $perm = if ($attr -band [System.IO.FileAttributes]::ReadOnly) { $F4PERM_RO } else { $F4PERM_DEF }
    return $type -bor $perm
}

function Get-EpochSecs([datetime]$t) {
    try {
        return [int64]([DateTimeOffset]::new($t.ToUniversalTime(), [TimeSpan]::Zero)).ToUnixTimeSeconds()
    } catch { return 0 }
}

# Emits one stat-format entry line. $nameOverride lets the caller
# substitute the full path (used by tree search where the client needs
# it) or force the name to "." for a self-stat.
function Emit-StatEntry {
    param(
        [System.IO.FileSystemInfo]$fi,
        [string]$nameOverride,
        [int64]$sizeOverride = -1
    )
    $mode = Get-EntryMode $fi
    $size = if ($sizeOverride -ge 0) {
                $sizeOverride
            } elseif ($fi -is [System.IO.FileInfo]) {
                $fi.Length
            } else { 0 }
    $mtime = Get-EpochSecs $fi.LastWriteTimeUtc
    $atime = Get-EpochSecs $fi.LastAccessTimeUtc
    $ctime = Get-EpochSecs $fi.CreationTimeUtc
    $name = if ([string]::IsNullOrEmpty($nameOverride)) { $fi.Name } else { $nameOverride }
    Write-Line ("{0:x} {1} {2} {3} {4} 0 0 {5}" -f $mode, $size, $mtime, $atime, $ctime, $name)
}

# Emits the mode marker the client's parseListing keys off of.
function Emit-ModeLine { Write-Line 'M stat' }

# ---------------------------------------------------------------------
# Cmd-* implementations. One function per wire command.
# ---------------------------------------------------------------------

function Cmd-Noop { Write-Ok }

function Cmd-Pwd {
    try {
        $w = (Get-Location).Path
        Write-Line (Convert-WinToPosix $w)
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Ping {
    try {
        $p = Read-PathLine
        Write-Line $p
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Enum {
    try {
        $wire = Read-PathLine
        if (Test-VirtualRoot $wire) {
            Emit-ModeLine
            foreach ($r in Get-RootEntries) {
                # Root pseudo-entries look like dirs with mtime 0.
                Write-Line ("{0:x} 0 0 0 0 0 0 {1}" -f $r.Mode, $r.Name)
            }
            Write-Ok
            return
        }
        $win = Convert-PosixToWin $wire
        if (-not (Test-Path -LiteralPath $win -PathType Container)) {
            Write-Err 'not a directory'
            return
        }
        Emit-ModeLine
        # -Force includes hidden and system entries; -ErrorAction Stop
        # would abort on the first inaccessible child, which is not
        # what a panel wants.
        $items = @()
        try {
            $items = Get-ChildItem -LiteralPath $win -Force -ErrorAction Stop
        } catch {
            Write-Err $_.Exception.Message
            return
        }
        foreach ($it in $items) {
            try { Emit-StatEntry -fi $it } catch { }
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-IsDirs {
    param([string]$countArg)
    try {
        $n = Parse-UInt $countArg
        $paths = New-Object 'string[]' $n
        for ($i = 0; $i -lt $n; $i++) { $paths[$i] = Read-PathLine }
        foreach ($p in $paths) {
            if (Test-VirtualRoot $p) { Write-Line '1'; continue }
            $w = Convert-PosixToWin $p
            if ([System.IO.Directory]::Exists($w)) { Write-Line '1' } else { Write-Line '0' }
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Info {
    param([bool]$follow)
    try {
        $wire = Read-PathLine
        if (Test-VirtualRoot $wire) {
            Emit-ModeLine
            # The format operator binds tighter than -bor, so the mode has to
            # be combined before it is formatted or the string gets ored.
            Write-Line ("{0:x} 0 0 0 0 0 0 /" -f ($F4MODE_DIR -bor $F4PERM_DEF))
            Write-Ok
            return
        }
        $win = Convert-PosixToWin $wire
        if (-not (Test-Path -LiteralPath $win)) {
            Write-Err 'no such file'
            return
        }
        $fi = Get-Item -LiteralPath $win -Force -ErrorAction Stop
        if ($follow -and ($fi.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
            try {
                $resolved = [System.IO.Path]::GetFullPath($fi.Target)
                if (Test-Path -LiteralPath $resolved) {
                    $fi = Get-Item -LiteralPath $resolved -Force -ErrorAction Stop
                }
            } catch { }
        }
        Emit-ModeLine
        # Name comes from the request on the client side, so what we
        # emit is only for shape.
        Emit-StatEntry -fi $fi -nameOverride $fi.Name
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-RdLink {
    try {
        $wire = Read-PathLine
        $win  = Convert-PosixToWin $wire
        $fi   = Get-Item -LiteralPath $win -Force -ErrorAction Stop
        if (-not ($fi.Attributes -band [System.IO.FileAttributes]::ReparsePoint)) {
            Write-Err 'not a symbolic link'
            return
        }
        $t = $fi.Target
        if ($null -eq $t -or $t -eq '') {
            Write-Err 'no target'
            return
        }
        # If the target is a Windows path we translate; otherwise emit
        # as-is so a relative symlink's target survives unchanged.
        if ($t -match '^[A-Za-z]:\\' -or $t.StartsWith('\\')) {
            Write-Line (Convert-WinToPosix $t)
        } else {
            Write-Line ($t -replace '\\', '/')
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Mkdir {
    try {
        $wire = Read-PathLine
        $win  = Convert-PosixToWin $wire
        if (-not (Test-SafeTarget $win)) { Write-Err 'unsafe path'; return }
        [void](New-Item -ItemType Directory -Path $win -Force -ErrorAction Stop)
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Rm {
    try {
        $wire = Read-PathLine
        $win  = Convert-PosixToWin $wire
        if (-not (Test-SafeTarget $win)) { Write-Err 'unsafe path'; return }
        if (Test-Path -LiteralPath $win) {
            Remove-Item -LiteralPath $win -Force -ErrorAction Stop
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Rmdir {
    try {
        $wire = Read-PathLine
        $win  = Convert-PosixToWin $wire
        if (-not (Test-SafeTarget $win)) { Write-Err 'unsafe path'; return }
        [System.IO.Directory]::Delete($win, $false)
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Rmtree {
    try {
        $wire = Read-PathLine
        $win  = Convert-PosixToWin $wire
        if (-not (Test-SafeTarget $win)) { Write-Err 'unsafe path'; return }
        if (Test-Path -LiteralPath $win) {
            Remove-Item -LiteralPath $win -Recurse -Force -ErrorAction Stop
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Mv {
    try {
        $src = Convert-PosixToWin (Read-PathLine)
        $dst = Convert-PosixToWin (Read-PathLine)
        if (-not (Test-SafeTarget $src)) { Write-Err 'unsafe path'; return }
        if (-not (Test-SafeTarget $dst)) { Write-Err 'unsafe path'; return }
        Move-Item -LiteralPath $src -Destination $dst -Force -ErrorAction Stop
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Cp {
    try {
        $src = Convert-PosixToWin (Read-PathLine)
        $dst = Convert-PosixToWin (Read-PathLine)
        if (-not (Test-SafeTarget $src)) { Write-Err 'unsafe path'; return }
        if (-not (Test-SafeTarget $dst)) { Write-Err 'unsafe path'; return }
        Copy-Item -LiteralPath $src -Destination $dst -Recurse -Force -ErrorAction Stop
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

# chmod on Windows is a projection: any write bit -> clear ReadOnly,
# otherwise set ReadOnly. Nothing else NTFS ACLs express is touched.
function Cmd-Chmod {
    param([string]$modeArg)
    try {
        $wire = Read-PathLine
        $win  = Convert-PosixToWin $wire
        if (-not (Test-SafeTarget $win)) { Write-Err 'unsafe path'; return }
        if ($modeArg -match '[^0-7]' -or [string]::IsNullOrEmpty($modeArg)) {
            Write-Err 'bad mode'
            return
        }
        $m = [Convert]::ToInt32($modeArg, 8)
        $ro = -not (($m -band 0x92) -ne 0)   # user/group/other write bits
        $fi = Get-Item -LiteralPath $win -Force -ErrorAction Stop
        if ($ro) {
            $fi.Attributes = $fi.Attributes -bor [System.IO.FileAttributes]::ReadOnly
        } else {
            $fi.Attributes = $fi.Attributes -band (-bnot [System.IO.FileAttributes]::ReadOnly)
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

# chown is meaningless without NTFS ACL manipulation, which we
# deliberately don't do. Announced as a feature (so the client offers
# the menu), refused with a clear message when invoked.
function Cmd-Chown {
    param([string]$uid, [string]$gid)
    try {
        $wire = Read-PathLine
        Write-Err 'chown is not supported on this Windows host'
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Utime {
    param([string]$mtimeArg, [string]$atimeArg)
    try {
        $wire = Read-PathLine
        $win  = Convert-PosixToWin $wire
        if (-not (Test-SafeTarget $win)) { Write-Err 'unsafe path'; return }
        if (($mtimeArg -ne '-' -and -not (Test-IsUInt $mtimeArg)) -or
            ($atimeArg -ne '-' -and -not (Test-IsUInt $atimeArg))) {
            Write-Err 'bad timestamp'
            return
        }
        if ($mtimeArg -eq '-' -and $atimeArg -eq '-') {
            Write-Err 'nothing to change'
            return
        }
        if (-not (Test-Path -LiteralPath $win)) { Write-Err 'no such file'; return }
        if ($mtimeArg -ne '-') {
            $t = [DateTimeOffset]::FromUnixTimeSeconds([int64]$mtimeArg).UtcDateTime
            [System.IO.File]::SetLastWriteTimeUtc($win, $t)
        }
        if ($atimeArg -ne '-') {
            $t = [DateTimeOffset]::FromUnixTimeSeconds([int64]$atimeArg).UtcDateTime
            [System.IO.File]::SetLastAccessTimeUtc($win, $t)
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-Trunc {
    param([string]$sizeArg)
    try {
        $wire = Read-PathLine
        $win  = Convert-PosixToWin $wire
        if (-not (Test-SafeTarget $win)) { Write-Err 'unsafe path'; return }
        if (-not (Test-IsUInt $sizeArg)) { Write-Err 'bad size'; return }
        $size = [int64]$sizeArg
        if ([System.IO.Directory]::Exists($win)) { Write-Err 'is a directory'; return }
        if ($size -eq 0) {
            # Same semantics as ": > file" — creates the file if absent.
            [System.IO.File]::WriteAllBytes($win, [byte[]]@())
            Write-Ok
            return
        }
        $fs = [System.IO.File]::Open($win, [System.IO.FileMode]::OpenOrCreate,
                                     [System.IO.FileAccess]::Write,
                                     [System.IO.FileShare]::Read)
        try { $fs.SetLength($size) } finally { $fs.Dispose() }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

# ---------------------------------------------------------------------
# read: server -> client range transfer. Emits "S <size>" + one binary
# frame with the bytes actually read. Length 0 means "to the end".
# ---------------------------------------------------------------------
function Cmd-Read {
    param([string]$offArg, [string]$lenArg)
    try {
        $wire = Read-PathLine
        if (-not (Test-IsUInt $offArg) -or -not (Test-IsUInt $lenArg)) {
            Write-Err 'bad range'; return
        }
        $off = [int64]$offArg
        $len = [int64]$lenArg
        $win = Convert-PosixToWin $wire
        if (-not (Test-Path -LiteralPath $win)) { Write-Err 'no such file'; return }
        if ([System.IO.Directory]::Exists($win)) { Write-Err 'is a directory'; return }
        $fs = [System.IO.File]::Open($win, [System.IO.FileMode]::Open,
                                     [System.IO.FileAccess]::Read,
                                     [System.IO.FileShare]::ReadWrite)
        try {
            $size = $fs.Length
            $avail = 0L
            if ($off -lt $size) {
                $avail = $size - $off
                if ($len -gt 0 -and $len -lt $avail) { $avail = $len }
            }
            Write-Line ("S " + $size)
            Write-Line ("#" + $avail)
            if ($avail -gt 0) {
                [void]$fs.Seek($off, [System.IO.SeekOrigin]::Begin)
                # Stream in 64 KiB blocks straight to stdout; we already
                # wrote the "#<n>" header, and Read-BytesFrom-Stream
                # keeps its own byte count so a short read is fatal.
                $remain = $avail
                $buf = New-Object 'byte[]' 65536
                while ($remain -gt 0) {
                    $want = [int]([Math]::Min([int64]$buf.Length, $remain))
                    $got  = $fs.Read($buf, 0, $want)
                    if ($got -le 0) { throw 'short read' }
                    $F4Out.Write($buf, 0, $got)
                    $remain -= $got
                }
            }
            Write-Ok
        } finally { $fs.Dispose() }
    } catch { Write-Err $_.Exception.Message }
}

# ---------------------------------------------------------------------
# write: client -> server. Only "b64" encoding is announced (write:b64),
# so the payload is one base64 line read via Read-Line.
# The reply protocol requires a "D" line before the terminator to say
# the payload was drained; without it the client marks the session
# broken.
# ---------------------------------------------------------------------
function Cmd-Write {
    param([string]$offArg, [string]$lenArg, [string]$enc)
    try {
        $wire = Read-PathLine
        $err = $null
        if (-not (Test-IsUInt $offArg) -or -not (Test-IsUInt $lenArg)) {
            $err = 'bad range'
        }
        $win = Convert-PosixToWin $wire
        if (-not $err -and -not (Test-SafeTarget $win)) {
            $err = 'unsafe path'
        }
        if (-not $err -and [System.IO.Directory]::Exists($win)) {
            $err = 'is a directory'
        }
        switch ($enc) {
            'b64' {
                # Payload line arrives whether or not we can write; we
                # MUST read it to keep the stream aligned.
                $line = Read-Line
                if ($null -eq $line) { throw 'eof' }
                if ($err) {
                    Write-Line 'D'
                    Write-Err $err
                    return
                }
                try {
                    $bytes = [Convert]::FromBase64String($line)
                } catch {
                    Write-Line 'D'
                    Write-Err 'bad base64 payload'
                    return
                }
                # length arg must match the decoded byte count; the
                # helper.sh port trusts the decoded count and writes it,
                # so match that behaviour.
                $off = [int64]$offArg
                $fs = [System.IO.File]::Open($win, [System.IO.FileMode]::OpenOrCreate,
                                             [System.IO.FileAccess]::Write,
                                             [System.IO.FileShare]::Read)
                try {
                    [void]$fs.Seek($off, [System.IO.SeekOrigin]::Begin)
                    $fs.Write($bytes, 0, $bytes.Length)
                } finally { $fs.Dispose() }
                Write-Line 'D'
                Write-Ok
            }
            'raw' {
                # Raw payload: read exactly $lenArg bytes from stdin.
                # We don't advertise write:raw as a preference, but the
                # client can still ask for it explicitly via wmode.
                $n = if (Test-IsUInt $lenArg) { [int64]$lenArg } else { 0 }
                if ($err) {
                    if ($n -gt 0) { [void](Read-ExactBytes $n) }
                    Write-Line 'D'
                    Write-Err $err
                    return
                }
                $bytes = Read-ExactBytes $n
                $off = [int64]$offArg
                $fs = [System.IO.File]::Open($win, [System.IO.FileMode]::OpenOrCreate,
                                             [System.IO.FileAccess]::Write,
                                             [System.IO.FileShare]::Read)
                try {
                    [void]$fs.Seek($off, [System.IO.SeekOrigin]::Begin)
                    $fs.Write($bytes, 0, $bytes.Length)
                } finally { $fs.Dispose() }
                Write-Line 'D'
                Write-Ok
            }
            default {
                Write-Err 'bad payload encoding'
            }
        }
    } catch { Write-Err $_.Exception.Message }
}

# ---------------------------------------------------------------------
# patch: rebuild <dst> out of segments of <src> (S <off> <len>) and
# literal bytes (D <len> [+ payload]). See helper.sh's f4_cmd_patch.
# ---------------------------------------------------------------------
function Cmd-Patch {
    param([string]$nsegsArg, [string]$enc)
    try {
        $srcWire = Read-PathLine
        $dstWire = Read-PathLine
        if ($enc -ne 'raw' -and $enc -ne 'b64') { Write-Err 'bad payload encoding'; return }
        if (-not (Test-IsUInt $nsegsArg)) { Write-Err 'bad segment count'; return }
        $nsegs = [int]$nsegsArg
        $src = Convert-PosixToWin $srcWire
        $dst = Convert-PosixToWin $dstWire
        $perr = $null
        $made = $false
        if (-not (Test-SafeTarget $dst)) { $perr = 'unsafe path' }
        elseif ([System.IO.Directory]::Exists($dst)) { $perr = 'is a directory' }
        elseif (-not (Test-Path -LiteralPath $src -PathType Leaf)) { $perr = 'cannot read the original file' }
        else {
            try {
                [System.IO.File]::WriteAllBytes($dst, [byte[]]@())
                $made = $true
            } catch {
                $perr = $_.Exception.Message
            }
        }
        $srcFs = $null
        $dstFs = $null
        try {
            if (-not $perr) {
                $srcFs = [System.IO.File]::Open($src, [System.IO.FileMode]::Open,
                                                [System.IO.FileAccess]::Read,
                                                [System.IO.FileShare]::ReadWrite)
                $dstFs = [System.IO.File]::Open($dst, [System.IO.FileMode]::Append,
                                                [System.IO.FileAccess]::Write,
                                                [System.IO.FileShare]::Read)
            }
            for ($i = 0; $i -lt $nsegs; $i++) {
                $head = Read-Line
                if ($null -eq $head) { throw 'eof' }
                $parts = $head.Split(' ', 3)
                switch ($parts[0]) {
                    'S' {
                        if ($parts.Length -lt 3 -or -not (Test-IsUInt $parts[1]) -or -not (Test-IsUInt $parts[2])) {
                            if (-not $perr) { $perr = 'bad segment' }
                            continue
                        }
                        if ($perr) { continue }
                        $so = [int64]$parts[1]
                        $sl = [int64]$parts[2]
                        [void]$srcFs.Seek($so, [System.IO.SeekOrigin]::Begin)
                        $buf = New-Object 'byte[]' 65536
                        $left = $sl
                        while ($left -gt 0) {
                            $want = [int]([Math]::Min([int64]$buf.Length, $left))
                            $got  = $srcFs.Read($buf, 0, $want)
                            if ($got -le 0) { $perr = 'copying from the original failed'; break }
                            $dstFs.Write($buf, 0, $got)
                            $left -= $got
                        }
                    }
                    'D' {
                        if ($parts.Length -lt 2 -or -not (Test-IsUInt $parts[1])) {
                            if ($made) { Remove-Item -LiteralPath $dst -Force -ErrorAction SilentlyContinue }
                            Write-Err 'bad segment length'
                            return
                        }
                        $dl = [int64]$parts[1]
                        if ($enc -eq 'b64') {
                            $payload = Read-Line
                            if ($null -eq $payload) { throw 'eof' }
                            if (-not $perr) {
                                try {
                                    $bytes = [Convert]::FromBase64String($payload)
                                    $dstFs.Write($bytes, 0, $bytes.Length)
                                } catch {
                                    $perr = 'bad base64 payload'
                                }
                            }
                        } else {
                            if ($perr) {
                                if ($dl -gt 0) { [void](Read-ExactBytes $dl) }
                            } else {
                                $bytes = Read-ExactBytes $dl
                                $dstFs.Write($bytes, 0, $bytes.Length)
                            }
                        }
                    }
                    default {
                        if ($made) { Remove-Item -LiteralPath $dst -Force -ErrorAction SilentlyContinue }
                        Write-Err 'bad segment'
                        return
                    }
                }
            }
        } finally {
            if ($null -ne $srcFs) { $srcFs.Dispose() }
            if ($null -ne $dstFs) { $dstFs.Dispose() }
        }
        Write-Line 'D'
        if ($perr) {
            if ($made) { Remove-Item -LiteralPath $dst -Force -ErrorAction SilentlyContinue }
            Write-Err $perr
        } else {
            Write-Ok
        }
    } catch { Write-Err $_.Exception.Message }
}

# ---------------------------------------------------------------------
# grep: byte offset per match, up to <limit> matches. Emits offsets in
# the same order they appear in the file, one per line.
# The client passes the byte-oriented offset semantics helper.sh has
# (grep -abo), so we count bytes in the file, not codepoints.
# ---------------------------------------------------------------------
function Cmd-Grep {
    param([string]$modeArg, [string]$limArg)
    try {
        $pat = Read-PathLine       # pattern (arrives as a path line for base64 handling)
        $wire = Read-PathLine
        if (-not (Test-IsUInt $limArg)) { Write-Err 'bad limit'; return }
        $limit = [int]$limArg
        $mode = if ($modeArg.StartsWith('f')) { 'fixed' }
                elseif ($modeArg.StartsWith('e')) { 'regex' }
                else { Write-Err 'bad grep mode'; return }
        $ci = $modeArg.EndsWith('i')
        $win = Convert-PosixToWin $wire
        if (-not (Test-Path -LiteralPath $win -PathType Leaf)) { Write-Err 'not a regular file'; return }

        $patBytes = $utf8NoBom.GetBytes($pat)
        $rx = $null
        if ($mode -eq 'regex') {
            $opts = [System.Text.RegularExpressions.RegexOptions]::CultureInvariant
            if ($ci) { $opts = $opts -bor [System.Text.RegularExpressions.RegexOptions]::IgnoreCase }
            $rx = New-Object System.Text.RegularExpressions.Regex($pat, $opts)
        }

        $cmp = if ($ci) { [System.StringComparison]::OrdinalIgnoreCase }
               else     { [System.StringComparison]::Ordinal }

        $fs = [System.IO.File]::Open($win, [System.IO.FileMode]::Open,
                                     [System.IO.FileAccess]::Read,
                                     [System.IO.FileShare]::ReadWrite)
        try {
            # The file is walked as the byte stream it is rather than through
            # a StreamReader: the reader hides how long a line's terminator
            # was, and on a CRLF file — the common case here — every offset
            # after the first line would be short by one byte per line.
            # A CR before the LF stays part of the line, which is what grep
            # sees too.
            $buf  = New-Object 'byte[]' 65536
            $acc  = New-Object System.IO.MemoryStream
            $count = 0
            $lineStart = 0L
            $pos = 0L
            $stop = $false
            while (-not $stop) {
                $got = $fs.Read($buf, 0, $buf.Length)
                if ($got -le 0) { break }
                for ($i = 0; $i -lt $got; $i++) {
                    if ($buf[$i] -ne 0x0A) { $acc.WriteByte($buf[$i]); continue }
                    $count = Emit-GrepHits $acc $lineStart $pat $rx $cmp $count $limit
                    $acc.SetLength(0)
                    $lineStart = $pos + $i + 1
                    if ($count -ge $limit) { $stop = $true; break }
                }
                $pos += $got
            }
            # A last line without a trailing LF still counts, as it does for
            # grep.
            if (-not $stop -and $acc.Length -gt 0) {
                [void](Emit-GrepHits $acc $lineStart $pat $rx $cmp $count $limit)
            }
        } finally { $fs.Dispose() }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

# Emits one offset per match inside a single line, the way "grep -a -b -o"
# does: the offset of the match itself, not of the line holding it, and one
# line of output per match. Returns the running match count.
function Emit-GrepHits {
    param(
        [System.IO.MemoryStream]$acc,
        [int64]$lineStart,
        [string]$pat,
        $rx,
        [System.StringComparison]$cmp,
        [int]$count,
        [int]$limit
    )
    if ($count -ge $limit) { return $count }
    $text = $utf8NoBom.GetString($acc.ToArray())
    if ($null -ne $rx) {
        foreach ($m in $rx.Matches($text)) {
            Write-Line ([string]($lineStart + $utf8NoBom.GetByteCount($text.Substring(0, $m.Index))))
            $count++
            if ($count -ge $limit) { return $count }
        }
        return $count
    }
    if ($pat.Length -eq 0) { return $count }
    $from = 0
    while ($from -le $text.Length - $pat.Length) {
        $j = $text.IndexOf($pat, $from, $cmp)
        if ($j -lt 0) { break }
        Write-Line ([string]($lineStart + $utf8NoBom.GetByteCount($text.Substring(0, $j))))
        $count++
        if ($count -ge $limit) { return $count }
        $from = $j + $pat.Length
    }
    return $count
}

# ---------------------------------------------------------------------
# lidx <first> <count>: byte offsets of the given lines + "T <total>".
# One pass over the file; byte offsets are counted at the LF boundary.
# ---------------------------------------------------------------------
function Cmd-LineIdx {
    param([string]$firstArg, [string]$countArg)
    try {
        $wire = Read-PathLine
        if (-not (Test-IsUInt $firstArg) -or -not (Test-IsUInt $countArg)) { Write-Err 'bad line range'; return }
        $first = [int64]$firstArg
        $count = [int64]$countArg
        if ($first -lt 1) { Write-Err 'bad line range'; return }
        $win = Convert-PosixToWin $wire
        if (-not (Test-Path -LiteralPath $win -PathType Leaf)) { Write-Err 'not a regular file'; return }
        $fs = [System.IO.File]::Open($win, [System.IO.FileMode]::Open,
                                     [System.IO.FileAccess]::Read,
                                     [System.IO.FileShare]::ReadWrite)
        try {
            # Byte-level walk for the same reason as grep: a StreamReader
            # eats the terminator without saying how many bytes it was, so a
            # CRLF file came out one byte short per line. awk on the POSIX
            # side counts the CR as part of the line, and so does this.
            $buf = New-Object 'byte[]' 65536
            $lineNo = 0L
            $lineStart = 0L
            $pos = 0L
            while ($true) {
                $got = $fs.Read($buf, 0, $buf.Length)
                if ($got -le 0) { break }
                for ($i = 0; $i -lt $got; $i++) {
                    if ($buf[$i] -ne 0x0A) { continue }
                    $lineNo++
                    if ($lineNo -ge $first -and $lineNo -lt ($first + $count)) {
                        Write-Line ([string]$lineStart)
                    }
                    $lineStart = $pos + $i + 1
                }
                $pos += $got
            }
            # A trailing line with no LF of its own is still a line.
            if ($lineStart -lt $pos) {
                $lineNo++
                if ($lineNo -ge $first -and $lineNo -lt ($first + $count)) {
                    Write-Line ([string]$lineStart)
                }
            }
            Write-Line ("T " + $lineNo)
        } finally { $fs.Dispose() }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

# ---------------------------------------------------------------------
# ffind <limit> <nmasks> <grep mode>: walk a whole tree, emit
# stat-shaped entries for hits, up to <limit>. If grep mode != '-',
# also read a pattern line and filter files whose content matches.
# ---------------------------------------------------------------------
function Cmd-FFind {
    param([string]$limArg, [string]$nmArg, [string]$gmode)
    try {
        $dirWire = Read-PathLine
        if (-not (Test-IsUInt $limArg) -or -not (Test-IsUInt $nmArg)) { Write-Err 'bad search request'; return }
        $limit = [int]$limArg
        $nm    = [int]$nmArg
        if ($nm -lt 1) { Write-Err 'bad search request'; return }
        $masks = New-Object 'string[]' $nm
        for ($i = 0; $i -lt $nm; $i++) { $masks[$i] = Read-PathLine }
        $pat = $null
        $ci  = $false
        $fixed = $false
        if ($gmode -ne '-') {
            if ($gmode -match '[^fie]') { Write-Err 'bad grep mode'; return }
            $pat = Read-PathLine
            if ([string]::IsNullOrEmpty($pat)) { Write-Err 'empty search pattern'; return }
            $fixed = $gmode.Contains('f')
            $ci    = $gmode.Contains('i')
        }
        $dir = Convert-PosixToWin $dirWire
        if (-not (Test-Path -LiteralPath $dir -PathType Container)) { Write-Err 'not a directory'; return }
        Emit-ModeLine
        $rx = $null
        if ($pat -ne $null -and -not $fixed) {
            $opts = [System.Text.RegularExpressions.RegexOptions]::CultureInvariant
            if ($ci) { $opts = $opts -bor [System.Text.RegularExpressions.RegexOptions]::IgnoreCase }
            $rx = New-Object System.Text.RegularExpressions.Regex($pat, $opts)
        }
        $enum = Get-ChildItem -LiteralPath $dir -Recurse -Force -File -ErrorAction SilentlyContinue
        $count = 0
        foreach ($fi in $enum) {
            $name = $fi.Name
            $ok = $false
            foreach ($m in $masks) {
                if ([System.IO.Path]::GetFileName($name) -like $m) { $ok = $true; break }
            }
            if (-not $ok) { continue }
            if ($pat -ne $null) {
                $hit = $false
                try {
                    if ($fixed) {
                        $cmp = if ($ci) { [System.StringComparison]::OrdinalIgnoreCase }
                               else     { [System.StringComparison]::Ordinal }
                        foreach ($ln in [System.IO.File]::ReadLines($fi.FullName, $utf8NoBom)) {
                            if ($ln.IndexOf($pat, $cmp) -ge 0) { $hit = $true; break }
                        }
                    } else {
                        foreach ($ln in [System.IO.File]::ReadLines($fi.FullName, $utf8NoBom)) {
                            if ($rx.IsMatch($ln)) { $hit = $true; break }
                        }
                    }
                } catch { $hit = $false }
                if (-not $hit) { continue }
            }
            # For a tree search the client wants the FULL wire path in
            # the name field of the entry.
            $wirePath = Convert-WinToPosix $fi.FullName
            try { Emit-StatEntry -fi $fi -nameOverride $wirePath } catch { }
            $count++
            if ($count -ge $limit) { break }
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

# ---------------------------------------------------------------------
# Background job runtime.
# Layout (mirrors helper.sh): job dir under %TEMP%\.f4jobs.<pid>.<token>
# with one subdir per job holding pid/kind/out/err/rc/n/kill files.
# We use Start-ThreadJob when available (PS 7+ or ThreadJob module),
# fall back to Start-Job (a full PS runspace per job) elsewhere.
# ---------------------------------------------------------------------
$script:F4JDir = $null
$script:F4JN   = 0
$script:F4Jobs = @{}
$script:F4HasThreadJob = $null

function Test-ThreadJobAvailable {
    if ($script:F4HasThreadJob -ne $null) { return $script:F4HasThreadJob }
    $script:F4HasThreadJob = $null -ne (Get-Command Start-ThreadJob -ErrorAction SilentlyContinue)
    return $script:F4HasThreadJob
}

function Ensure-JobDir {
    if ($null -ne $script:F4JDir) { return }
    $base = $env:TEMP
    if ([string]::IsNullOrEmpty($base)) { $base = [System.IO.Path]::GetTempPath() }
    $script:F4JDir = Join-Path $base (".f4jobs." + $PID + "." + $F4TOKEN)
    [void](New-Item -ItemType Directory -Path $script:F4JDir -Force -ErrorAction Stop)
}

function New-JobSlot {
    Ensure-JobDir
    $script:F4JN++
    $d = Join-Path $script:F4JDir $script:F4JN
    [void](New-Item -ItemType Directory -Path $d -Force -ErrorAction Stop)
    return @{ Id = $script:F4JN; Dir = $d }
}

# The scan job body: walks a tree, counts files/dirs and their bytes,
# emits "P" progress lines every 2000 entries and one "T" total at end.
function Cmd-JStart {
    param([string]$kind, [string]$nPathsArg)
    try {
        if (-not (Test-IsUInt $nPathsArg)) { Write-Err 'bad path count'; return }
        $n = [int]$nPathsArg
        if ($n -gt 4) { Write-Err 'bad path count'; return }
        $paths = New-Object 'string[]' $n
        for ($i = 0; $i -lt $n; $i++) { $paths[$i] = Read-PathLine }
        switch ($kind) {
            'scan' { if ($n -ne 1) { Write-Err 'this job takes one path'; return } }
            'hash' { if ($n -ne 1) { Write-Err 'this job takes one path'; return } }
            'exec' { if ($n -ne 2) { Write-Err 'the exec job takes a directory and a command'; return } }
            default { Write-Err 'unknown job kind'; return }
        }
        $slot = New-JobSlot
        $jd = $slot.Dir
        $outP = Join-Path $jd 'out'
        $errP = Join-Path $jd 'err'
        $rcP  = Join-Path $jd 'rc'
        $nP   = Join-Path $jd 'n'
        [System.IO.File]::WriteAllText((Join-Path $jd 'kind'), $kind + "`n", $utf8NoBom)
        [System.IO.File]::WriteAllText($nP, '0', $utf8NoBom)
        [System.IO.File]::WriteAllBytes($outP, [byte[]]@())
        # The body runs in its own runspace (Start-Job) or thread
        # (Start-ThreadJob). Neither one inherits the parent's function
        # table, so every helper the body needs is defined inside it.
        # The parent's Invoke-JobScan/Hash/Exec below are kept only as
        # references so a maintainer can read them independently; the
        # runtime copies live in $body.
        $body = {
            param($kind, $paths, $jd, $outP, $errP, $rcP)
            $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
            function Convert-PosixToWin([string]$p) {
                if ($p -eq '' -or $p -eq '/') { return '' }
                if ($p.StartsWith('//')) { return '\\' + $p.Substring(2).Replace('/', '\') }
                if (-not $p.StartsWith('/')) { return $p.Replace('/', '\') }
                $tail = $p.Substring(1); $s = $tail.IndexOf('/')
                if ($s -lt 0) { if ($tail.Length -eq 1) { return $tail.ToUpper() + ':\' } else { return $tail.Replace('/', '\') } }
                $d = $tail.Substring(0, $s); $r = $tail.Substring($s + 1)
                if ($d.Length -eq 1) { return $d.ToUpper() + ':\' + $r.Replace('/', '\') }
                return $p.Substring(1).Replace('/', '\')
            }
            function Convert-WinToPosix([string]$w) {
                if ([string]::IsNullOrEmpty($w)) { return '/' }
                if ($w.StartsWith('\\')) { return '//' + $w.Substring(2).Replace('\', '/').TrimEnd('/') }
                if ($w.Length -ge 2 -and $w[1] -eq ':') {
                    $dr = [char]::ToLower($w[0])
                    $rs = if ($w.Length -gt 2) { $w.Substring(2).TrimStart('\').Replace('\', '/') } else { '' }
                    if ($rs -eq '') { return '/' + $dr } else { return '/' + $dr + '/' + $rs.TrimEnd('/') }
                }
                return '/' + $w.Replace('\', '/').TrimStart('/').TrimEnd('/')
            }
            function Run-JobScan {
                param($rootWin, $outPath, $errPath, $rcPath, $enc)
                $rc = 0
                try {
                    if (-not (Test-Path -LiteralPath $rootWin -PathType Container)) { throw 'not a directory' }
                    $out = New-Object System.IO.StreamWriter($outPath, $false, $enc)
                    try {
                        $fbytes = 0L; $dbytes = 0L; $files = 0L; $dirs = 0L; $k = 0L
                        $lastPath = ''
                        foreach ($p in [System.IO.Directory]::EnumerateFileSystemEntries(
                                        $rootWin, '*', [System.IO.SearchOption]::AllDirectories)) {
                            $k++
                            try {
                                $attr = [System.IO.File]::GetAttributes($p)
                                if ($attr -band [System.IO.FileAttributes]::Directory) {
                                    $dirs++
                                } else {
                                    $files++
                                    $fbytes += (New-Object System.IO.FileInfo $p).Length
                                }
                            } catch { }
                            if (($k % 2000) -eq 0) {
                                $lastPath = Convert-WinToPosix $p
                                $out.WriteLine("P $fbytes $dbytes $files $dirs $lastPath")
                                $out.Flush()
                            }
                        }
                        $out.WriteLine("T $fbytes $dbytes $files $dirs")
                        $out.Flush()
                    } finally { $out.Dispose() }
                } catch {
                    [System.IO.File]::AppendAllText($errPath, $_.Exception.Message + "`n", $enc)
                    $rc = 1
                }
                [System.IO.File]::WriteAllText($rcPath, $rc.ToString(), $enc)
            }
            function Run-JobHash {
                param($rootWin, $jobDir, $outPath, $errPath, $rcPath, $enc)
                $rc = 0
                try {
                    if (-not (Test-Path -LiteralPath $rootWin -PathType Container)) { throw 'not a directory' }
                    $sizesPath = Join-Path $jobDir 'sizes'
                    $sw = New-Object System.IO.StreamWriter($sizesPath, $false, $enc)
                    try {
                        foreach ($p in [System.IO.Directory]::EnumerateFiles(
                                        $rootWin, '*', [System.IO.SearchOption]::AllDirectories)) {
                            try { $sw.WriteLine("$((New-Object System.IO.FileInfo $p).Length) $p") } catch { }
                        }
                    } finally { $sw.Dispose() }
                    $counts = @{}
                    foreach ($ln in [System.IO.File]::ReadLines($sizesPath, $enc)) {
                        $sp = $ln.IndexOf(' '); if ($sp -lt 0) { continue }
                        $k = $ln.Substring(0, $sp)
                        if ($counts.ContainsKey($k)) { $counts[$k]++ } else { $counts[$k] = 1 }
                    }
                    $candPath = Join-Path $jobDir 'cand'
                    $cw = New-Object System.IO.StreamWriter($candPath, $false, $enc)
                    $total = 0L
                    try {
                        foreach ($ln in [System.IO.File]::ReadLines($sizesPath, $enc)) {
                            $sp = $ln.IndexOf(' '); if ($sp -lt 0) { continue }
                            if ($counts[$ln.Substring(0, $sp)] -gt 1) {
                                $cw.WriteLine($ln.Substring($sp + 1)); $total++
                            }
                        }
                    } finally { $cw.Dispose() }
                    $out = New-Object System.IO.StreamWriter($outPath, $false, $enc)
                    try {
                        $sha = [System.Security.Cryptography.SHA256]::Create()
                        $n = 0L
                        foreach ($p in [System.IO.File]::ReadLines($candPath, $enc)) {
                            $n++
                            $hex = ''
                            try {
                                $fs = [System.IO.File]::Open($p, [System.IO.FileMode]::Open,
                                                             [System.IO.FileAccess]::Read,
                                                             [System.IO.FileShare]::ReadWrite)
                                try {
                                    $hex = [BitConverter]::ToString($sha.ComputeHash($fs)).Replace('-','').ToLower()
                                } finally { $fs.Dispose() }
                            } catch { }
                            if (-not [string]::IsNullOrEmpty($hex)) { $out.WriteLine("H $hex $p") }
                            $out.WriteLine("P $n $total $p"); $out.Flush()
                        }
                        $out.WriteLine("T $n")
                    } finally { $out.Dispose() }
                } catch {
                    [System.IO.File]::AppendAllText($errPath, $_.Exception.Message + "`n", $enc)
                    $rc = 1
                }
                [System.IO.File]::WriteAllText($rcPath, $rc.ToString(), $enc)
            }
            function Run-JobExec {
                param($dirWire, $cmdText, $outPath, $errPath, $rcPath, $enc)
                try {
                    $cwd = $null
                    if (-not [string]::IsNullOrEmpty($dirWire)) {
                        $cwd = Convert-PosixToWin $dirWire
                        if (-not (Test-Path -LiteralPath $cwd -PathType Container)) {
                            [System.IO.File]::AppendAllText($errPath, "no such directory`n", $enc)
                            [System.IO.File]::WriteAllText($rcPath, '1', $enc); return
                        }
                    }
                    if ([string]::IsNullOrEmpty($cmdText)) {
                        [System.IO.File]::AppendAllText($errPath, "empty command`n", $enc)
                        [System.IO.File]::WriteAllText($rcPath, '1', $enc); return
                    }
                    $psi = New-Object System.Diagnostics.ProcessStartInfo
                    $psi.FileName = 'cmd.exe'
                    $psi.Arguments = '/d /c ' + $cmdText
                    $psi.UseShellExecute = $false
                    $psi.RedirectStandardOutput = $true
                    $psi.RedirectStandardError  = $true
                    if ($null -ne $cwd) { $psi.WorkingDirectory = $cwd }
                    $p = [System.Diagnostics.Process]::Start($psi)
                    $out = New-Object System.IO.StreamWriter($outPath, $false, $enc)
                    try {
                        while (-not $p.StandardOutput.EndOfStream) { $out.WriteLine($p.StandardOutput.ReadLine()); $out.Flush() }
                        while (-not $p.StandardError.EndOfStream)  { $out.WriteLine($p.StandardError.ReadLine());  $out.Flush() }
                    } finally { $out.Dispose() }
                    $p.WaitForExit()
                    [System.IO.File]::WriteAllText($rcPath, $p.ExitCode.ToString(), $enc)
                } catch {
                    [System.IO.File]::AppendAllText($errPath, $_.Exception.Message + "`n", $enc)
                    [System.IO.File]::WriteAllText($rcPath, '1', $enc)
                }
            }
            try {
                switch ($kind) {
                    'scan' { Run-JobScan (Convert-PosixToWin $paths[0]) $outP $errP $rcP $utf8NoBom }
                    'hash' { Run-JobHash (Convert-PosixToWin $paths[0]) $jd $outP $errP $rcP $utf8NoBom }
                    'exec' { Run-JobExec $paths[0] $paths[1] $outP $errP $rcP $utf8NoBom }
                }
            } catch {
                [System.IO.File]::AppendAllText($errP, $_.Exception.Message + "`n", $utf8NoBom)
                [System.IO.File]::WriteAllText($rcP, '1', $utf8NoBom)
            }
        }
        if (Test-ThreadJobAvailable) {
            $j = Start-ThreadJob -ScriptBlock $body -ArgumentList $kind, $paths, $jd, $outP, $errP, $rcP
        } else {
            $j = Start-Job -ScriptBlock $body -ArgumentList $kind, $paths, $jd, $outP, $errP, $rcP
        }
        $script:F4Jobs[$slot.Id] = $j
        [System.IO.File]::WriteAllText((Join-Path $jd 'pid'), $j.Id.ToString(), $utf8NoBom)
        Write-Line ("J " + $slot.Id)
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Get-JobDir([string]$idArg) {
    if (-not (Test-IsUInt $idArg)) { return $null }
    if ($null -eq $script:F4JDir) { return $null }
    $d = Join-Path $script:F4JDir $idArg
    if (-not (Test-Path -LiteralPath $d -PathType Container)) { return $null }
    return $d
}

function Cmd-JPoll {
    param([string]$idArg, [string]$limArg)
    try {
        $jd = Get-JobDir $idArg
        if ($null -eq $jd) { Write-Err 'no such job'; return }
        if (-not (Test-IsUInt $limArg) -or [int]$limArg -lt 1) { Write-Err 'bad limit'; return }
        $limit = [int]$limArg
        $rcP = Join-Path $jd 'rc'
        $killP = Join-Path $jd 'kill'
        $errP = Join-Path $jd 'err'
        $outP = Join-Path $jd 'out'
        $nP = Join-Path $jd 'n'
        $state = 'run'; $rc = '-'; $msg = ''
        if (Test-Path -LiteralPath $killP) {
            $state = 'kill'
        } elseif (Test-Path -LiteralPath $rcP) {
            $state = 'done'
            $rcText = [System.IO.File]::ReadAllText($rcP, $utf8NoBom).Trim()
            if (Test-IsUInt $rcText) { $rc = $rcText } else { $rc = '-' }
            if ($rc -ne '0' -and (Test-Path -LiteralPath $errP)) {
                foreach ($ln in [System.IO.File]::ReadLines($errP, $utf8NoBom)) {
                    $msg = $ln; break
                }
            }
        }
        $sLine = "S $state $rc"
        if (-not [string]::IsNullOrEmpty($msg)) { $sLine = $sLine + ' ' + (Format-Flat $msg) }
        Write-Line $sLine

        # How many whole lines have been emitted so far, and how many we
        # already gave the caller (persisted in $nP).
        $tot = 0L
        if (Test-Path -LiteralPath $outP) {
            foreach ($ln in [System.IO.File]::ReadLines($outP, $utf8NoBom)) { $tot++ }
        }
        $done = 0L
        if (Test-Path -LiteralPath $nP) {
            $t = [System.IO.File]::ReadAllText($nP, $utf8NoBom).Trim()
            if (Test-IsUInt $t) { $done = [int64]$t }
        }
        $avail = $tot - $done
        if ($avail -gt 0) {
            if ($avail -gt $limit) { $avail = $limit }
            $skip = $done
            $sent = 0
            foreach ($ln in [System.IO.File]::ReadLines($outP, $utf8NoBom)) {
                if ($skip -gt 0) { $skip--; continue }
                Write-Line $ln
                $sent++
                if ($sent -ge $avail) { break }
            }
            [System.IO.File]::WriteAllText($nP, ($done + $avail).ToString(), $utf8NoBom)
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-JKill {
    param([string]$idArg)
    try {
        $jd = Get-JobDir $idArg
        if ($null -eq $jd) { Write-Err 'no such job'; return }
        $j = $script:F4Jobs[[int]$idArg]
        if ($null -ne $j) {
            try { Stop-Job -Job $j -ErrorAction SilentlyContinue } catch { }
        }
        [System.IO.File]::WriteAllText((Join-Path $jd 'kill'), '1', $utf8NoBom)
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-JDrop {
    param([string]$idArg)
    try {
        $jd = Get-JobDir $idArg
        if ($null -eq $jd) { Write-Ok; return }
        $j = $script:F4Jobs[[int]$idArg]
        if ($null -ne $j) {
            try { Stop-Job -Job $j -ErrorAction SilentlyContinue } catch { }
            try { Remove-Job -Job $j -Force -ErrorAction SilentlyContinue } catch { }
            $script:F4Jobs.Remove([int]$idArg)
        }
        try { Remove-Item -LiteralPath $jd -Recurse -Force -ErrorAction SilentlyContinue } catch { }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

function Cmd-JList {
    try {
        if ($null -ne $script:F4JDir -and (Test-Path -LiteralPath $script:F4JDir)) {
            foreach ($d in [System.IO.Directory]::EnumerateDirectories($script:F4JDir)) {
                $id = [System.IO.Path]::GetFileName($d)
                $state = 'run'
                if (Test-Path -LiteralPath (Join-Path $d 'kill')) { $state = 'kill' }
                elseif (Test-Path -LiteralPath (Join-Path $d 'rc')) { $state = 'done' }
                $kind = ''
                $kP = Join-Path $d 'kind'
                if (Test-Path -LiteralPath $kP) {
                    $kind = [System.IO.File]::ReadAllText($kP, $utf8NoBom).Trim()
                }
                Write-Line "$id $state $kind"
            }
        }
        Write-Ok
    } catch { Write-Err $_.Exception.Message }
}

# mode/rmode/wmode: only one backend per capability exists in this
# helper, so accept its exact name and reject the rest.
function Cmd-Mode {
    param([string]$name)
    if ($name -eq 'stat') { Write-Ok } else { Write-Err 'mode not available' }
}
function Cmd-RMode {
    param([string]$name)
    if ($name -eq 'filestream' -or $name -eq 'cat') { Write-Ok } else { Write-Err 'read mode not available' }
}
function Cmd-WMode {
    param([string]$name)
    if ($name -eq 'b64') { Write-Ok } else { Write-Err 'write mode not available' }
}

# ---------------------------------------------------------------------
# Cleanup + banner + main loop.
# ---------------------------------------------------------------------
function Invoke-JCleanup {
    if ($null -eq $script:F4JDir) { return }
    foreach ($j in $script:F4Jobs.Values) {
        try { Stop-Job -Job $j -ErrorAction SilentlyContinue } catch { }
        try { Remove-Job -Job $j -Force -ErrorAction SilentlyContinue } catch { }
    }
    try { Remove-Item -LiteralPath $script:F4JDir -Recurse -Force -ErrorAction SilentlyContinue } catch { }
    $script:F4JDir = $null
    $script:F4Jobs = @{}
}

# The features string mirrors what helper.sh advertises, with fake
# tokens for tools the client uses to gate features. Real work happens
# in .NET. See WINDOWS_PORT.md for the rationale of each tag.
# "hash:<tool>" is what gates the duplicate search on the client side
# (Features.HashTool, checked by CanHash); announcing sha256sum alone is
# not enough, exactly as in helper.sh where the two are separate tags.
$F4FEATS = 'flavor:pwsh base64 grep sed awk wc head tail truncate touch date sha256sum findbin jobs cp dd readlink du chown mode:stat hash:sha256sum read:filestream write:b64 headc headsafe tailc ddnotrunc statl ddbytes awkflush'

try {
    # A leading LF ensures the terminator starts a line even if the
    # remote shell's login noise did not end in one.
    Write-Line ''
    Write-End 'ok' ("FISHPLUS " + $F4PROTO + " " + $F4FEATS)

    while ($true) {
        $reqLine = Read-Line
        if ($null -eq $reqLine) { break }
        if ($reqLine -eq '') { continue }
        $parts = $reqLine.Split(' ', 5)
        if ($parts.Length -lt 2) { continue }
        if (-not (Test-IsUInt $parts[0])) { continue }
        $script:F4ID = [int64]$parts[0]
        $cmd = $parts[1]
        $a1 = if ($parts.Length -ge 3) { $parts[2] } else { '' }
        $a2 = if ($parts.Length -ge 4) { $parts[3] } else { '' }
        $a3 = if ($parts.Length -ge 5) { $parts[4] } else { '' }

        switch ($cmd) {
            'noop'   { Cmd-Noop }
            'pwd'    { Cmd-Pwd }
            'ping'   { Cmd-Ping }
            'enum'   { Cmd-Enum }
            'isdirs' { Cmd-IsDirs $a1 }
            'info'   { Cmd-Info $true }
            'linfo'  { Cmd-Info $false }
            'rdlink' { Cmd-RdLink }
            'mkdir'  { Cmd-Mkdir }
            'rm'     { Cmd-Rm }
            'rmdir'  { Cmd-Rmdir }
            'rmtree' { Cmd-Rmtree }
            'mv'     { Cmd-Mv }
            'cp'     { Cmd-Cp }
            'chmod'  { Cmd-Chmod $a1 }
            'chown'  { Cmd-Chown $a1 $a2 }
            'utime'  { Cmd-Utime $a1 $a2 }
            'trunc'  { Cmd-Trunc $a1 }
            'read'   { Cmd-Read $a1 $a2 }
            'write'  { Cmd-Write $a1 $a2 $a3 }
            'patch'  { Cmd-Patch $a1 $a2 }
            'grep'   { Cmd-Grep $a1 $a2 }
            'lidx'   { Cmd-LineIdx $a1 $a2 }
            'ffind'  { Cmd-FFind $a1 $a2 $a3 }
            'jstart' { Cmd-JStart $a1 $a2 }
            'jpoll'  { Cmd-JPoll $a1 $a2 }
            'jkill'  { Cmd-JKill $a1 }
            'jdrop'  { Cmd-JDrop $a1 }
            'jlist'  { Cmd-JList }
            'mode'   { Cmd-Mode $a1 }
            'rmode'  { Cmd-RMode $a1 }
            'wmode'  { Cmd-WMode $a1 }
            'feats'  { Write-Line ($F4PROTO.ToString() + ' ' + $F4FEATS); Write-Ok }
            'exit'   { Invoke-JCleanup; Write-Ok; break }
            default  { Write-Err 'unknown command' }
        }
        $F4Out.Flush()
    }
} finally {
    Invoke-JCleanup
    try { $F4Out.Flush() } catch { }
}
