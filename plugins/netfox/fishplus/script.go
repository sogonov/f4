package fishplus

import (
	_ "embed"
	"encoding/base64"
	"strings"
)

// ProtocolVersion is the FISH+ wire protocol revision implemented by both
// this package and the embedded helper script. The remote side reports its
// own version in the handshake banner; a mismatch is fatal for the session.
const ProtocolVersion = 1

// tokenPlaceholder is replaced by a per-session random token before the
// helper is sent to the remote shell.
const tokenPlaceholder = "__F4_TOKEN__"

//go:embed helper.sh
var helperSource string

//go:embed helper.ps1
var helperSourcePwsh string

// HelperSource returns the unmodified helper script. Useful for tests and
// for dumping the script when debugging a remote host.
func HelperSource() string { return helperSource }

// HelperSourcePwsh returns the unmodified PowerShell helper. Windows peers
// speak the same wire protocol as POSIX ones; the flavor detection picks
// which script is delivered.
func HelperSourcePwsh() string { return helperSourcePwsh }

// Compact strips comments and blank lines from a shell script and removes
// leading indentation. The helper is sent over the wire on every connect,
// so shaving it down is worth the few lines of code. The helper script must
// therefore not rely on here-documents or multi-line string literals.
func Compact(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	for _, line := range strings.Split(src, "\n") {
		// Embedded files use the working tree's line endings. On Windows a
		// CRLF helper would otherwise send the carriage return to the remote
		// shell as part of every command (for example "in\r" and "}\r").
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		b.WriteString(trimmed)
		b.WriteByte('\n')
	}
	return b.String()
}

// HelperEndMarker is the line that follows the helper script on the wire.
// It cannot occur inside the script, which is ours to write.
const HelperEndMarker = "F4EOF"

// ReadyMarker is what the bootstrap prints once the remote shell is done
// parsing it and has started executing. The token is part of it so that
// login noise cannot be mistaken for it.
func ReadyMarker(token string) string { return "F4RDY" + token }

// BootstrapLine is the single line that has to reach the remote shell
// before the helper does.
//
// A shell reads its script from the same stream the requests arrive on, and
// it does not read it a byte at a time: dash fills a buffer, and whatever
// lands in that buffer past the end of the script is parsed as part of it.
// A request that arrives while the shell is still parsing therefore gets
// executed as a shell command — "1: not found" — and the session hangs
// waiting for an answer that will never come. bash happens to read byte by
// byte and never showed it, which is why this survived so long.
//
// So the script is not parsed off the stream at all. This one line is, and
// then it takes the script in through the shell's own read builtin, which
// reads from the file descriptor and cannot run ahead of itself. The marker
// tells the client when the line has been parsed and is running, so that
// nothing is in flight while the parser is still working.
//
// The marker is printed as two pieces on purpose: a terminal that echoes
// the line back would otherwise send the client its own request to match.
func BootstrapLine(token string) string {
	return "echo F4R\"DY\"" + token +
		"; F4NL=$(printf '\\nx'); F4NL=${F4NL%x}; F4S=; " +
		"while IFS= read -r F4L; do [ \"$F4L\" = " + HelperEndMarker + " ] && break; " +
		"F4S=$F4S$F4L$F4NL; done; eval \"$F4S\"\n"
}

// Base64BootstrapLine returns a self-contained bootstrap command. Unlike
// BootstrapLine, it carries the complete helper in the same shell line, so a
// high-latency shell does not have to assemble the script one read at a time.
// Only printable ASCII plus the terminating newline goes over the wire.
//
// The decoded payload starts with a per-session sentinel. Merely exiting with
// status zero is not enough to accept a decoder: some base64 implementations
// silently ignore an option they do not understand. The sentinel proves that
// the whole pipeline produced the expected bytes before eval is allowed.
//
// The decoder spellings cover toybox/BusyBox/GNU (including Android API 24),
// BSD/macOS, and OpenSSL. No temporary file is created. If none works, the
// command emits a properly framed handshake error instead of leaving the
// client waiting for a banner that will never arrive.
func Base64BootstrapLine(token string) string {
	prefix := ": F4B64" + token + ";"
	payload := prefix + "\n" + HelperScript(token)
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	// The encoded bytes are opaque, but the readiness marker must be absent
	// from the command source by construction rather than probability. Split
	// a coincidental occurrence into adjacent shell literals; their value is
	// identical while a terminal echo contains two quote bytes in the middle.
	encoded = splitReadyMarker(encoded, token)
	quotedToken := shellSingleQuote(token)
	quotedPrefix := shellSingleQuote(prefix)

	// The readiness marker is split in the source so a terminal echo of this
	// command cannot be mistaken for the marker printed by the running shell.
	return "F4B='" + encoded + "'; " +
		"printf '%s%s\\n' F4R\"DY\" " + quotedToken + "; " +
		"F4S=; for F4D in 'base64 -d' 'base64 -D' 'base64 --decode' 'openssl base64 -A -d'; do " +
		"F4S=$(printf '%s\\n' \"$F4B\" | $F4D 2>/dev/null) || F4S=; " +
		"case \"$F4S\" in " + quotedPrefix + "*) break;; *) F4S=;; esac; done; " +
		"unset F4B F4D; if [ -n \"$F4S\" ]; then eval \"$F4S\"; " +
		"else printf '\\n.%s 0 err bootstrap base64 decoder unavailable\\n' " + quotedToken + "; fi\n"
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func splitReadyMarker(s, token string) string {
	return strings.ReplaceAll(s, ReadyMarker(token), "F''4RDY"+token)
}

// HelperScript returns the compacted helper script with the session token
// substituted, ready to be written into the remote shell's stdin.
func HelperScript(token string) string {
	return Compact(strings.ReplaceAll(helperSource, tokenPlaceholder, token))
}

// HelperScriptPwsh returns the compacted PowerShell helper script with the
// session token substituted. Compaction rules match the POSIX helper: strip
// CRs, comment lines and blank lines, keep everything else. The PowerShell
// helper is written without here-strings or backtick continuations so this
// simple stripping never breaks a statement.
func HelperScriptPwsh(token string) string {
	return Compact(strings.ReplaceAll(helperSourcePwsh, tokenPlaceholder, token))
}

// Base64BootstrapLinePwsh is the PowerShell analogue of Base64BootstrapLine.
// It fits the compacted helper into one printable-ASCII line that the remote
// PowerShell decodes with .NET and hands to Invoke-Expression. No temporary
// file, no assumption about locale, no dependency on PSReadLine.
//
// The readiness marker is printed before Invoke-Expression so waitForReady
// consumes the login banner and any prompt output first. Splitting the
// marker into two adjacent literals keeps a terminal echo of this very
// command from looking like the marker itself, mirroring the POSIX version.
func Base64BootstrapLinePwsh(token string) string {
	prefix := "# F4B64" + token
	payload := prefix + "\n" + HelperScriptPwsh(token)
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))
	// Same reason as the POSIX version: the encoded blob must not contain the
	// marker as one contiguous run of bytes.
	encoded = splitReadyMarker(encoded, token)
	return "$F4B='" + encoded + "'; " +
		"Write-Output ('F4R'+'DY'+'" + token + "'); " +
		"try { " +
		"$F4S=[System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($F4B)); " +
		"Remove-Variable F4B -Force -ErrorAction SilentlyContinue; " +
		"Invoke-Expression $F4S " +
		"} catch { " +
		"Write-Output ('.' + '" + token + "' + ' 0 err bootstrap ' + $_.Exception.Message.Replace([char]10,' ').Replace([char]13,' ')) " +
		"}\n"
}
