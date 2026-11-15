package app

import (
	"strings"
	"testing"

	"github.com/unxed/f4/internal/config"
	"github.com/unxed/f4/internal/theme"
	"github.com/unxed/vtinput"
	"github.com/unxed/vtui"
)

// Repeat and shuffle (#2185): R goes from repeating nothing to the whole
// playlist, to the track that plays and back to nothing, S mixes the
// playlist, and both show at the right end of the row of the stream facts as
// [R] and [S], lit while on, and [R1] while one track repeats.

// lit is whether the label text on the player is drawn lit, as a mode that
// is on.
func (f *playerFixture) lit(text string) bool {
	f.t.Helper()
	x, y := f.find(text)
	return f.scr.GetCell(x, y).Attributes == vtui.Palette[theme.ColPanelSelectedText]
}

// modes is repeat and shuffle as the player shows them: "[R]-", "[R1]+",
// the label and whether it is lit.
func (f *playerFixture) modes() (repeat, shuffle string) {
	f.t.Helper()
	label := func(text string) string {
		if f.lit(text) {
			return text + "+"
		}
		return text + "-"
	}
	x1, _, rows := f.playerRows()
	for _, row := range rows {
		if s := string(row); strings.Contains(s, "[S]") {
			repeat = "[R]"
			if strings.Contains(s, "[R1]") {
				repeat = "[R1]"
			}
			return label(repeat), label("[S]")
		}
	}
	f.t.Fatalf("no [S] on the player (x %d):\n%s", x1, strings.Join(runeRows(rows), "\n"))
	return "", ""
}

func TestPlayerRAndSChangeRepeatAndShuffle(t *testing.T) {
	f := newPlayerFixture(t, "a.mp3")
	f.playlistOf("a.mp3")
	check := func(step, repeat, wantRepeat, wantShuffle string, shuffle bool) {
		t.Helper()
		gotRepeat, gotShuffle := f.modes()
		if config.App.PlayerRepeat != repeat || config.App.PlayerShuffle != shuffle || gotRepeat != wantRepeat || gotShuffle != wantShuffle {
			t.Errorf("%s: repeat %q, shuffle %v, shown %s %s; want %q, %v, shown %s %s", step,
				config.App.PlayerRepeat, config.App.PlayerShuffle, gotRepeat, gotShuffle, repeat, shuffle, wantRepeat, wantShuffle)
		}
	}
	check("at first", config.PlayerRepeatOff, "[R]-", "[S]-", false)
	f.press(playerChar(vtinput.VK_R, 'r'))
	check("R", config.PlayerRepeatAll, "[R]+", "[S]-", false)
	f.press(playerChar(0, 'к')) // R in the Russian layout, from a terminal that sends the character alone
	check("R again", config.PlayerRepeatOne, "[R1]+", "[S]-", false)
	f.press(playerChar(vtinput.VK_R, 'r'))
	check("R a third time", config.PlayerRepeatOff, "[R]-", "[S]-", false)
	f.press(playerChar(vtinput.VK_S, 's'))
	check("S", config.PlayerRepeatOff, "[R]-", "[S]+", true)
	f.press(playerChar(0, 'ы'))
	check("S again", config.PlayerRepeatOff, "[R]-", "[S]-", false)
	if !f.pf.CmdLine.IsEmpty() {
		t.Errorf("the letters went to the command line too: %q", f.pf.CmdLine.Edit.GetText())
	}
}

// [R] and [S] under the mouse, on the row of the facts, and with the bar off
// on the row of the clock, where a click elsewhere puts the cursor on the
// clock.
func TestPlayerRepeatAndShuffleUnderTheMouse(t *testing.T) {
	oldBar := config.App.PlayerBar
	t.Cleanup(func() { config.App.PlayerBar = oldBar })
	config.App.PlayerBar = true

	f := newPlayerFixture(t, "a.mp3")
	f.playlistOf("a.mp3")
	f.click(f.find("[R]"))
	f.click(f.find("[S]"))
	if config.App.PlayerRepeat != config.PlayerRepeatAll || !config.App.PlayerShuffle {
		t.Fatalf("a click on [R] and one on [S]: repeat %q, shuffle %v; want all and on", config.App.PlayerRepeat, config.App.PlayerShuffle)
	}

	config.App.PlayerBar = false
	f.click(f.find("[R]"))
	if config.App.PlayerRepeat != config.PlayerRepeatOne {
		t.Errorf("with the bar off a click on [R] left the repeat at %q, want one", config.App.PlayerRepeat)
	}
	f.click(f.find("[S]"))
	if config.App.PlayerShuffle {
		t.Error("with the bar off a click on [S] left the shuffle on")
	}
}

// Repeat all goes on past either end of the playlist, with Next and with
// Previous; without it the ends are the ends.
func TestPlayerRepeatAllGoesOnPastTheEnds(t *testing.T) {
	f := newPlayerFixture(t, "a.mp3", "b.mp3", "c.mp3")
	f.playlistOf("a.mp3", "b.mp3", "c.mp3")
	f.press(playerKey(vtinput.VK_RETURN, 0)) // a
	f.press(playerChar(vtinput.VK_Z, 'z'))
	if got := f.audio.Path(); got != f.path("a.mp3") {
		t.Fatalf("Previous on the first track without repeat loaded %q, want a.mp3 to stay", got)
	}

	f.press(playerChar(vtinput.VK_R, 'r')) // repeat all
	f.press(playerChar(vtinput.VK_Z, 'z'))
	if got := f.audio.Path(); got != f.path("c.mp3") {
		t.Errorf("Previous on the first track with repeat all loaded %q, want the last, c.mp3", got)
	}
	f.press(playerChar(vtinput.VK_B, 'b'))
	if got := f.audio.Path(); got != f.path("a.mp3") {
		t.Errorf("Next on the last track with repeat all loaded %q, want the first, a.mp3", got)
	}
}

// Shuffle plays every track of the playlist once, in an order of its own,
// starting from the one that plays; Previous goes back through it. Without
// repeat a round ends where it ends; with repeat all the next round starts
// with another track than the last one and plays every track once again.
func TestPlayerShufflePlaysEachTrackOnceARound(t *testing.T) {
	names := []string{"a.mp3", "b.mp3", "c.mp3", "d.mp3"}
	f := newPlayerFixture(t, names...)
	f.playlistOf(names...)
	f.press(playerKey(vtinput.VK_RETURN, 0)) // a
	f.press(playerChar(vtinput.VK_S, 's'))

	next := func() string {
		t.Helper()
		f.press(playerChar(vtinput.VK_B, 'b'))
		return f.audio.Path()
	}
	round := []string{f.path("a.mp3"), next(), next(), next()}
	if !playsEachOnce(round, f, names) {
		t.Fatalf("a round of the shuffle played %q, want each of %q once from a", round, names)
	}
	if got := next(); got != round[3] {
		t.Errorf("Next after the round without repeat loaded %q, want %q to stay", got, round[3])
	}
	f.press(playerChar(vtinput.VK_Z, 'z'))
	if got := f.audio.Path(); got != round[2] {
		t.Errorf("Previous in the shuffle loaded %q, want the track before, %q", got, round[2])
	}

	f.press(playerChar(vtinput.VK_B, 'b')) // back to the end of the round
	f.press(playerChar(vtinput.VK_R, 'r')) // repeat all
	second := []string{next()}
	if second[0] == round[3] {
		t.Errorf("the second round starts with %q, the track that ended the first", second[0])
	}
	second = append(second, next(), next(), next())
	if !playsEachOnce(second, f, names) {
		t.Errorf("the second round of the shuffle played %q, want each of %q once", second, names)
	}
}

func playsEachOnce(played []string, f *playerFixture, names []string) bool {
	if len(played) != len(names) {
		return false
	}
	seen := map[string]bool{}
	for _, p := range played {
		seen[p] = true
	}
	for _, n := range names {
		if !seen[f.path(n)] {
			return false
		}
	}
	return true
}
