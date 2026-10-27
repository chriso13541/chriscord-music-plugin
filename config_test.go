package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// The line the server writes (plugins.rs: numbers arrive as floats, after
// it has applied plugin.json's defaults, min and max).
func TestHelloFromServer(t *testing.T) {
	line := `{"chriscord_plugin":1,"id":"music","server":"http://127.0.0.1:7070/","token":"ccbot_1_x",` +
		`"settings":{"prefix":"?","text_commands":true,"volume_percent":80.0,"bitrate_kbps":128.0,"stereo":true,"audio_format":"mp3",` +
		`"max_queue":10.0,"max_playlist":"5","alone_minutes":2.5,"ytdlp_check_hours":12.0,` +
		`"ytdlp":"","ffmpeg":"","ytdlp_args":"--cookies cookies.txt"},"data_dir":"/srv/plugin-data/music"}` + "\n"
	h, err := readHello(strings.NewReader(line), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := configFrom(h)
	if err != nil {
		t.Fatal(err)
	}
	want := config{ServerURL: "http://127.0.0.1:7070", Token: "ccbot_1_x", DataDir: "/srv/plugin-data/music",
		FFmpeg: "ffmpeg", AudioFormat: "mp3", Bitrate: "128k", Channels: 2, Volume: 0.8, MaxQueue: 10, MaxPlaylist: 5,
		AloneAfter: 150 * time.Second, YtdlpEvery: 12 * time.Hour}
	c.YtdlpArgs, c.CodeDir = nil, ""
	if c.ServerURL != want.ServerURL || c.Token != want.Token || c.DataDir != want.DataDir ||
		c.FFmpeg != want.FFmpeg || c.AudioFormat != want.AudioFormat || c.Bitrate != want.Bitrate || c.Channels != want.Channels ||
		c.Volume != want.Volume || c.MaxQueue != want.MaxQueue || c.MaxPlaylist != want.MaxPlaylist ||
		c.AloneAfter != want.AloneAfter || c.YtdlpEvery != want.YtdlpEvery {
		t.Fatalf("got %+v\nwant %+v", c, want)
	}
}

func TestHelloDefaults(t *testing.T) {
	h, err := readHello(strings.NewReader(`{"chriscord_plugin":1,"server":"127.0.0.1:7071","token":"t"}`), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := configFrom(h)
	if c.ServerURL != "http://127.0.0.1:7071" || c.Volume != 0.5 || c.Bitrate != "96k" || c.Channels != 1 ||
		c.AloneAfter != 5*time.Minute || c.DataDir != "." {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestNotStartedByServer(t *testing.T) {
	for _, in := range []string{"", "hello\n", `{"token":"x"}` + "\n"} {
		if _, err := readHello(strings.NewReader(in), time.Second); err != errNotPlugin {
			t.Fatalf("%q: %v", in, err)
		}
	}
	// Run by hand from a terminal: stdin stays open and nothing comes.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer r.Close()
	if _, err := readHello(r, 50*time.Millisecond); err != errNotPlugin {
		t.Fatal(err)
	}
}
