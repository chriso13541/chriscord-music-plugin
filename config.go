package main

// config.go — what the plugin is given when the server starts it.
//
// A Chriscord server runs this as a plugin (the admin panel → Plugins): it
// writes one line of JSON to our stdin and closes it —
//
//	{"chriscord_plugin": 1, "id": "music", "server": "http://127.0.0.1:7070",
//	 "token": "ccbot_1_…", "settings": {…}, "data_dir": "…"}
//
// — so nobody ever handles a token: the server made this plugin's identity
// when it was installed. The settings are the ones in plugin.json, as the
// server's owner set them in the admin panel.
//
// (To run a music bot on your own machine with a token instead, that's
// chriscord-musicbot, which still works the old way.)

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	ServerURL   string        // where the Chriscord server is (from the server)
	Token       string        // the plugin's bot token (from the server)
	DataDir     string        // where it keeps yt-dlp, downloads (from the server)
	CodeDir     string        // its own folder (pfp.png can ship there)
	PluginID    string        // its id here: commands are typed as /<id> play
	AloneAfter  time.Duration // alone_minutes — leave after sitting alone this long
	YtdlpEvery  time.Duration // ytdlp_check_hours — how often to check GitHub for a new yt-dlp
	YtdlpPath   string        // ytdlp — use this yt-dlp instead of the managed one (no auto-update)
	FFmpeg      string        // ffmpeg — the ffmpeg to use
	AudioFormat string        // audio_format — "best" (no re-encode) or "mp3"
	Bitrate     string        // bitrate_kbps — Opus bitrate sent to the call
	Channels    int           // stereo — 1 (mono, like voices) or 2
	Autoplay    bool          // autoplay — when the queue runs out, play more like it (until turned off with /music autoplay)
	Volume      float64       // volume_percent / 100 — 1.0 = as downloaded
	MaxQueue    int           // max_queue — most tracks waiting at once
	MaxPlaylist int           // max_playlist — most tracks one playlist link adds
	YtdlpArgs   []string      // ytdlp_args — extra yt-dlp arguments (e.g. --cookies cookies.txt)
}

// hello is the line the server writes to stdin.
type hello struct {
	Plugin   int                        `json:"chriscord_plugin"`
	ID       string                     `json:"id"`
	Server   string                     `json:"server"`
	Token    string                     `json:"token"`
	Settings map[string]json.RawMessage `json:"settings"`
	DataDir  string                     `json:"data_dir"`
}

var errNotPlugin = errors.New("this is a Chriscord server plugin: install it from your server's admin panel (Plugins), " +
	"and the server starts it. To run a music bot yourself with a token, use chriscord-musicbot instead")

// readHello reads the server's line from r (stdin), giving up after a while
// so that running it by hand says what it is instead of waiting forever.
func readHello(r io.Reader, wait time.Duration) (hello, error) {
	type got struct {
		line []byte
		err  error
	}
	ch := make(chan got, 1)
	go func() {
		line, err := bufio.NewReaderSize(r, 64<<10).ReadBytes('\n')
		if err == io.EOF && len(line) > 0 {
			err = nil
		}
		ch <- got{line, err}
	}()
	var g got
	select {
	case g = <-ch:
	case <-time.After(wait):
		return hello{}, errNotPlugin
	}
	if g.err != nil {
		return hello{}, errNotPlugin
	}
	var h hello
	if err := json.Unmarshal(g.line, &h); err != nil || h.Plugin != 1 {
		return hello{}, errNotPlugin
	}
	if h.Plugin != 1 || h.Token == "" || h.Server == "" {
		return hello{}, errors.New("the server's start-up message is missing the token or address — update the server")
	}
	return h, nil
}

// configFrom turns the server's hello into the settings the bot runs with.
// The server has already checked each setting against plugin.json (types,
// min/max, choices); this still copes with anything missing.
func configFrom(h hello) (config, error) {
	s := settings(h.Settings)
	c := config{
		ServerURL:   strings.TrimRight(h.Server, "/"),
		Token:       h.Token,
		DataDir:     h.DataDir,
		CodeDir:     os.Getenv("CHRISCORD_PLUGIN_DIR"),
		PluginID:    h.ID,
		Autoplay:    s.boolean("autoplay", false),
		YtdlpPath:   strings.TrimSpace(s.text("ytdlp", "")),
		FFmpeg:      strings.TrimSpace(s.text("ffmpeg", "")),
		AudioFormat: strings.ToLower(s.text("audio_format", "best")),
		Bitrate:     fmt.Sprintf("%dk", int(s.number("bitrate_kbps", 96))),
		Channels:    1,
		Volume:      s.number("volume_percent", 50) / 100,
		MaxQueue:    int(s.number("max_queue", 200)),
		MaxPlaylist: int(s.number("max_playlist", 50)),
		AloneAfter:  minutes(s.number("alone_minutes", 5)),
		YtdlpEvery:  time.Duration(s.number("ytdlp_check_hours", 24) * float64(time.Hour)),
		YtdlpArgs:   strings.Fields(s.text("ytdlp_args", "")),
	}
	if c.DataDir == "" {
		c.DataDir = "."
	}
	if c.PluginID == "" {
		c.PluginID = "music"
	}
	// (Settings from before chat commands were taken out — "prefix",
	// "text_commands" — may still be saved for it: they're just not read.)
	if c.FFmpeg == "" {
		c.FFmpeg = "ffmpeg"
	}
	if s.boolean("stereo", false) {
		c.Channels = 2
	}
	if !strings.Contains(c.ServerURL, "://") {
		c.ServerURL = "http://" + c.ServerURL
	}
	if c.AudioFormat != "best" && c.AudioFormat != "mp3" {
		c.AudioFormat = "best"
	}
	if c.AloneAfter <= 0 {
		c.AloneAfter = 5 * time.Minute
	}
	if c.YtdlpEvery <= 0 {
		c.YtdlpEvery = 24 * time.Hour
	}
	if c.MaxQueue <= 0 {
		c.MaxQueue = 200
	}
	if c.MaxPlaylist <= 0 {
		c.MaxPlaylist = 50
	}
	if c.Volume <= 0 {
		c.Volume = 0.5
	}
	return c, nil
}

func minutes(m float64) time.Duration { return time.Duration(m * float64(time.Minute)) }

type settings map[string]json.RawMessage

func (s settings) text(k, def string) string {
	var v string
	if json.Unmarshal(s[k], &v) == nil {
		return v
	}
	return def
}

func (s settings) number(k string, def float64) float64 {
	var v float64
	if json.Unmarshal(s[k], &v) == nil {
		return v
	}
	var t string // a number sent as text
	if json.Unmarshal(s[k], &t) == nil {
		if n, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return n
		}
	}
	return def
}

func (s settings) boolean(k string, def bool) bool {
	var v bool
	if json.Unmarshal(s[k], &v) == nil {
		return v
	}
	return def
}
