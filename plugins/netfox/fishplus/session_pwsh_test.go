package fishplus

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHelperAgainstLocalPwsh runs the real PowerShell helper in a local
// pwsh process. It is the counterpart of TestHelperAgainstLocalShell for
// the Windows backend: the two together are what prove the two helper
// implementations serve the same wire protocol.
//
// The test is skipped when pwsh is not on PATH, and on Windows itself —
// Windows console pipes are ~4 KiB and the base64 bootstrap is ~50 KiB,
// which deadlocks the write-then-read pattern Handshake uses. On Linux
// and macOS pipe buffers are 64 KiB and the whole line fits. The manual
// Windows lane exercises the deadlock case with a spooled writer in the
// probe program.
//
// pwsh under Linux does not reproduce every ConsoleHost quirk of Windows
// PowerShell — the stdin race that motivated helper.ps1 defect fix
// d4b52ad, for instance, is Windows-only — but every request-formatting
// bug, every wire-format regression and every path-translation error
// this test does catch.
func TestHelperAgainstLocalPwsh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("small Windows console pipes deadlock the write-then-read handshake in test conditions; run the manual probe instead")
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not on PATH")
	}
	// -NoProfile keeps a user profile from printing before the banner;
	// -NoLogo keeps pwsh's own greeting away. -NonInteractive is NOT
	// passed on purpose: it disables the host reader helper.ps1 relies
	// on for its stdin, so a "non-interactive" test would run the wrong
	// code path.
	cmd := exec.Command(pwsh, "-NoProfile", "-NoLogo")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", pwsh, err)
	}
	sess := NewSession(stdin, stdout, stdin)
	t.Cleanup(func() {
		sess.Close()
		done := make(chan struct{})
		go func() {
			cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.HandshakeWithOptions(ctx, HandshakeOptions{Bootstrap: BootstrapBase64LinePwsh}); err != nil {
		t.Fatalf("pwsh handshake: %v (pwsh stderr: %s)", err, stderr.String())
	}
	feats := sess.Features()
	if feats.Proto != ProtocolVersion {
		t.Fatalf("Proto = %d, want %d", feats.Proto, ProtocolVersion)
	}
	// The three tags helper.ps1 must announce for the client's
	// capability gates to open: mode:stat (parseListing accepts it),
	// write:b64 (Client.Write encodes the payload as base64), and
	// flavor:pwsh (a hint for whoever cares who they are talking to).
	for _, want := range []string{"mode:stat", "write:b64", "flavor:pwsh"} {
		if !feats.Has(want) {
			t.Errorf("features do not include %q; raw = %q", want, feats.Raw)
		}
	}
	// hash:sha256sum is what Client.CanHash gates on — the tag was
	// added in commit b5f5b60 after the Windows lane found CanHash
	// false. Regressing to a bare "sha256sum" would silently disable
	// the duplicate search on every Windows peer.
	if feats.HashTool() != "sha256sum" {
		t.Errorf("HashTool = %q, want %q; raw = %q", feats.HashTool(), "sha256sum", feats.Raw)
	}

	if err := sess.Noop(ctx); err != nil {
		t.Fatalf("noop: %v (pwsh stderr: %s)", err, stderr.String())
	}

	// Ping payload covers the encoding path end to end: spaces, ASCII
	// quotes, PowerShell variable syntax, and non-ASCII bytes so a
	// hidden re-encoding through a code page other than UTF-8 shows up.
	const payload = "spaces and юникод and 'quotes' and $VARS"
	got, err := sess.Ping(ctx, payload)
	if err != nil {
		t.Fatalf("ping: %v (pwsh stderr: %s)", err, stderr.String())
	}
	if got != payload {
		t.Errorf("ping echoed %q, want %q", got, payload)
	}

	// The point of this call is *the pause*. Every regression of the
	// stdin-race bug the Windows lane found — a helper that reads from
	// stdin ahead of the request loop and loses whatever arrives after
	// the handshake — hangs here on Windows. The test still runs the
	// call on other systems so a change that always breaks it fails
	// everywhere, not only on Windows.
	time.Sleep(500 * time.Millisecond)
	got, err = sess.Ping(ctx, "after-pause")
	if err != nil {
		t.Fatalf("ping after pause: %v (pwsh stderr: %s)", err, stderr.String())
	}
	if got != "after-pause" {
		t.Errorf("ping after pause echoed %q, want %q", got, "after-pause")
	}

	// pwd from a helper that speaks POSIX-shaped wire paths is either
	// "/" (running from a drive root) or begins with a slash and a
	// single lowercase letter (Cygwin convention).
	resp, err := sess.Exec(ctx, "pwd")
	if err != nil {
		t.Fatalf("pwd: %v (pwsh stderr: %s)", err, stderr.String())
	}
	if err := resp.Err("pwd"); err != nil {
		t.Fatalf("pwd: %v", err)
	}
	if len(resp.Lines) != 1 {
		t.Fatalf("pwd payload = %q, want one line", resp.Lines)
	}
	if !strings.HasPrefix(resp.Lines[0], "/") {
		t.Errorf("pwd = %q, want a POSIX-shaped path", resp.Lines[0])
	}

	// An unknown command MUST not desynchronize the session — this is
	// the defect that costs a whole panel connection when it regresses.
	resp, err = sess.Exec(ctx, "frobnicate")
	if err != nil {
		t.Fatalf("unknown command must not break the session: %v", err)
	}
	if resp.OK() || !strings.Contains(resp.Msg, "unknown") {
		t.Errorf("unknown command: status = %q, msg = %q", resp.Status, resp.Msg)
	}
	if err := sess.Noop(ctx); err != nil {
		t.Errorf("noop after error: %v", err)
	}
}

// TestHelperAgainstLocalPwshEnumAndRead builds on the plain handshake
// test with the two calls a panel makes first: enum of a directory and
// read of a file. It exists here rather than as a subtest so a run that
// stops at the handshake still shows the second test as separately
// skipped rather than being silently swallowed.
func TestHelperAgainstLocalPwshEnumAndRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("see TestHelperAgainstLocalPwsh")
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh not on PATH")
	}
	dir := t.TempDir()
	// A file with known contents and a known length: the enum entry
	// carries the length, the read brings back the bytes.
	const body = "one two three\nfour five six\n"
	fp := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(fp, []byte(body), 0644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	cmd := exec.Command(pwsh, "-NoProfile", "-NoLogo")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", pwsh, err)
	}
	sess := NewSession(stdin, stdout, stdin)
	t.Cleanup(func() {
		sess.Close()
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sess.HandshakeWithOptions(ctx, HandshakeOptions{Bootstrap: BootstrapBase64LinePwsh}); err != nil {
		t.Fatalf("handshake: %v (pwsh stderr: %s)", err, stderr.String())
	}

	client := NewClient(sess)

	// Translate the tmpdir to the wire's POSIX shape. On Linux the
	// path already starts with a slash, so nothing changes; on macOS
	// the same holds; on Windows this test would not run at all.
	wireDir := "/" + strings.TrimPrefix(strings.TrimPrefix(dir, "/"), "\\")
	wireDir = strings.ReplaceAll(wireDir, "\\", "/")
	// Non-Windows path already correct as long as it starts with '/'.
	if !strings.HasPrefix(wireDir, "/") {
		t.Fatalf("cannot translate %q to a wire path", dir)
	}

	entries, err := client.Enum(ctx, wireDir)
	if err != nil {
		t.Fatalf("enum %q: %v (pwsh stderr: %s)", wireDir, err, stderr.String())
	}
	var seenSample bool
	for _, e := range entries {
		if e.Name != "sample.txt" {
			continue
		}
		seenSample = true
		if !e.IsRegular() {
			t.Errorf("sample.txt IsRegular = false, mode = %#o", e.Mode)
		}
		if e.Size != int64(len(body)) {
			t.Errorf("sample.txt Size = %d, want %d", e.Size, len(body))
		}
	}
	if !seenSample {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		t.Fatalf("sample.txt not found in %q; got %q", wireDir, names)
	}

	wireFile := wireDir + "/sample.txt"
	data, size, err := client.Read(ctx, wireFile, 0, int64(len(body)))
	if err != nil {
		t.Fatalf("read %q: %v (pwsh stderr: %s)", wireFile, err, stderr.String())
	}
	if size != int64(len(body)) {
		t.Errorf("read reported size %d, want %d", size, len(body))
	}
	if string(data) != body {
		t.Errorf("read %q returned %q, want %q", wireFile, string(data), body)
	}

	// The reverse direction: write a new file, then read it back and
	// compare. Covers the write:b64 payload path end to end.
	newFile := wireDir + "/written.txt"
	newBody := []byte("bytes via base64 write\n")
	if err := client.WriteFile(ctx, newFile, newBody); err != nil {
		t.Fatalf("write %q: %v (pwsh stderr: %s)", newFile, err, stderr.String())
	}
	back, err := client.ReadFile(ctx, newFile)
	if err != nil {
		t.Fatalf("read-back %q: %v", newFile, err)
	}
	if string(back) != string(newBody) {
		t.Errorf("read-back %q returned %q, want %q", newFile, string(back), string(newBody))
	}
}
