package main

// chriscord-music-plugin — music in voice channels, as a Chriscord server
// plugin.
//
// The server runs it (admin panel → Plugins): it's given its own identity
// and the channels the server's owner picked when installing it, and joins
// calls with its own end-to-end encrypted audio, like any member. Since it
// runs on the server, the app labels it "PLUGIN — runs on this server".
// See README.md.

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	chriscordbot "github.com/chriso13541/chriscord-bot-sdk"
)

// version is the plugin's, from plugin.json (build.sh sets it).
var version = "dev" // set by build.sh from plugin.json

const usage = `chriscord-music-plugin — music for a Chriscord server, as a plugin

The server starts this itself: install the plugin's .tar from your
server's admin panel (Plugins). By hand:
    chriscord-music-plugin version
    chriscord-music-plugin update-ytdlp [folder]   fetch the latest yt-dlp into folder/bin
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			fmt.Println("chriscord-music-plugin", version)
			return
		case "help", "-h", "--help":
			fmt.Print(usage)
			return
		case "update-ytdlp":
			cfg := config{DataDir: "."}
			if len(os.Args) > 2 {
				cfg.DataDir = os.Args[2]
			}
			if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
				fatal(err)
			}
			if err := newYtdlp(cfg).update(); err != nil {
				fatal(err)
			}
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}
	h, err := readHello(os.Stdin, 10*time.Second)
	if err != nil {
		fatal(err)
	}
	cfg, err := configFrom(h)
	if err != nil {
		fatal(err)
	}

	for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "downloads")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			fatal(err)
		}
	}
	// Downloads left over from before a restart.
	if old, _ := filepath.Glob(filepath.Join(cfg.DataDir, "downloads", "t-*")); len(old) > 0 {
		for _, o := range old {
			os.RemoveAll(o)
		}
	}
	if _, err := exec.LookPath(cfg.FFmpeg); err != nil {
		fatal(fmt.Errorf("ffmpeg not found (%s) — install it on the server, or set the plugin's FFmpeg setting in the admin panel", cfg.FFmpeg))
	}
	c, err := chriscordbot.New(chriscordbot.Options{
		Server: cfg.ServerURL, Token: cfg.Token, Client: "chriscord-music-plugin/" + version, Logf: logf,
		// The server's signed member list, remembered between runs: who it
		// may give its keys to (the SDK's rostersync.go).
		RosterFile: filepath.Join(cfg.DataDir, "roster.json"),
	})
	if err != nil {
		fatal(fmt.Errorf("the identity the server gave it: %w", err))
	}
	y := newYtdlp(cfg)
	go y.updateLoop()

	logf("chriscord-music-plugin %s starting: %s as the bot %q (commands start with %q)", version, cfg.ServerURL, c.Name(), cfg.Prefix)
	b := newBot(cfg, c, y)
	go b.pl.loop()
	go b.ui.loop()
	c.Run(context.Background())
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "chriscord-music-plugin:", err)
	os.Exit(1)
}

func randRead(b []byte) (int, error) { return io.ReadFull(rand.Reader, b) }

func logf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, time.Now().Format("2006-01-02 15:04:05 ")+format+"\n", args...)
}
