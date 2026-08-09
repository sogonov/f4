package fishplus

import "testing"

func TestWirePathToWindows(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"/", ""},
		{"/c", `C:\`},
		{"/c/", `C:\`},
		{"/c/Users/sogonov", `C:\Users\sogonov`},
		{"/c/Users/John Doe/AppData/Local", `C:\Users\John Doe\AppData\Local`},
		{"/d/backups", `D:\backups`},
		{"//server/share", `\\server\share`},
		{"//server/share/rest/of/path", `\\server\share\rest\of\path`},
		{"relative/path", `relative\path`},
	}
	for _, c := range cases {
		got := WirePathToWindows(c.in)
		if got != c.want {
			t.Errorf("WirePathToWindows(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
