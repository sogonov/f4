package fishplus

import "strings"

// WirePathToWindows converts a POSIX-shaped wire path back to the
// Windows path it stands for. helper.ps1 speaks the Cygwin convention
// on the wire (/c/Users/foo, //server/share/rest, / for the virtual
// root), and callers that talk to a native Windows shell — cmd.exe on
// a PTY channel, for instance — need the Windows form.
//
// Mirrors Convert-PosixToWin in helper.ps1. Kept in the fishplus
// package rather than in netfox so a caller with only a client and no
// FishVFS wrapper can still use it.
func WirePathToWindows(p string) string {
	if p == "" || p == "/" {
		return ""
	}
	if strings.HasPrefix(p, "//") {
		// UNC: //server/share/rest -> \\server\share\rest
		return `\\` + strings.ReplaceAll(p[2:], "/", `\`)
	}
	if !strings.HasPrefix(p, "/") {
		// Relative paths never enter our wire, but if one shows up
		// hand it back with the separators flipped rather than
		// producing something nonsensical.
		return strings.ReplaceAll(p, "/", `\`)
	}
	tail := p[1:]
	slash := strings.Index(tail, "/")
	if slash < 0 {
		// /c -> C:\, or /somethingLonger -> \somethingLonger
		if len(tail) == 1 {
			return strings.ToUpper(tail) + `:\`
		}
		return strings.ReplaceAll(tail, "/", `\`)
	}
	drive := tail[:slash]
	rest := tail[slash+1:]
	if len(drive) == 1 {
		return strings.ToUpper(drive) + `:\` + strings.ReplaceAll(rest, "/", `\`)
	}
	// First segment is not a drive letter: separator swap and hope for
	// the best. helper.sh paths never take this branch; Windows peers
	// always produce single-letter drive prefixes.
	return strings.ReplaceAll(p[1:], "/", `\`)
}
