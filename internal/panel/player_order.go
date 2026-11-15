package panel

import (
	"math/rand/v2"

	"github.com/unxed/f4/internal/config"
)

// The order the player plays in (#2185): what it repeats when it gets to the
// end (config.App.PlayerRepeat: nothing, the whole list, the track that
// plays; R goes through them) and whether it plays the playlist in a random
// order (config.App.PlayerShuffle, S). Both are kept in settings.ini.
//
// Repeat one plays the track again when it ends; Next and Previous still go
// to another one. Repeat all goes on past the end of the list, by the end of
// a track and by Next or Previous, in the playlist and in the recordings of a
// file panel. Shuffle mixes the playlist, each track once in a round: the
// order is kept while the playlist changes, so Previous goes back through
// what played, and with repeat all the next round has an order of its own.
// The recordings of a file panel play in the order of the panel.

// playOrder is the tracks of the playlist in the order they play: from the
// top down, or with shuffle on in the session's mix of them.
func (s *playerSession) playOrder() []*playlistItem {
	list := s.root.tracks(nil)
	if !config.App.PlayerShuffle {
		s.shuffled = nil
		return list
	}
	s.shuffled = keepShuffled(s.shuffled, list, s.at, s.randIntN)
	return s.shuffled
}

// randIntN is a random number in [0, n), from the test's source when it gave
// one.
func (s *playerSession) randIntN(n int) int {
	if s.intn != nil {
		return s.intn(n)
	}
	return rand.IntN(n) //nolint:gosec // the order of the tracks, not a secret
}

// keepShuffled is the mix mixed brought up to date with the tracks of list:
// what has left the playlist leaves it, and a track added goes to a random
// place among those that have not played yet, after at. With no mix yet it is
// a new one that starts with at, which plays already.
func keepShuffled(mixed, list []*playlistItem, at *playlistItem, intn func(int) int) []*playlistItem {
	if len(mixed) == 0 {
		mixed = shuffledTracks(list, intn)
		if i := indexOfItem(mixed, at); i > 0 {
			mixed[0], mixed[i] = mixed[i], mixed[0]
		}
		return mixed
	}
	inList := make(map[*playlistItem]bool, len(list))
	for _, it := range list {
		inList[it] = true
	}
	kept := make([]*playlistItem, 0, len(list))
	inMix := make(map[*playlistItem]bool, len(mixed))
	for _, it := range mixed {
		if inList[it] {
			kept = append(kept, it)
			inMix[it] = true
		}
	}
	pos := indexOfItem(kept, at)
	for _, it := range list {
		if inMix[it] {
			continue
		}
		j := pos + 1 + intn(len(kept)-pos)
		kept = append(kept, nil)
		copy(kept[j+1:], kept[j:])
		kept[j] = it
	}
	return kept
}

// shuffledTracks is a new random order of list.
func shuffledTracks(list []*playlistItem, intn func(int) int) []*playlistItem {
	mixed := append([]*playlistItem(nil), list...)
	for i := len(mixed) - 1; i > 0; i-- {
		j := intn(i + 1)
		mixed[i], mixed[j] = mixed[j], mixed[i]
	}
	return mixed
}

// nextRound is the mix of the next round of repeat all with shuffle on. The
// track that ended the round does not start the next one.
func (s *playerSession) nextRound(order []*playlistItem) []*playlistItem {
	mixed := shuffledTracks(order, s.randIntN)
	if len(mixed) > 1 && mixed[0] == s.at {
		j := 1 + s.randIntN(len(mixed)-1)
		mixed[0], mixed[j] = mixed[j], mixed[0]
	}
	s.shuffled = mixed
	return mixed
}

// walkOrder tries the places of a list of n after pos, by delta, one after
// another, until try takes one. With wrap it goes on past either end and
// tries every place once, pos itself the last. A pos outside the list is
// before the first place, or with delta < 0 after the last.
func walkOrder(n, pos, delta int, wrap bool, try func(int) bool) bool {
	if n == 0 {
		return false
	}
	if pos < 0 || pos >= n {
		pos = -1
		if delta < 0 {
			pos = n
		}
	}
	for range n {
		pos += delta
		if pos < 0 || pos >= n {
			if !wrap {
				return false
			}
			pos = (pos%n + n) % n
		}
		if try(pos) {
			return true
		}
	}
	return false
}

// playOnAfterEnd is what plays when a track has ended: with repeat one the
// same track again, else the next one.
func (s *playerSession) playOnAfterEnd() bool {
	if config.App.PlayerRepeat == config.PlayerRepeatOne && s.current != nil && s.playItem(s.current) {
		return true
	}
	return s.playRelative(+1)
}

// cycleRepeat is R: repeat nothing, all, one, nothing.
func (s *playerSession) cycleRepeat() {
	config.App.PlayerRepeat = config.NextPlayerRepeat(config.App.PlayerRepeat)
	config.RequestSaveConfig()
}

// toggleShuffle is S. A mix turned on starts with the track that plays.
func (s *playerSession) toggleShuffle() {
	config.App.PlayerShuffle = !config.App.PlayerShuffle
	s.shuffled = nil
	config.RequestSaveConfig()
}

func repeatsAll() bool { return config.App.PlayerRepeat == config.PlayerRepeatAll }
