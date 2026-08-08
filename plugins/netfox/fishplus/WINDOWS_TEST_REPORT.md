# FISH+ Windows helper — manual test report

Result of running `WINDOWS_TEST_PLAN.md` against a real Windows host.
Eight defects were found and fixed; the wire is now clean for every
command the client can send. One item of the plan could not be run as
written, and the ssh lane is still blocked on one manual step — both are
spelled out below rather than glossed over.

Defects 1–7 are from the first pass over the draft. Defect 8 is from a
second pass on top of the owner's `6e8806b`/`6c60b09`, which landed while
this was being written; see *Follow-up round*.

## Environment

| | |
| --- | --- |
| Windows | 11 Home Single Language, build 10.0.22621 |
| Windows PowerShell | 5.1.22621.4249 (`powershell.exe`) |
| PowerShell 7 | 7.6.0 (`pwsh`) |
| Go | go1.26.0 windows/amd64 |
| `Start-ThreadJob` under PS 5.1 | **absent** — the `Start-Job` fallback is what ran |
| `Start-ThreadJob` under PS 7 | present (`Microsoft.PowerShell.ThreadJob`) |
| Console input code page | cp866 (matters — see defect 1) |
| OpenSSH Server | installed and running; `DefaultShell` set to `powershell.exe`; key not yet authorized (see *Known-still-broken*) |

Everything below was run under **both** shells. Both are green, so both
the `Start-Job` and the `Start-ThreadJob` job backends are covered.

## Summary of defects

| # | Defect | Commit |
| - | ------ | ------ |
| 1 | The helper could not read any request sent after it started | `d4b52ad` |
| 2 | `CanHash()` false on every Windows peer — no `hash:` tag | `b5f5b60` |
| 3 | `info`/`linfo` on `/` answered with a cast error | `09d7bea` |
| 4 | `grep` reported the line's offset instead of the match's, and was wrong on CRLF | `f795387` |
| 5 | `lidx` offsets were short by one byte per line on CRLF files | `9eb8afd` |
| 6 | the hash job emitted Windows paths the client cannot use | `f6d64fc` |
| 7 | polling a running job failed with a sharing violation | `c23e4a1` |
| 8 | the flavor fallback could not fire — the first handshake hung instead of failing | `1087a30` |

`go test -count=1 ./plugins/netfox/fishplus/...` passes after every one of
them, and passed before the first — the existing suite never exercised
`helper.ps1` against a live shell, which is why none of this showed up.

## § 1 Standalone lane

### 1.1 Syntax check — PASS

`[scriptblock]::Create()` over `helper.ps1` parses under PS 5.1 and PS 7.
The **compacted** helper — what actually goes over the wire, after
`Compact()` strips comments, blank lines and indentation — parses under
both as well. That is the form worth checking; the raw file parsing does
not prove the shipped one does.

### 1.2 Handshake — PASS

Fed the token-substituted helper into `powershell.exe -NoProfile
-NonInteractive -Command -` and captured stdout as raw bytes:

```
0a 2e 64 65 61 64 62 65 65 66 ...        <- LF, then ".deadbeefcafefeed 0 ok FISHPLUS 1 ..."
```

237 bytes, **no BOM, not one CR in the whole stream**, LF terminators
throughout. The banner is exactly what the plan expects.

### 1.3 One request — PASS, but not the way the plan describes

The plan pipes the helper and the requests into `powershell -Command -`.
That cannot work for more than one request: `-Command -` reads **all** of
stdin as the script text, so `1 pwd` and `2 exit` are parsed by PowerShell
rather than delivered to the helper, and it says so:

```
+ 1 pwd
+   ~~~
Unexpected token 'pwd' in expression or statement.
```

The helper meanwhile saw EOF and exited. Ran it through
`powershell -NoProfile -NonInteractive -File <helper>` instead, which
leaves stdin to the helper:

```
.deadbeefcafefeed 0 ok FISHPLUS 1 flavor:pwsh ...
/c/Users/sogonov/AppData/Local/Temp/claude/.../scratchpad
.deadbeefcafefeed 1 ok
.deadbeefcafefeed 2 ok
```

`pwd` is POSIX-shaped, so `Convert-WinToPosix` is intact.

Two things worth folding back into the plan: `-Command -` should be
replaced by `-File`, and `-File` is itself blocked by ExecutionPolicy on a
default machine (`Restricted` for CurrentUser here). It only worked at
first because the parent process had `Process=Bypass` and children inherit
it through `PSExecutionPolicyPreference`. A standalone run on a clean host
needs `-ExecutionPolicy Bypass -File`.

## § 2 Integrated lane

Driven by a small Go program built against this branch (the
`cmd/fishplus-probe` the plan says does not exist yet). It starts
`powershell.exe -NoProfile -NoLogo` with stdin/stdout as pipes — the shape
an ssh shell session has — and hands it `Base64BootstrapLinePwsh`, then
runs the real `fishplus.Client` against it. 82 checks, all passing under
both shells. It lives outside the repository; see *Suggested next steps*.

**What this lane does not cover: a real ssh channel.** `DefaultShell` has
since been set to `powershell.exe`, but authorizing a key is still
outstanding (see *Known-still-broken*). The local pipe
reproduces the console host's stdin behaviour exactly — that is where
defect 1 was found and fixed — but it does not prove out sshd's own
plumbing. See *Known-still-broken* for why an ssh run would fail today for
a reason that has nothing to do with `helper.ps1`.

### 2.1 Handshake — PASS

```
feats: flavor:pwsh base64 grep sed awk wc head tail truncate touch date
       sha256sum findbin jobs cp dd readlink du chown mode:stat
       hash:sha256sum read:filestream write:b64 headc headsafe tailc
       ddnotrunc statl ddbytes awkflush
```

`ListingMode()` → `stat`, `WriteMode()` → `b64`, `ReadMode()` →
`filestream`, `flavor:pwsh` present. `CanGrep`, `CanFind`, `CanRunJobs`,
`CanScan`, `CanHash` all true; `Pwd()` returns `/c/...`.

**Defect 1 — the helper could not read a request that arrived after it
started.** This blocked the whole lane, and it is the one that matters.

A PowerShell console host with a redirected stdin keeps a read pending on
that handle for its own line reader, and it wins every race against a
script running inside it. The helper read stdin directly, so it got
whatever had already arrived when it started and nothing else: the
handshake succeeded, the first request sent after any pause vanished into
the host, and the helper blocked forever. Reduced to a five-line script,
the behaviour is unambiguous — `first` (sent before startup) arrives,
`second` (sent three seconds later) never does. It is not specific to the
interactive host: `-File` and `-Command` behave the same way.

Fixed by asking the host for the line (`$host.UI.ReadLine()`) and undoing
the decoding it applies — it decodes stdin with the console code page
(cp866 here), so encoding the string back with that same encoding recovers
the bytes that were actually sent. Verified byte-exact for UTF-8:
`привет.txt` comes back as `d0bf d180 d0b8 d0b2 d0b5 d182 2e747874`. Two
consequences had to be handled with it:

- reading through the host **echoes every line into stdout**, which would
  put a copy of each request in front of its answer. The host's writer is
  muted; nothing in the helper prints through it. This also disposes of
  the prompt.
- `[Console]::OpenStandardInput()` costs bytes *just by being called* — it
  buffers what has arrived into a stream neither reader looks at again.
  Opening stdin is now deferred to first use.

Checked at the size the protocol actually uses: an 87 KiB base64 line
(the shape a 64 KiB `write:b64` chunk takes) survives the host reader
bit-for-bit, SHA-256 verified. CR is stripped by the host, EOF arrives as
`$null`.

The raw-stream path stays for a host that refuses to read for us
(`-NonInteractive`), which is the standalone case where nothing competes
for stdin. Byte-exact raw payloads are unreachable through the host reader
and are now refused with a clear message rather than half-read; the client
never asks for one, since the banner announces `write:b64` and `wmode`
accepts nothing else.

**Defect 2 — `CanHash()` was false on every Windows peer.** The client
gates its duplicate search on `Features.HashTool()`, which reads the
`hash:<tool>` tag. The helper announced a bare `sha256sum`, which says a
tool exists but not which one. `helper.sh` announces both tags separately
for exactly this reason.

### 2.2 Panel-shaped operations — PASS

`enum /` lists `[c d]` as directories; `enum /c` works; the lab directory
comes back with the right entry count. Verified against the local
filesystem: names survive verbatim (Cyrillic and spaces both), sizes match
`os.Stat`, mtimes match to the second, `IsDir()`/`IsRegular()` classify
correctly, and `isdirs` answers correctly for a directory, a file and `/`.

**Defect 3 — `stat /` failed.** PowerShell's format operator binds tighter
than `-bor`, so `"{0:x} ..." -f $F4MODE_DIR -bor $F4PERM_DEF` formatted the
type bits alone and then tried to or the resulting *string* with an
integer. The virtual root answered with the cast error instead of an
entry.

### 2.3 Read / write — PASS

Small file read back byte-identical; write → read → compare against the
on-disk file identical; `Truncate` to 5 verified through `Stat`; `Remove`
verified. A 3 MiB file exercised the chunking in both directions — read in
256 KiB chunks and written in 64 KiB base64 chunks — byte-identical both
ways. No defects.

### 2.4 Search + jobs — PASS after four fixes

`grep` offsets verified against the same search run over a local copy of
the file, for an LF file, a **CRLF** file and a UTF-8 Cyrillic file.
`lidx` offsets and totals verified the same way. `find` by mask returns
the right count with full wire paths; `find` with a content filter finds
the one file. `scan` totals match a local walk exactly (files, dirs,
bytes). `hash` hashes exactly the files sharing a size, and the SHA-256
values match those computed locally.

**Defect 4 — `grep` reported the wrong offsets.** The POSIX side runs
`grep -a -b -o`, which prints the byte offset of *every match* and one
line per match. The helper reported the start of the line instead, once
per matching line however many times it matched, so a viewer sent to one
of those offsets landed in the wrong place. It also went through a
`StreamReader`, which drops the terminator without saying how long it was:
on a CRLF file — the ordinary case on Windows — every offset past the
first line was short by one byte per line. Now walks the file as bytes.

**Defect 5 — `lidx` had the same CRLF error.** Offsets were built by
adding one byte for the terminator regardless, so seeking to line N landed
inside line N−1 on a CRLF file. `awk` counts the CR as part of the line,
and so does the fix.

**Defect 6 — the hash job emitted Windows paths.** It walked the tree with
.NET and wrote what it found straight out, so every `H` and `P` line
carried `C:\dir\file`. The client feeds those back into its own path
handling, where such a string is neither absolute nor joinable, so a
duplicate search produced entries nothing could open. The scan job already
translated its progress path.

**Defect 7 — polling a running job failed outright.**
`[IO.File]::ReadLines`/`ReadAllText` open with `FileShare.Read`, which
refuses a file another handle is writing to; the job holds its output file
open for its whole run. Every poll of a running job failed with *"the
process cannot access the file"* instead of returning what had been
written, so a scan never reported anything and ended as an error. Reads
now permit a concurrent writer, only whole lines are counted (the count is
the number of LFs, so a line still being written stays invisible), and the
cursor advances by what was really sent rather than by what was intended.

### 2.5 Odd paths — PASS

A name with a space, a Cyrillic name and a name with a leading `~` all
stat and read correctly end to end. `EncodePathLine` escapes a leading
tilde and `Read-PathLine` decodes it. Nothing triggered a spurious base64
escape.

### 2.6 Everything else the client can send — PASS (added)

Not in the plan, but a panel uses these constantly and none of them were
covered: `mkdir`, `mv`, `cp`, `utime`, `rm`, `rmdir`, `rmtree`, `patch`
and remote `exec`. All verified against the local filesystem — including
`Chtimes` setting both mtime and atime to the second, `patch` assembling
a file out of two copied ranges and one literal, and `RunOutput` capturing
a command's output and exit code. `jlist` answers.

### 2.7 Error paths (added)

The failure modes matter more than the wording: a helper that answers a
bad request by going quiet, or by leaving a reply unterminated,
desynchronizes the session and costs the panel its connection. Fourteen
deliberate failures — stat/read/grep/lidx of a missing file, enum of a
file, read and truncate of a directory, `rdlink` of a plain file, `rmdir`
of a non-empty directory, a write through a `..` path, a write to a
directory, find in a missing directory, a poll of a job that does not
exist — each came back as a clean `RemoteError`, and a `noop` after **each
one** confirmed the stream was still where the next request expects it. A
final `ping` with a Cyrillic path round-tripped exactly.

## § 3 Declared limitations — behave as declared

- `chown` refuses with *"chown is not supported on this Windows host"*,
  as a remote error; the session stays usable.
- `chmod 0444` sets the ReadOnly attribute, `chmod 0644` clears it —
  the projection the design describes, verified through `os.Stat`.
- Symlinks were **not** tested: creating one needs a privilege this
  account does not have by default. `rdlink` was only checked for its
  refusal on a plain file.

## Follow-up round: the flavor fallback

`6e8806b` (netfox: fall back to a PowerShell shell request when POSIX
fails) closes the transport gap this report originally listed as
still-broken, and `6c60b09` adds the pwsh-gated test. Retested on top of
both; `helper.ps1` needed no further changes.

**Defect 8 — the fallback could not fire, because the first attempt never
failed: it hung.** `establishWithFallback` switches flavors only when the
primary handshake returns an error. A PowerShell peer handed the POSIX
bootstrap prints its parse error on **stderr** — which the ssh transport
sends to `io.Discard` — and then waits for input forever. On stdout it
produces two prompt lines and nothing more, so `waitForReady` blocked on a
read that would never return.

Measured against a local `powershell.exe`: with a 15 s context deadline the
handshake was **still blocked after 20 s**. The deadline is only checked
between lines, and no further line arrives — so the ctx never rescues it.
`waitForReady` now bounds the silence between lines (`ReadyTimeout`, 20 s),
which makes the POSIX attempt end with *"the remote shell never reported
being ready within 20s"* — a message `isHandshakeFailure` already matches,
so the fallback promotes the PowerShell bootstrap as intended. Fixed in
`1087a30`; bounding silence rather than the total wait keeps a slow motd
working.

Note on the new test: `TestHelperAgainstLocalPwsh` skips on Windows because
the ~4 KiB console pipe deadlocks the write-then-read handshake. That is
avoidable — the probe spools its writes into a goroutine, about fifteen
lines — which would let the test cover the one platform whose ConsoleHost
quirks it is really about. Left alone as the owner's call.

## Known-still-broken

**The ssh lane still has not run.** `DefaultShell` is now set to
`powershell.exe`, but key authentication is not in place: this account is
in the administrators group, so sshd reads
`__PROGRAMDATA__/ssh/administrators_authorized_keys` (per the `Match Group
administrators` block in `sshd_config`), and that file does not exist —
`ssh -o BatchMode=yes localhost` answers *Permission denied
(publickey,password,keyboard-interactive)*. Creating it is the one step
still outstanding.

The remaining step (the sandbox refuses this edit, so it has to be run by
hand):

```powershell
$pub = Get-Content "$env:USERPROFILE\.ssh\id_ed25519.pub"
Add-Content 'C:\ProgramData\ssh\administrators_authorized_keys' $pub
icacls 'C:\ProgramData\ssh\administrators_authorized_keys' /inheritance:r `
    /grant 'Administrators:F' /grant 'SYSTEM:F'
```

Two things need checking there that the local pipe cannot answer: whether
sshd allocates a ConPTY for the session (a real console would bring back
echo and VT sequences the mute does not cover), and what sshd actually
passes to PowerShell for an `exec` request.

**A 50 KiB bootstrap line can deadlock a small pipe.** The console host
echoes the whole line back before the helper mutes stdout, and a 4 KiB
Windows pipe fills before the client has written it all; the probe spools
its writes to get past this. An ssh channel's window is large enough that
it should not appear there, but a transport that writes the bootstrap
synchronously and only then starts reading is relying on that.

**`Get-RootEntries` has a dead branch.** The `if (-not $d.IsReady)` body
contains only a comment, so an unready drive is listed like any other.
That is what the comment says should happen — the branch just does not do
anything. Harmless, left alone.

## Deviations from the design

`WINDOWS_PORT.md` is right about everything it decided; two of its
statements about the mechanism are not.

1. **"stdin must deliver raw bytes for binary payloads"** with
   `[Console]::OpenStandardInput()` — that is exactly what does not work
   under a console host, for the reason in defect 1. The section should
   say that the host owns stdin, that lines come from `$host.UI.ReadLine()`
   and must be re-encoded with the host's input code page, and that its
   echo has to be muted. Raw byte payloads are unavailable, which costs
   nothing because the design already chose `write:b64` for other reasons.
2. **The feats string in the document omits `hash:`**, so the helper was
   built to match it. It needs `hash:sha256sum` next to `mode:stat`.

The path convention, the `mode:stat` choice, `write:b64`, and the mock
feature list all held up under test and were not touched.

## Suggested next steps

1. **Let the pwsh-gated test run on Windows.** `6c60b09` already covers
   the handshake, the pause and enum/read; it skips on Windows only
   because of the pipe deadlock, which a spooled writer removes in about
   fifteen lines. Windows is the platform whose ConsoleHost quirks
   produced defects 1 and 8, so it is the one worth covering. The probe
   also carries checks the test does not: grep/lidx offsets against a
   CRLF file, scan/hash totals, the mutations, and the error paths —
   defects 4–7 live there.
2. **Run the ssh lane** once the key is authorized, specifically looking
   for a ConPTY and for what sshd passes to PowerShell on an `exec`
   request.
3. **Consider compressing the helper.** At 38 KiB compacted the bootstrap
   line is 50 KiB, which is over the 32 KiB Windows command-line limit —
   so a transport that ever wants to pass it as an argument rather than on
   stdin cannot. gzip+base64 would bring it to roughly 10 KiB.
4. **Re-check `Test-SafeTarget` against a UNC path.** It accepts
   `\\server\share`, but nothing in this lane exercised one.
