//go:build !noffi && !lite && !android && (windows || ((linux || darwin || freebsd) && (amd64 || arm64)))

package media

// A probe for #2185, never meant for main: what the device does around a
// suspend and a resume, as a time series in the log.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAudioProbeSuspendResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "silence.wav")
	if err := os.WriteFile(path, wavBytes(1, 2, 44100, 16, make([]byte, 8*44100*AudioBytesPerFrame)), 0o600); err != nil {
		t.Fatal(err)
	}
	a := NewAudioEngine()
	if err := a.Load(path); err != nil {
		t.Skipf("no audio output here: %v", err)
	}
	defer a.Close()
	state := func(step string) {
		a.mu.Lock()
		read, buffered := int64(0), 0
		var perr, cerr error
		playing := false
		if a.tap != nil {
			read = a.tap.BytesRead()
		}
		if a.player != nil {
			buffered = a.player.BufferedSize()
			perr = a.player.Err()
			playing = a.player.IsPlaying()
		}
		if a.ctx != nil {
			cerr = a.ctx.Err()
		}
		pos := a.PositionLocked()
		a.mu.Unlock()
		a.power.mu.Lock()
		suspended := a.power.suspended
		a.power.mu.Unlock()
		t.Logf("%-18s pos=%-12s read=%-8d buffered=%-6d playing=%v suspended=%v playerErr=%v ctxErr=%v", step, pos, read, buffered, playing, suspended, perr, cerr)
	}
	series := func(label string, d time.Duration) {
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			state(label)
		}
	}

	a.Play()
	series("playing", 600*time.Millisecond)

	// 1: pause, the device suspended by outputPower, play.
	a.power.mu.Lock()
	a.power.delay = 50 * time.Millisecond
	a.power.mu.Unlock()
	a.Pause()
	series("paused", 500*time.Millisecond)
	a.Play()
	series("after resume", 1500*time.Millisecond)

	// 2: the context suspended and resumed under a playing player, without
	// outputPower, with the settle between the two calls.
	a.mu.Lock()
	ctx := a.ctx
	a.mu.Unlock()
	if err := ctx.Suspend(); err != nil {
		t.Logf("Suspend: %v", err)
	}
	series("ctx suspended", 400*time.Millisecond)
	if err := ctx.Resume(); err != nil {
		t.Logf("Resume: %v", err)
	}
	series("ctx resumed", 1500*time.Millisecond)
}
