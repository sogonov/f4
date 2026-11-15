package panel

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/unxed/f4/internal/config"
)

// usePlayerModes sets repeat and shuffle for the test and puts them back.
func usePlayerModes(t *testing.T, repeat string, shuffle bool) {
	t.Helper()
	oldRepeat, oldShuffle := config.App.PlayerRepeat, config.App.PlayerShuffle
	t.Cleanup(func() { config.App.PlayerRepeat, config.App.PlayerShuffle = oldRepeat, oldShuffle })
	config.App.PlayerRepeat, config.App.PlayerShuffle = repeat, shuffle
}

func TestWalkOrder(t *testing.T) {
	for _, c := range []struct {
		name           string
		n, pos, delta  int
		wrap           bool
		want           []int // the places tried, none taken
		wantTakenFirst bool
	}{
		{name: "on from the middle", n: 4, pos: 1, delta: +1, want: []int{2, 3}},
		{name: "back from the middle", n: 4, pos: 2, delta: -1, want: []int{1, 0}},
		{name: "from before the first", n: 3, pos: -1, delta: +1, want: []int{0, 1, 2}},
		{name: "back from none starts at the last", n: 3, pos: -1, delta: -1, want: []int{2, 1, 0}},
		{name: "wrap tries every place, itself last", n: 4, pos: 2, delta: +1, wrap: true, want: []int{3, 0, 1, 2}},
		{name: "wrap backwards", n: 3, pos: 0, delta: -1, wrap: true, want: []int{2, 1, 0}},
		{name: "the last without wrap", n: 3, pos: 2, delta: +1, want: nil},
		{name: "an empty list", n: 0, pos: -1, delta: +1, wrap: true, want: nil},
	} {
		var tried []int
		walkOrder(c.n, c.pos, c.delta, c.wrap, func(i int) bool { tried = append(tried, i); return false })
		if !reflect.DeepEqual(tried, c.want) {
			t.Errorf("%s: tried %v, want %v", c.name, tried, c.want)
		}
	}
	var tried []int
	if !walkOrder(5, 0, +1, false, func(i int) bool { tried = append(tried, i); return i == 2 }) || !reflect.DeepEqual(tried, []int{1, 2}) {
		t.Errorf("a walk stops at the place taken: tried %v", tried)
	}
}

// items is n playlist tracks named a, b, c...
func items(n int) []*playlistItem {
	out := make([]*playlistItem, n)
	for i := range out {
		out[i] = &playlistItem{Name: string(rune('a' + i))}
	}
	return out
}

// joinedNames is the names of list in a row: "dbac".
func joinedNames(list []*playlistItem) string {
	return strings.Join(names(list), "")
}

// firstOf is a random source that always picks the first place it may.
func firstOf(int) int { return 0 }

func TestKeepShuffled(t *testing.T) {
	list := items(4) // a b c d

	// A new mix starts with the track that plays.
	if mixed := keepShuffled(nil, list, list[2], firstOf); mixed[0] != list[2] || len(mixed) != 4 {
		t.Errorf("a new mix with c playing is %q, want it to start with c", joinedNames(mixed))
	}

	// Mixed as d b a c, with b playing: a track removed leaves the mix, one
	// added goes after b (firstOf: right after it), and the order of the rest
	// stays.
	mixed := []*playlistItem{list[3], list[1], list[0], list[2]}
	e := &playlistItem{Name: "e"}
	now := []*playlistItem{list[0], list[1], list[3], e} // c removed, e added
	if got := joinedNames(keepShuffled(mixed, now, list[1], firstOf)); got != "dbea" {
		t.Errorf("the mix dbac with b playing, c removed and e added is %q, want dbea", got)
	}
	// A random source that picks the last place puts it at the end.
	last := func(n int) int { return n - 1 }
	if got := joinedNames(keepShuffled(mixed, now, list[1], last)); got != "dbae" {
		t.Errorf("e added at the last place: %q, want dbae", got)
	}
}

// writeTrack is a file the tick engine can load.
func writeTrack(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Repeat one plays the track that ended again; Next still goes on.
func TestPlayerTickRepeatsOneTrack(t *testing.T) {
	usePlayerModes(t, config.PlayerRepeatOne, false)
	s, eng, _ := tickFixture(t, false)
	a := eng.path
	eng.playing, eng.finished = false, true
	s.step()
	if eng.path != a || !eng.playing {
		t.Fatalf("after a's end with repeat one: engine %+v; want a playing again", *eng)
	}
	if !s.playRelative(+1) || filepath.Base(eng.path) != "b.mp3" {
		t.Errorf("Next with repeat one: engine %+v; want b", *eng)
	}
}

// Repeat all starts the playlist again after its last track, and the file
// panel's recordings after the last of them.
func TestPlayerTickRepeatsAll(t *testing.T) {
	usePlayerModes(t, config.PlayerRepeatAll, false)
	s, eng, _ := tickFixture(t, false)
	a := eng.path
	s.playRelative(+1) // b, the last
	eng.playing, eng.finished = false, true
	s.step()
	if eng.path != a || !eng.playing {
		t.Fatalf("after the last track's end with repeat all: engine %+v; want a playing", *eng)
	}

	dir := t.TempDir()
	files := []string{writeTrack(t, dir, "x.mp3"), writeTrack(t, dir, "y.mp3")}
	if !s.PlayFile(files, 1) {
		t.Fatal("the file panel's y.mp3 did not start")
	}
	eng.playing, eng.finished = false, true
	s.step()
	if eng.path != files[0] || !eng.playing {
		t.Errorf("after the last recording's end with repeat all: engine %+v; want x.mp3 playing", *eng)
	}
}

// With shuffle on the end of a track goes on in the mix, and each track of
// the playlist plays once before the music stops.
func TestPlayerTickFollowsTheShuffle(t *testing.T) {
	usePlayerModes(t, config.PlayerRepeatOff, false)
	s, eng, _ := tickFixture(t, false) // a plays
	config.App.PlayerShuffle = true
	dir := t.TempDir()
	for _, n := range []string{"c", "d"} {
		s.root.insertChild(-1, &playlistItem{Name: n, Path: writeTrack(t, dir, n+".mp3")})
	}
	// Fisher-Yates with a source that always says 0 mixes a b c d into
	// b c d a, and a, which plays, goes first: a c d b.
	s.intn = firstOf
	s.shuffled = nil
	var played []string
	for eng.loaded {
		played = append(played, filepath.Base(eng.path))
		eng.playing, eng.finished = false, true
		s.step()
	}
	if got := joinedNames(s.shuffled); got != "acdb" || !reflect.DeepEqual(played, []string{"a.mp3", "c.mp3", "d.mp3", "b.mp3"}) {
		t.Errorf("shuffle played %q (mix %q); want a c d b and then the music stopped", played, got)
	}
}
