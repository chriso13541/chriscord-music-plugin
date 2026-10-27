package main

// autoplay.go — what's been played (kept in the plugin's data folder), and
// autoplay: when the queue runs out, more like what people were listening
// to is queued, one track at a time.
//
// "Like what people were listening to" is YouTube's own mix for the last
// track someone asked for (list=RD<video>: the "Mix" YouTube makes after
// any video). A track someone asked for that isn't on YouTube is looked up
// there by its title first. Anything played lately, live streams and very
// long videos are left out. Someone queueing something themselves always
// comes first: queued autoplay tracks make way for it.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	historyKeep       = 500              // tracks kept in the history file
	autoplayRecent    = 100              // not played again within this many tracks
	autoplayMinLen    = 30 * time.Second // shorter: an intro, a jingle
	autoplayMaxLen    = 15 * time.Minute // longer: an hour-long mix, a podcast
	autoplayMixSize   = 40
	autoplayRequester = "Autoplay"
)

// historyEntry is one track that played.
type historyEntry struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Uploader string `json:"uploader,omitempty"`
	Seconds  int    `json:"seconds,omitempty"`
	By       string `json:"by"`             // who asked for it ("Autoplay" for autoplay's own)
	Auto     bool   `json:"auto,omitempty"` // queued by autoplay
	At       int64  `json:"at"`             // when it started (unix seconds)
}

type autoplayFile struct {
	Autoplay *bool          `json:"autoplay,omitempty"` // turned on or off here (else the setting)
	History  []historyEntry `json:"history"`
}

type autoplay struct {
	b    *Bot
	path string

	mu      sync.Mutex
	on      bool
	history []historyEntry // oldest first
	picking bool
	seed    string      // the video the cached mix is for
	mix     []trackInfo // that mix, less what's been used
}

func newAutoplay(b *Bot) *autoplay {
	a := &autoplay{b: b, path: filepath.Join(b.cfg.DataDir, "history.json"), on: b.cfg.Autoplay}
	if data, err := os.ReadFile(a.path); err == nil {
		var f autoplayFile
		if err := json.Unmarshal(data, &f); err != nil {
			logf("autoplay: %s is damaged (%v) — starting a new history", a.path, err)
		} else {
			a.history = f.History
			if f.Autoplay != nil {
				a.on = *f.Autoplay
			}
		}
	}
	return a
}

// saveLocked writes the history file whole (a temporary file, renamed over
// the old one: never half written).
func (a *autoplay) saveLocked() {
	on := a.on
	data, err := json.MarshalIndent(autoplayFile{Autoplay: &on, History: a.history}, "", " ")
	if err != nil {
		return
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		logf("autoplay: saving the history: %v", err)
		return
	}
	if err := os.Rename(tmp, a.path); err != nil {
		logf("autoplay: saving the history: %v", err)
	}
}

// enabled: whether autoplay is on.
func (a *autoplay) enabled() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.on }

// set turns autoplay on or off (kept across restarts). Turned on with
// nothing queued, it queues something straight away.
func (a *autoplay) set(on bool) {
	a.mu.Lock()
	a.on = on
	a.saveLocked()
	a.mu.Unlock()
	if on {
		go a.fill()
	} else {
		a.b.pl.dropAuto()
	}
}

// played: t has started playing — into the history, and if nothing's
// queued after it, autoplay looks for the next one now (so it's ready).
func (a *autoplay) played(t *track) {
	a.mu.Lock()
	a.history = append(a.history, historyEntry{URL: t.URL, Title: t.Title, Uploader: t.Uploader,
		Seconds: int(t.Duration / time.Second), By: t.requester, Auto: t.auto, At: time.Now().Unix()})
	if len(a.history) > historyKeep {
		a.history = a.history[len(a.history)-historyKeep:]
	}
	a.saveLocked()
	a.mu.Unlock()
	go a.fill()
}

// recent: the last n tracks played, newest first.
func (a *autoplay) recent(n int) []historyEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []historyEntry
	for i := len(a.history) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, a.history[i])
	}
	return out
}

// fill queues one track like what's been played, if autoplay's on, the
// queue's empty and someone's in the call to hear it.
func (a *autoplay) fill() {
	a.mu.Lock()
	if !a.on || a.picking {
		a.mu.Unlock()
		return
	}
	a.picking = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); a.picking = false; a.mu.Unlock() }()

	if a.b.c == nil {
		return // not connected (a test)
	}
	board, _ := a.b.inVoice()
	v := a.b.pl.view()
	if board == "" || len(v.queue) > 0 || len(a.b.othersIn(board)) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t, err := a.pick(ctx, v)
	if err != nil {
		logf("autoplay: %v", err)
		return
	}
	if t == nil {
		return
	}
	// Still nothing queued (someone may have asked for something meanwhile).
	if v := a.b.pl.view(); len(v.queue) > 0 || !a.enabled() {
		return
	}
	logf("autoplay: queued %q", t.Title)
	a.b.pl.enqueue([]*track{t})
	a.b.ui.poke()
}

// pick: the next track for autoplay (nil if there's nothing to go on).
func (a *autoplay) pick(ctx context.Context, v queueView) (*track, error) {
	a.mu.Lock()
	// The last track someone asked for themselves; else the last one played.
	var seed *historyEntry
	for i := len(a.history) - 1; i >= 0; i-- {
		if !a.history[i].Auto {
			seed = &a.history[i]
			break
		}
	}
	if seed == nil && len(a.history) > 0 {
		seed = &a.history[len(a.history)-1]
	}
	played := map[string]bool{}
	for i := len(a.history) - 1; i >= 0 && len(a.history)-i <= autoplayRecent; i-- {
		played[videoKey(a.history[i].URL)] = true
	}
	var s historyEntry
	if seed != nil {
		s = *seed
	}
	a.mu.Unlock()
	if seed == nil {
		return nil, nil // nothing's been played yet
	}
	if v.current != nil {
		played[videoKey(v.current.URL)] = true
	}
	board := ""
	if v.current != nil {
		board = v.current.board
	}

	id := youtubeID(s.URL)
	if id == "" {
		// Not from YouTube: find it there by name.
		res, err := a.b.ytdlp.search(ctx, strings.TrimSpace(s.Title+" "+s.Uploader), 1)
		if err != nil || len(res) == 0 {
			return nil, fmt.Errorf("couldn't find %q on YouTube to go on from", s.Title)
		}
		id = youtubeID(res[0].URL)
	}
	if id == "" {
		return nil, nil
	}
	a.mu.Lock()
	mix := a.mix
	if a.seed != id {
		mix = nil
	}
	a.mu.Unlock()
	if len(mix) == 0 {
		r, err := a.b.ytdlp.resolve(ctx, "https://www.youtube.com/watch?v="+id+"&list=RD"+id, autoplayMixSize)
		if err != nil {
			return nil, fmt.Errorf("YouTube's mix for %q: %w", s.Title, err)
		}
		mix = r.tracks
	}
	for i, t := range mix {
		if t.Live || played[videoKey(t.URL)] || (t.Duration > 0 && (t.Duration < autoplayMinLen || t.Duration > autoplayMaxLen)) {
			continue
		}
		a.mu.Lock()
		a.seed, a.mix = id, mix[i+1:]
		a.mu.Unlock()
		return &track{trackInfo: t, requester: autoplayRequester, board: board, auto: true}, nil
	}
	a.mu.Lock()
	a.seed, a.mix = "", nil
	a.mu.Unlock()
	return nil, fmt.Errorf("everything in the mix for %q was played lately", s.Title)
}

// youtubeID: a YouTube video's id from its link ("" if it isn't one).
func youtubeID(u string) string {
	l, err := parseLink(u)
	if err != nil || (l.Kind != linkVideo && l.Kind != linkRadio) {
		return ""
	}
	if i := strings.Index(l.URL, "v="); i >= 0 {
		id := l.URL[i+2:]
		if j := strings.IndexByte(id, '&'); j >= 0 {
			id = id[:j]
		}
		if ytIDPattern.MatchString(id) {
			return id
		}
	}
	return ""
}

// videoKey: what makes two links the same track (a YouTube video's id, or
// the link itself).
func videoKey(u string) string {
	if id := youtubeID(u); id != "" {
		return "yt:" + id
	}
	return u
}

// historyText: the last n tracks played, for /music history.
func (b *Bot) historyText(n int) string {
	h := b.auto.recent(n)
	if len(h) == 0 {
		return "Nothing's been played yet."
	}
	var sb strings.Builder
	sb.WriteString("Played lately (newest first):")
	for i, e := range h {
		by := "asked for by " + e.By
		if e.Auto {
			by = "autoplay"
		}
		fmt.Fprintf(&sb, "\n%d. %s — %s, %s", i+1, e.Title, by, time.Unix(e.At, 0).Format("Jan 2 3:04 PM"))
	}
	return sb.String()
}
