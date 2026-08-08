package fishplus

import (
	"encoding/base64"
	"strings"
	"testing"
)

// The PowerShell bootstrap has to fit in a single line, use only
// printable ASCII, and produce the compacted helper when decoded. These
// three invariants are what makes the line survive being pasted into a
// remote shell that mangles anything else.

func TestBase64BootstrapLinePwshIsOneLine(t *testing.T) {
	token := "abcdef0123456789"
	line := Base64BootstrapLinePwsh(token)
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("pwsh bootstrap does not end in a newline: %q", line[max(0, len(line)-20):])
	}
	body := strings.TrimSuffix(line, "\n")
	if strings.ContainsAny(body, "\r\n") {
		t.Fatalf("pwsh bootstrap body contains an embedded newline")
	}
}

func TestBase64BootstrapLinePwshIsPrintableAscii(t *testing.T) {
	token := "0011223344556677"
	line := Base64BootstrapLinePwsh(token)
	body := strings.TrimSuffix(line, "\n")
	for i, r := range body {
		if r < 0x20 || r > 0x7e {
			t.Fatalf("pwsh bootstrap has a non-printable byte %#x at offset %d", r, i)
		}
	}
}

func TestBase64BootstrapLinePwshCarriesHelperScript(t *testing.T) {
	token := "deadbeefcafefeed"
	line := Base64BootstrapLinePwsh(token)
	// Find the '<b64>' literal between the two single quotes assigning
	// $F4B: base64 alphabet is [A-Za-z0-9+/=], never contains a quote.
	const prefix = "$F4B='"
	start := strings.Index(line, prefix)
	if start < 0 {
		t.Fatalf("pwsh bootstrap does not assign $F4B: %q", line[:min(80, len(line))])
	}
	rest := line[start+len(prefix):]
	end := strings.Index(rest, "'")
	if end < 0 {
		t.Fatalf("pwsh bootstrap $F4B assignment is not terminated")
	}
	encoded := rest[:end]
	// The compact helper sometimes contains the raw ready marker; the
	// bootstrap splits it into F''4RDY<token> so a terminal echo cannot
	// look like the marker itself. Undo the split before decoding.
	restored := strings.ReplaceAll(encoded, "F''4RDY"+token, "F4RDY"+token)
	decoded, err := base64.StdEncoding.DecodeString(restored)
	if err != nil {
		t.Fatalf("pwsh bootstrap payload is not valid base64: %v", err)
	}
	got := string(decoded)
	// The payload begins with the sentinel comment then the helper.
	if !strings.HasPrefix(got, "# F4B64"+token+"\n") {
		t.Fatalf("pwsh bootstrap payload has no sentinel prefix; got head %q", got[:min(80, len(got))])
	}
	// And the tail is exactly the compacted helper the same call produces.
	want := "# F4B64" + token + "\n" + HelperScriptPwsh(token)
	if got != want {
		t.Fatalf("pwsh bootstrap payload does not match HelperScriptPwsh(token)")
	}
}

func TestHelperScriptPwshSubstitutesToken(t *testing.T) {
	token := "0123456789abcdef"
	s := HelperScriptPwsh(token)
	if strings.Contains(s, tokenPlaceholder) {
		t.Fatalf("helper.ps1 still contains the token placeholder")
	}
	if !strings.Contains(s, token) {
		t.Fatalf("helper.ps1 does not contain the substituted token")
	}
}

func TestHelperSourcePwshIsNotEmpty(t *testing.T) {
	if len(HelperSourcePwsh()) < 1000 {
		t.Fatalf("helper.ps1 embed looks truncated: %d bytes", len(HelperSourcePwsh()))
	}
}

// max/min are 1.21+ builtins; f4 targets that already.
