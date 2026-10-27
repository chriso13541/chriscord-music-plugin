package main

// commands.go — what people can ask for, however they ask:
//
//   - typed in the message box as /music play <song> (the app suggests the
//     commands and sends them straight to the plugin: pluginui.go);
//   - clicked in the Music panel (pluginui.go);
//   - typed as a chat message, !play <song> — only with the "Text commands"
//     setting on.
//
// Anyone who can see a text channel the plugin is in can use them there;
// the ones that change what's playing need you to be in its voice channel
// (like Discord music bots).

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	chriscordbot "github.com/chriso13541/chriscord-bot-sdk"
)

// Symbols at the start of replies. Plain text characters, drawn in the
// chat's own font and colour: the emoji versions (⏭ ⏸ ⏹) come out as dark
// pictures that disappear on dark backgrounds.
const (
	iconPlay   = "▶"
	iconPause  = "||"
	iconSkip   = ">>"
	iconStop   = "■"
	iconQueued = "+"
)

const helpText = `Music commands (type them in the message box):
{p}play <link or words> — join your voice channel and play it, or queue it (playlists and mixes too)
{p}search <words> — the top 10 results, then {p}pick <number>
{p}pause / {p}resume — pause or carry on
{p}skip — next track
{p}remove <number> — take a track out of the queue
{p}stop — stop playing and empty the queue (stays in the call)
{p}clear — empty the queue (the current track keeps playing)
{p}queue — what's playing and what's next
{p}np — what's playing now
{p}autoplay [on|off] — when the queue runs out, keep playing songs like the ones asked for
{p}history — what's played lately
{p}leave — leave the call
Or use the Music panel in the plugins area.`

// req is one request, from whichever place it came.
type req struct {
	user  string
	board string // the text channel it came from ("" for a click in the panel)
	msg   *chriscordbot.Message
	// say tells the channel (what everyone should know: "Queued …");
	// nil when nobody but them needs to hear (a click in the panel).
	say func(string)
	// tell tells just them (problems, and answers only they asked for).
	tell func(string)
	// how commands are written, for hints: "/music " or "!"
	prefix string
}

// ok: something happened that the channel should hear about.
func (r req) ok(format string, a ...interface{}) {
	if r.say != nil {
		r.say(fmt.Sprintf(format, a...))
	}
}

// no: a problem, or an answer just for them.
func (r req) no(format string, a ...interface{}) { r.tell(fmt.Sprintf(format, a...)) }

// onChat: a chat message, which is a command only if it starts with the
// prefix and text commands are on.
func (b *Bot) onChat(m chatMessage) {
	if !b.cfg.TextCommands {
		return
	}
	text := strings.TrimSpace(m.Text)
	if !strings.HasPrefix(text, b.cfg.Prefix) {
		return
	}
	cmd, arg, _ := strings.Cut(strings.TrimPrefix(text, b.cfg.Prefix), " ")
	public := func(s string) { b.say(m.BoardID, s) }
	logf("chat: %s: %s%s %s", m.Username, b.cfg.Prefix, cmd, arg)
	b.run(strings.ToLower(strings.TrimSpace(cmd)), strings.TrimSpace(arg),
		req{user: m.Username, board: m.BoardID, msg: m.msg, say: public, tell: public, prefix: b.cfg.Prefix})
}

// run carries out one command.
func (b *Bot) run(cmd, arg string, r req) {
	switch cmd {
	case "play", "p", "add":
		b.cmdPlay(r, arg)
	case "search", "find", "yt":
		b.cmdSearch(r, arg)
	case "pick":
		if b.pendingSearch(r.user) == nil {
			r.no("Search first: %ssearch <words>", r.prefix)
		} else {
			b.cmdPlay(r, arg)
		}
	case "stop":
		if b.mustShareCall(r) {
			n := b.pl.clear()
			if t := b.pl.skip(); t != nil {
				r.ok(iconStop+" Stopped%s.", clearedNote(n))
			} else if n > 0 {
				r.ok(iconStop+" Cleared the queue (%d track%s).", n, plural(n))
			} else {
				r.no("Nothing's playing.")
			}
		}
	case "pause":
		if !b.mustShareCall(r) {
			break
		}
		if b.pl.setPaused(true) {
			r.ok(iconPause + " Paused.")
		} else if b.pl.view().current == nil {
			r.no("Nothing's playing.")
		} else {
			r.no("Already paused.")
		}
	case "resume", "unpause", "continue":
		if b.mustShareCall(r) {
			if b.pl.setPaused(false) {
				r.ok(iconPlay + " Resumed.")
			} else {
				r.no("Not paused.")
			}
		}
	case "toggle": // the panel's play/pause button
		if b.mustShareCall(r) {
			v := b.pl.view()
			switch {
			case v.current == nil:
				r.no("Nothing's playing.")
			case v.paused:
				b.pl.setPaused(false)
			default:
				b.pl.setPaused(true)
			}
		}
	case "skip", "s", "next":
		if b.mustShareCall(r) {
			if t := b.pl.skip(); t != nil {
				r.ok(iconSkip+" Skipped %s.", t.Title)
			} else {
				r.no("Nothing's playing.")
			}
		}
	case "remove", "rm":
		if b.mustShareCall(r) {
			n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(arg), "#"))
			if err != nil {
				r.no("Which one? %sremove <number in the queue>", r.prefix)
				break
			}
			if t := b.pl.remove(n); t != nil {
				r.ok("Took %s out of the queue.", t.Title)
			} else {
				r.no("There's no #%d in the queue.", n)
			}
		}
	case "clear":
		if b.mustShareCall(r) {
			n := b.pl.clear()
			r.ok("🧹 Cleared the queue (%d track%s).", n, plural(n))
		}
	case "disconnect", "dc", "leave":
		if b.mustShareCall(r) {
			if r.board != "" {
				b.mu.Lock()
				b.noticeTo = r.board
				b.mu.Unlock()
			}
			if r.say != nil {
				b.leave("👋 Left the call.")
			} else {
				b.leave("")
			}
		}
	case "queue", "q":
		r.no("%s", b.queueText(10))
	case "np", "nowplaying", "now":
		v := b.pl.view()
		if v.current == nil {
			r.no("Nothing's playing.")
		} else {
			r.no("%s", nowLine(v))
		}
	case "autoplay", "auto", "radio":
		on := !b.auto.enabled()
		switch strings.ToLower(strings.TrimSpace(arg)) {
		case "on", "yes", "1", "true":
			on = true
		case "off", "no", "0", "false":
			on = false
		case "":
		default:
			r.no("Usage: %sautoplay [on|off]", r.prefix)
			return
		}
		b.auto.set(on)
		if on {
			r.ok(iconPlay + " Autoplay's on: when the queue runs out, I'll keep playing songs like the ones you asked for.")
		} else {
			r.ok(iconStop + " Autoplay's off.")
		}
	case "history", "played", "recent":
		r.no("%s", b.historyText(15))
	case "version":
		r.no("chriscord-music-plugin %s", version)
	case "help", "commands", "music":
		r.no("%s", strings.ReplaceAll(helpText, "{p}", r.prefix))
	default:
		r.no("There's no %s%s — try %shelp", r.prefix, cmd, r.prefix)
	}
}

// mustShareCall: the command needs the person in the plugin's call.
func (b *Bot) mustShareCall(r req) bool {
	board, _ := b.inVoice()
	if board == "" {
		r.no("I'm not in a call.")
		return false
	}
	if b.voiceChannelOf(r.user) != board {
		r.no("You need to be in my voice channel for that.")
		return false
	}
	return true
}

func (b *Bot) cmdPlay(r req, arg string) {
	if arg == "" {
		if b.pl.view().paused {
			if b.mustShareCall(r) && b.pl.setPaused(false) {
				r.ok(iconPlay + " Resumed.")
			}
		} else {
			r.no("Usage: %splay <link or search words>", r.prefix)
		}
		return
	}
	theirs := b.voiceChannelOf(r.user)
	if theirs == "" {
		// (It only sees the voice channels it was given when it was
		// installed, so someone in another one looks like they're in none.)
		r.no("Join a voice channel first, then ask again. (If you're in one, I haven't been given access to it — the server's owner can add it in the admin panel, under Plugins.)")
		return
	}
	mine, _ := b.inVoice()
	if mine != "" && mine != theirs {
		v := b.pl.view()
		if len(b.othersIn(mine)) > 0 && (v.current != nil || len(v.queue) > 0) {
			r.no("I'm busy in another voice channel right now.")
			return
		}
	}
	// Only joins a channel with someone in it (they're in it, so: unless
	// they've just left).
	if mine != theirs && len(b.othersIn(theirs)) == 0 {
		r.no("Nobody's in that voice channel.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var found resolved
	switch {
	case isPick(arg) && b.pendingSearch(r.user) != nil:
		// A number after a search: that result.
		res := b.pendingSearch(r.user)
		n, _ := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if n < 1 || n > len(res) {
			r.no("Pick 1–%d from the search.", len(res))
			return
		}
		found = resolved{tracks: []trackInfo{res[n-1]}, kind: linkVideo}
		b.forgetSearch(r.user)
	case strings.HasPrefix(strings.ToLower(arg), "search "):
		b.cmdSearch(r, arg[len("search "):])
		return
	case looksLikeURL(strings.Trim(arg, "<>")):
		var err error
		if found, err = b.ytdlp.resolve(ctx, arg, b.cfg.MaxPlaylist); err != nil {
			r.no("Couldn't play that — %v", friendlyError(err))
			return
		}
	default:
		// Words: the top YouTube result.
		res, err := b.ytdlp.search(ctx, arg, 1)
		if err != nil {
			r.no("Couldn't find anything for %q — %v", arg, err)
			return
		}
		found = resolved{tracks: res, kind: linkVideo}
	}
	infos := found.tracks
	playlist := found.kind == linkPlaylist || found.kind == linkRadio
	var ts []*track
	skipped := found.skipped
	board := r.board
	if board == "" { // from the panel: where it was last asked to play
		b.mu.Lock()
		board = b.noticeTo
		b.mu.Unlock()
	}
	for _, in := range infos {
		ts = append(ts, &track{trackInfo: in, requester: r.user, board: board})
	}
	if len(ts) == 0 {
		r.no("Nothing to play there.")
		return
	}

	// Earlier !plays queue first, even if this lookup was quicker.
	if r.msg != nil {
		b.c.WaitTurn(r.msg)
	}

	// Into the call, if not there already (one play at a time decides
	// that, so two at once don't both join).
	b.joinMu.Lock()
	mine, _ = b.inVoice()
	if mine != theirs {
		if len(b.othersIn(theirs)) == 0 {
			b.joinMu.Unlock()
			r.no("Nobody's in that voice channel any more.")
			return
		}
		if mine != "" {
			b.pl.stopAll()
		}
		if err := b.joinVoice(theirs); err != nil {
			b.joinMu.Unlock()
			r.no("Couldn't join the call — %v", err)
			return
		}
	}
	b.joinMu.Unlock()
	if r.board != "" {
		b.mu.Lock()
		b.noticeTo = r.board
		b.mu.Unlock()
	}

	b.pl.dropAuto() // what someone asks for comes before autoplay's picks
	pos, added := b.pl.enqueue(ts)
	switch {
	case added == 0:
		r.no("The queue's full (%d tracks).", b.cfg.MaxQueue)
	case playlist:
		what := "the playlist"
		if found.kind == linkRadio {
			what = "the mix"
		}
		if found.title != "" {
			what += " “" + found.title + "”"
		}
		var notes []string
		if skipped > 0 {
			notes = append(notes, fmt.Sprintf("%d unavailable", skipped))
		}
		if added < len(ts) {
			notes = append(notes, fmt.Sprintf("only %d fit in the queue", added))
		}
		note := ""
		if len(notes) > 0 {
			note = " (skipped " + strings.Join(notes, ", ") + ")"
			note = strings.Replace(note, "skipped only", "only", 1)
		}
		r.ok(iconQueued+" Queued %d track%s from %s%s.", added, plural(added), what, note)
	case pos == 0:
		// Starting now: "Now playing" follows when it does.
	default:
		r.ok(iconQueued+" Queued %s%s — #%d in line.", ts[0].Title, lengthNote(ts[0].trackInfo), pos)
	}
}

func (b *Bot) queueText(limit int) string {
	v := b.pl.view()
	if v.current == nil && len(v.queue) == 0 {
		return "The queue's empty."
	}
	var sb strings.Builder
	if v.current != nil {
		sb.WriteString(nowLine(v))
	}
	if len(v.queue) == 0 {
		sb.WriteString("\nNothing up next.")
		return sb.String()
	}
	sb.WriteString("\nUp next:")
	var total time.Duration
	for i, t := range v.queue {
		total += t.Duration
		if i < limit {
			fmt.Fprintf(&sb, "\n%d. %s%s — %s", i+1, t.Title, lengthNote(t.trackInfo), t.requester)
		}
	}
	if len(v.queue) > limit {
		fmt.Fprintf(&sb, "\n…and %d more", len(v.queue)-limit)
	}
	fmt.Fprintf(&sb, "\n%d track%s", len(v.queue), plural(len(v.queue)))
	if total > 0 {
		fmt.Fprintf(&sb, ", %s in all", clockTime(total))
	}
	return sb.String()
}

func nowLine(v queueView) string {
	state := iconPlay + " Now playing"
	if v.paused {
		state = iconPause + " Paused"
	}
	pos := clockTime(v.elapsed)
	if v.current.Live {
		pos = "🔴 live, " + pos
	} else if v.current.Duration > 0 {
		pos += " / " + clockTime(v.current.Duration)
	}
	return fmt.Sprintf("%s: %s [%s] — asked for by %s", state, v.current.Title, pos, v.current.requester)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func clearedNote(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(" and cleared the queue (%d track%s)", n, plural(n))
}

// ── Search ────────────────────────────────────────────────────────────────────

const (
	searchResults = 10
	searchKeep    = 10 * time.Minute // how long "pick <number>" picks from a search
)

type searchResult struct {
	tracks []trackInfo
	at     time.Time
}

func isPick(s string) bool {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	return err == nil && n >= 0 && n < 100
}

func (b *Bot) pendingSearch(user string) []trackInfo {
	b.mu.Lock()
	defer b.mu.Unlock()
	if r, ok := b.searches[user]; ok && time.Since(r.at) < searchKeep {
		return r.tracks
	}
	return nil
}

func (b *Bot) forgetSearch(user string) {
	b.mu.Lock()
	delete(b.searches, user)
	b.mu.Unlock()
}

func (b *Bot) cmdSearch(r req, words string) {
	words = strings.TrimSpace(words)
	if words == "" {
		r.no("Usage: %ssearch <words>", r.prefix)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := b.ytdlp.search(ctx, words, searchResults)
	if err != nil {
		logf("search: %q: %v", words, err)
		r.no("Couldn't search for %q — %v", words, err)
		return
	}
	logf("search: %q: %d results", words, len(res))
	b.mu.Lock()
	b.searches[r.user] = searchResult{tracks: res, at: time.Now()}
	b.mu.Unlock()
	var sb strings.Builder
	fmt.Fprintf(&sb, "🔎 Results for %q — %spick <number> to play one:", words, r.prefix)
	for i, t := range res {
		fmt.Fprintf(&sb, "\n%d. %s", i+1, t.Title)
		if t.Uploader != "" {
			sb.WriteString(" — " + t.Uploader)
		}
		sb.WriteString(lengthNote(t))
	}
	r.no("%s", sb.String())
}
