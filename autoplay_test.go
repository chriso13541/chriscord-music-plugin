//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A stand-in yt-dlp that answers any question with a YouTube mix.
const fakeMix = `#!/bin/sh
cat <<'JSON'
{"_type":"playlist","title":"Mix - Seed song","entries":[
 {"id":"AAAAAAAAAAA","ie_key":"Youtube","url":"https://www.youtube.com/watch?v=AAAAAAAAAAA","title":"Seed song","duration":200},
 {"id":"BBBBBBBBBBB","ie_key":"Youtube","url":"https://www.youtube.com/watch?v=BBBBBBBBBBB","title":"Played lately","duration":200},
 {"id":"CCCCCCCCCCC","ie_key":"Youtube","url":"https://www.youtube.com/watch?v=CCCCCCCCCCC","title":"Hour-long mix","duration":4000},
 {"id":"DDDDDDDDDDD","ie_key":"Youtube","url":"https://www.youtube.com/watch?v=DDDDDDDDDDD","title":"Good one","duration":210},
 {"id":"EEEEEEEEEEE","ie_key":"Youtube","url":"https://www.youtube.com/watch?v=EEEEEEEEEEE","title":"Next good one","duration":180}
]}
JSON
`

func testAutoplay(t *testing.T) *autoplay {
	dir := t.TempDir()
	bin := filepath.Join(dir, "yt-dlp")
	if err := os.WriteFile(bin, []byte(fakeMix), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := config{DataDir: dir, YtdlpPath: bin, MaxQueue: 50}
	b := &Bot{cfg: cfg, ytdlp: newYtdlp(cfg)}
	b.pl = newPlayer(b)
	b.auto = newAutoplay(b)
	return b.auto
}

func TestAutoplayPicksFromMix(t *testing.T) {
	a := testAutoplay(t)
	a.played(&track{trackInfo: trackInfo{URL: "https://www.youtube.com/watch?v=BBBBBBBBBBB", Title: "Played lately"}, requester: "alice"})
	a.played(&track{trackInfo: trackInfo{URL: "https://youtu.be/AAAAAAAAAAA", Title: "Seed song"}, requester: "alice"})
	ctx := context.Background()
	got, err := a.pick(ctx, queueView{})
	if err != nil || got == nil {
		t.Fatal(err)
	}
	// Not the seed or what played lately, not the hour-long one.
	if got.Title != "Good one" || !got.auto || got.requester != "Autoplay" {
		t.Fatalf("picked %+v", got)
	}
	// Its pick plays (autoplay's own): the next comes from the same mix,
	// still made for the song alice asked for.
	a.played(got)
	if next, _ := a.pick(ctx, queueView{}); next == nil || next.Title != "Next good one" {
		t.Fatalf("next %+v", next)
	}
}

func TestHistoryKeptAndAutoplayRemembered(t *testing.T) {
	a := testAutoplay(t)
	a.set(true)
	a.played(&track{trackInfo: trackInfo{URL: "https://youtu.be/AAAAAAAAAAA", Title: "Seed song", Duration: 3 * time.Minute}, requester: "alice"})
	// Restarted: the history and the switch are still there.
	b := &Bot{cfg: a.b.cfg}
	again := newAutoplay(b)
	if !again.enabled() || len(again.history) != 1 || again.history[0].Seconds != 180 || again.history[0].By != "alice" {
		t.Fatalf("on %v history %+v", again.enabled(), again.history)
	}
	if txt := a.b.historyText(5); !strings.Contains(txt, "Seed song — asked for by alice") {
		t.Fatalf("history: %s", txt)
	}
	if _, err := os.Stat(a.path + ".tmp"); err == nil {
		t.Fatal("left a temporary file")
	}
}

func TestDropAuto(t *testing.T) {
	a := testAutoplay(t)
	p := a.b.pl
	p.queue = []*track{{trackInfo: trackInfo{Title: "auto 1"}, auto: true}, {trackInfo: trackInfo{Title: "asked for"}}}
	if n := p.dropAuto(); n != 1 || len(p.queue) != 1 || p.queue[0].Title != "asked for" {
		t.Fatalf("dropped %d, queue %v", n, p.queue)
	}
	if youtubeID("https://music.youtube.com/watch?v=AAAAAAAAAAA&si=x") != "AAAAAAAAAAA" || youtubeID("https://soundcloud.com/x/y") != "" {
		t.Fatal("youtubeID")
	}
}
