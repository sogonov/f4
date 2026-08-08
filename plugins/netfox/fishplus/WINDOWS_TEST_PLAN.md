# FISH+ Windows helper — manual test plan

Sanity checks to run against a real Windows peer with OpenSSH-Server
enabled. Two lanes: **standalone** (drive helper.ps1 by hand, verify
the wire) and **integrated** (drive it from f4 via
`BootstrapBase64LinePwsh`, verify the panel works).

Nothing here is automated — automation belongs in a `pwsh`-gated Go
test, which will follow once these pass by hand.

## 0. Prerequisites on the Windows peer

- Windows 10/11 or Server 2019+.
- **OpenSSH Server** installed and running (`Settings > Optional
  Features > OpenSSH Server` on Win10/11; `Add-WindowsCapability` on
  Server).
- Default shell for SSH set to PowerShell (the installer sets `cmd.exe`;
  switch it to `powershell.exe`):
  ```powershell
  New-ItemProperty -Path 'HKLM:\SOFTWARE\OpenSSH' -Name DefaultShell `
      -Value 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe' `
      -PropertyType String -Force
  ```
- PowerShell 5.1 (default on Win10+) is fine; PS 7+ is preferred (has
  `Start-ThreadJob` for cheaper jobs).
- Verify SSH login works: from the Linux side,
  `ssh user@windows-host` should drop you in a `PS>` prompt.
- Copy `helper.ps1` to the Windows peer (any path;
  `C:\Users\<you>\helper.ps1` is fine).

## 1. Standalone lane — drive the helper by hand

The point of this lane: catch any parse error, encoding trouble or
prompt leak *before* the Go client is involved.

### 1.1 Syntax check

On the Windows peer:
```powershell
powershell.exe -NoProfile -NonInteractive -Command `
    "Get-Content C:\Users\you\helper.ps1 | Out-Null; `
     [scriptblock]::Create((Get-Content C:\Users\you\helper.ps1 -Raw)) | Out-Null; `
     'ok'"
```
Expect: prints `ok`. Any red text means a syntax problem — post the
whole message; the fix is a helper.ps1 edit.

### 1.2 Handshake

Still on Windows:
```powershell
$env:F4TOKEN='deadbeefcafefeed'
(Get-Content C:\Users\you\helper.ps1 -Raw).Replace('__F4_TOKEN__', $env:F4TOKEN) |
    powershell.exe -NoProfile -NonInteractive -Command -
```
Expect on stdout: a blank line, then
`.deadbeefcafefeed 0 ok FISHPLUS 1 flavor:pwsh …`.

If the banner is missing: PSReadLine leaked (rerun with
`-NonInteractive`), or a profile is echoing a prompt.

### 1.3 One request

Feed a hand-crafted request through the same pipe as above; add
`"1 pwd"; "2 exit"` to the input.
Expect on stdout:
```
(blank)
.deadbeefcafefeed 0 ok FISHPLUS 1 …
/c/Users/you
.deadbeefcafefeed 1 ok
.deadbeefcafefeed 2 ok
```
Path in `pwd` should be POSIX-shaped (`/c/…`, not `C:\…`). If it is
Windows-shaped, `Convert-WinToPosix` regressed.

## 2. Integrated lane — drive the helper from f4

Requires a small Go program you build once (say, `cmd/fishplus-probe/`,
not included yet). The pattern:

```go
sess := fishplus.NewSession(sshStdin, sshStdout, sshCloser)
if err := sess.HandshakeWithOptions(ctx, fishplus.HandshakeOptions{
    Bootstrap: fishplus.BootstrapBase64LinePwsh,
}); err != nil { log.Fatal(err) }
client := fishplus.NewClient(sess)

// Then whatever probing you want:
fmt.Println(client.Pwd(ctx))
fmt.Println(client.Enum(ctx, "/"))
fmt.Println(client.Enum(ctx, "/c"))
```

### 2.1 Handshake over SSH

Point the above at your Windows peer over `crypto/ssh`. Expect: no
error, `sess.Features().Flavor()` (once we add it) returns `pwsh`,
`ListingMode()` returns `stat`, `WriteMode()` returns `b64`.

### 2.2 Panel-shaped operations

Run against `/` first (virtual root — should list drive letters as
directories), then `/c`, then a directory you know well. What to
watch:

- Filenames come back verbatim (UTF-8 through the wire).
- Timestamps look sane (mtime/atime/ctime as epoch seconds, matching
  what Explorer shows).
- `IsDir()` and `IsRegular()` classify correctly.

### 2.3 Read / write

Pick a small text file. `Read` it, `Write` a new one, `Read` back to
verify byte identity, `Truncate`, `Remove`.

Then a larger file (a few MB) — verify chunking (client asks in
256 KiB chunks by default) and that the total decoded matches the
source.

### 2.4 Search + jobs

- `Grep` for a substring in a text file — verify byte offsets against
  a local `grep -b` on the same file transferred from the peer.
- `Find` on a small tree with a mask.
- `Scan` — verify the totals match what Explorer reports.
- `Hash` — expect only files sharing a size with another to hash;
  verify the SHA-256 values against `Get-FileHash`.

### 2.5 Odd paths

- A path with a space in it — must NOT trigger base64 escape (the
  wire allows spaces in path lines; the escape is for LF/CR/tab/tilde
  only).
- A path with Cyrillic in the name — must survive round trip
  end-to-end (UTF-8 all the way).
- A path with a leading `~` — must be base64-escaped by the client
  (`EncodePathLine` in session.go) and correctly decoded by
  `Read-PathLine` in helper.ps1.

## 3. What we already know won't work in this pass

- `chown` — returns `err "chown is not supported on this Windows host"`.
  The client offers the menu (because we announce the feature), the
  helper refuses the operation with a clear message.
- `chmod` — reduces to setting/clearing the `ReadOnly` attribute
  based on whether *any* write bit is set in the requested mode.
  All other POSIX bits (setuid/setgid/sticky/exec) are silently
  discarded. Not lossless.
- Symlinks — read only. Creating one from the client isn't a wire op
  the protocol has.
- `cmd.exe`-only peers — no helper, refuse the connection.

## 4. Reporting a failure

When something breaks, please include:
- The exact request line the client sent (turn on protocol debug in
  the client if the plumbing supports it, otherwise print each
  `ExecPath/ExecPayload` arg).
- The full reply lines up to and including the `.<token> <id> ...`
  terminator, if any.
- `$PSVersionTable | Out-String` from the Windows peer.
- Whether `Get-Module PSReadLine` returns anything after login (should
  be empty after our `Remove-Module` at bootstrap).

The wire is small enough that a full protocol trace of one failed
command usually tells us the whole story.
