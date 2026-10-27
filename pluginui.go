package main

// pluginui.go — the plugin's part of the app: the commands people type in
// the message box (/music play …) and the Music panel in the plugins area.
//
// Both come on lanes (the SDK's OnLaneOpen and OnLane): each member's app
// opens one to the plugin, end-to-end encrypted, so the server passes them
// along without reading them:
//
//	app → plugin: {"t": "command", "id", "command", "args": {name: text}, "board"}
//	              {"t": "action",  "id", "action", "value", "board"}
//	plugin → app: {"t": "reply", "to": id, "content", "error"?}   (only they see it)
//	              {"t": "panel", "board", "panel"}                 (null: no panel)
//	              {"t": "notice", "board", "text"}
//
// An app too old for lanes sends them to the server in the clear instead
// (plugin_command, plugin_action: OnEvent), answered with plugin_reply; its
// panel only comes on lanes.
//
// The panel is a description the app draws in its own style (plugins never
// run code in the app): blocks of "title", "progress", "buttons", "input",
// "list" and "text". While the plugin's in a call the panel is about that
// voice channel (only people who can see it get it); otherwise there's a
// short one for everyone.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	chriscordbot "github.com/chriso13541/chriscord-bot-sdk"
)

// uiEvent is a command or a click, from the server.
type uiEvent struct {
	Type    string            `json:"type"`
	ID      string            `json:"id"`
	User    string            `json:"user"`
	BoardID string            `json:"board_id"`
	Command string            `json:"command"`
	Args    map[string]string `json:"args"`
	Action  string            `json:"action"`
	Value   *string           `json:"value"`
}

// The arguments each command takes, by name (as in plugin.json).
var commandArg = map[string]string{
	"play": "query", "search": "words", "pick": "number", "remove": "number", "autoplay": "state",
}

// onEvent: everything from the server. Commands and clicks only come on
// lanes (end-to-end encrypted, signed by the member: below) — this plugin
// takes them (plugin.json "lanes"), so one that arrives in the clear isn't
// from a member's app: the server made it up, or was told the plugin has no
// lanes to get the member's app to send it in the clear. Ignored, and never
// answered in the clear.
func (b *Bot) onEvent(e chriscordbot.Event) {
	switch e.Type {
	case "plugin_command", "plugin_action":
		var ev uiEvent
		_ = json.Unmarshal(e.Raw, &ev)
		logf("app: ignored a %s that came in the clear (from %q, says the server) — this plugin only takes them end-to-end encrypted", e.Type, ev.User)
	}
}

// ── Lanes ─────────────────────────────────────────────────────────────────────

// laneMsg is a command or a click, on a lane.
type laneMsg struct {
	T       string            `json:"t"`
	ID      string            `json:"id"`
	Command string            `json:"command"`
	Args    map[string]string `json:"args"`
	Action  string            `json:"action"`
	Value   string            `json:"value"`
	Board   string            `json:"board"`
}

// onLaneOpen: someone's app opened a lane — their panel.
func (b *Bot) onLaneOpen(s *chriscordbot.LaneSession) {
	logf("app: %s opened a lane", s.Name)
	b.ui.sendTo(s)
}

// onLane: a command or click from someone's app.
func (b *Bot) onLane(s *chriscordbot.LaneSession, raw json.RawMessage) {
	var m laneMsg
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	who := s.Name
	tell := func(text string) { s.Send(map[string]interface{}{"t": "reply", "to": m.ID, "content": text}) }
	// Its turn: what's asked for is queued in the order it was asked,
	// however long each takes to look up (see turns).
	var turn uint64
	if m.T == "command" || m.T == "action" {
		turn = b.turns.take()
		defer b.turns.finish(turn)
	}
	// A channel they named has to be one they can read (it's where answers
	// everyone should see go).
	if m.Board != "" && !s.MayRead(m.Board) {
		tell("You can't see that channel.")
		return
	}
	switch m.T {
	case "command":
		cmd := strings.ToLower(m.Command)
		arg := strings.TrimSpace(m.Args[commandArg[cmd]])
		logf("app: %s: /%s %s %s", who, b.cfg.PluginID, cmd, arg)
		board := m.Board
		// What everyone should know is announced in the channel it was
		// typed in, with who asked.
		say := func(text string) {
			if board == "" {
				tell(text)
				return
			}
			b.say(board, text+" — "+who)
		}
		b.run(cmd, arg, req{user: who, board: board, say: say, tell: tell, prefix: "/" + b.cfg.PluginID + " ", turn: turn})
	case "action":
		logf("panel: %s: %s %s", who, m.Action, m.Value)
		b.action(m.Action, strings.TrimSpace(m.Value), req{user: who, tell: tell, prefix: "/" + b.cfg.PluginID + " ", turn: turn})
	default:
		return // a ping
	}
	b.ui.poke()
}

// panelResults is how many search results the panel offers.
const panelResults = 5

// action: a click in the panel.
func (b *Bot) action(action, value string, r req) {
	switch action {
	case "add":
		// A link plays; words show the top results, for them to choose from.
		if looksLikeURL(strings.Trim(value, "<>")) || isPick(value) {
			b.ui.setResults(r.user, nil, "")
			b.run("play", value, r)
			return
		}
		b.panelSearch(r, value)
	case "pick": // one of their results
		if b.pendingSearch(r.user) == nil {
			b.ui.setResults(r.user, nil, "")
			r.tell("Those results have expired — search again.")
			return
		}
		b.ui.setResults(r.user, nil, "")
		b.run("play", value, r)
	case "close_results":
		b.ui.setResults(r.user, nil, "")
	case "autoplay": // the panel's toggle
		b.run("autoplay", "", req{user: r.user, board: b.noticeBoard(), say: func(s string) { b.say(b.noticeBoard(), s+" — "+r.user) }, tell: r.tell, prefix: r.prefix})
	case "toggle", "skip", "stop", "clear", "leave", "remove":
		b.run(action, value, r)
	default:
		r.tell("That button doesn't do anything any more — the panel may be out of date.")
	}
}

// panelSearch: words typed in the panel's box — the top results go in
// their panel (only theirs), each with a button to play it.
func (b *Bot) panelSearch(r req, words string) {
	if words == "" {
		return
	}
	b.ui.setResults(r.user, nil, "Searching for “"+words+"”…")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := b.ytdlp.search(ctx, words, panelResults)
	if err != nil {
		logf("search: %q: %v", words, err)
		b.ui.setResults(r.user, nil, "")
		r.tell(fmt.Sprintf("Couldn't search for %q — %v", words, err))
		return
	}
	if len(res) == 0 {
		b.ui.setResults(r.user, nil, "")
		r.tell(fmt.Sprintf("Nothing found for %q.", words))
		return
	}
	// Kept like /music search's, so "pick" (and /music pick) play from them.
	b.mu.Lock()
	b.searches[r.user] = searchResult{tracks: res, at: time.Now()}
	b.mu.Unlock()
	b.ui.setResults(r.user, res, "Results for “"+words+"”")
}

// ── The panel ─────────────────────────────────────────────────────────────────

type panelUI struct {
	b    *Bot
	wake chan struct{}

	mu        sync.Mutex
	board     string // the channel the shown panel is about ("" for everyone)
	sig       string // what was last shown, timings left out
	sentAt    time.Time
	elapsed   time.Duration
	running   bool
	connected bool
	inCall    string // who's in its call, as last seen (callChanged)
	// Search results in someone's panel (theirs alone), by their name.
	results map[string]panelSearchView
}

type panelSearchView struct {
	title  string
	tracks []trackInfo // none: still searching (title says so)
	at     time.Time
}

func newPanelUI(b *Bot) *panelUI { return &panelUI{b: b, wake: make(chan struct{}, 1)} }

// poke: something changed; show it soon.
func (u *panelUI) poke() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

// setResults shows (tracks or a "searching" title) or takes away (both
// empty) search results in user's panel, and sends them their panel now.
func (u *panelUI) setResults(user string, tracks []trackInfo, title string) {
	u.mu.Lock()
	if u.results == nil {
		u.results = map[string]panelSearchView{}
	}
	_, had := u.results[user]
	if title == "" && len(tracks) == 0 {
		delete(u.results, user)
	} else {
		u.results[user] = panelSearchView{title: title, tracks: tracks, at: time.Now()}
	}
	if had || title != "" {
		u.sig = "" // send the panel again (each person's with their own results)
	}
	u.mu.Unlock()
	u.poke()
}

// forUser is panel with user's search results in it (after the box), if
// they have any; panel itself isn't changed.
func (u *panelUI) forUser(panel map[string]interface{}, user string) map[string]interface{} {
	if panel == nil {
		return nil
	}
	u.mu.Lock()
	r, ok := u.results[user]
	if ok && time.Since(r.at) > searchKeep {
		delete(u.results, user)
		ok = false
	}
	u.mu.Unlock()
	if !ok {
		return panel
	}
	var items []interface{}
	for i, t := range r.tracks {
		sub := t.Uploader
		if t.Live {
			sub = strings.TrimPrefix(sub+" · live", " · ")
		} else if t.Duration > 0 {
			sub = strings.TrimPrefix(sub+" · "+clockTime(t.Duration), " · ")
		}
		items = append(items, map[string]interface{}{"title": t.Title, "sub": sub, "actions": []interface{}{
			map[string]interface{}{"action": "pick", "value": strconv.Itoa(i + 1), "icon": "play_arrow", "label": "Play this"},
		}})
	}
	results := map[string]interface{}{"type": "list", "title": r.title, "empty": "Searching…", "items": items}
	close := map[string]interface{}{"type": "buttons", "buttons": []interface{}{
		map[string]interface{}{"action": "close_results", "icon": "close", "label": "Close results"},
	}}
	blocks, _ := panel["blocks"].([]interface{})
	out := make([]interface{}, 0, len(blocks)+2)
	placed := false
	for _, bl := range blocks {
		out = append(out, bl)
		if m, _ := bl.(map[string]interface{}); !placed && m != nil && m["type"] == "input" {
			out = append(out, results, close)
			placed = true
		}
	}
	if !placed {
		out = append(out, results, close)
	}
	cp := map[string]interface{}{}
	for k, v := range panel {
		cp[k] = v
	}
	cp["blocks"] = out
	return cp
}

// reconnected: the server forgot the panel when the connection dropped.
func (u *panelUI) reconnected() {
	u.mu.Lock()
	u.sig, u.board, u.connected = "", "", true
	u.mu.Unlock()
	u.poke()
}

// callChanged: voice_state came. If who's in its call changed, the panel
// goes to everyone again, even if it looks the same: someone who left and
// came back may have lost it on their side (and the panel only went out
// when it changed, so they'd see nothing playing while it plays).
func (u *panelUI) callChanged() {
	board, _ := u.b.inVoice()
	who := ""
	if board != "" {
		others := append([]string(nil), u.b.othersIn(board)...)
		sort.Strings(others)
		who = board + "|" + strings.Join(others, ",")
	}
	u.mu.Lock()
	changed := who != u.inCall
	u.inCall = who
	if changed {
		u.sig = ""
	}
	u.mu.Unlock()
	if changed {
		u.poke()
	}
}

// loop keeps the panel up to date: on a poke, and twice a second in case
// something changed without one (a track ending, someone leaving).
func (u *panelUI) loop() {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-u.wake:
		case <-t.C:
		}
		u.update()
	}
}

func (u *panelUI) update() {
	board, _ := u.b.inVoice()
	panel, progress, elapsed, running := u.b.panel(board != "")
	// What it shows, timings left out (they change all the time, and the
	// app moves the progress bar along by itself).
	sigData, _ := json.Marshal(panel)
	sig := string(sigData)
	if progress != nil {
		progress["elapsed_ms"] = elapsed.Milliseconds()
	}

	u.mu.Lock()
	if !u.connected {
		u.mu.Unlock()
		return
	}
	// The same as shown, and its progress bar still right (the app moves it
	// along by itself while it's playing): nothing to send.
	drift := elapsed - u.elapsed
	if running && u.running {
		drift -= time.Since(u.sentAt)
	}
	if sig == u.sig && board == u.board && drift.Abs() < 2*time.Second {
		u.mu.Unlock()
		return
	}
	old := u.board
	u.board, u.sig, u.sentAt, u.elapsed, u.running = board, sig, time.Now(), elapsed, running
	u.mu.Unlock()

	if old != board {
		u.send(old, nil) // the one about the other channel goes
	}
	u.send(board, panel)
}

// send: the panel to everyone with a lane open who may see it (a panel
// about a voice channel: those who can read it).
func (u *panelUI) send(board string, panel map[string]interface{}) {
	for _, s := range u.b.c.LaneSessions() {
		if board != "" && !s.MayRead(board) {
			continue
		}
		if err := s.Send(map[string]interface{}{"t": "panel", "board": board, "panel": u.forUser(panel, s.Name)}); err != nil {
			logf("panel: %v", err)
			u.mu.Lock()
			u.sig = "" // try again next time
			u.mu.Unlock()
			return
		}
	}
}

// sendTo: the panel now, to one lane just opened.
func (u *panelUI) sendTo(s *chriscordbot.LaneSession) {
	board, _ := u.b.inVoice()
	panel, progress, elapsed, _ := u.b.panel(board != "")
	if progress != nil {
		progress["elapsed_ms"] = elapsed.Milliseconds()
	}
	if board != "" && !s.MayRead(board) {
		return
	}
	s.Send(map[string]interface{}{"t": "panel", "board": board, "panel": u.forUser(panel, s.Name)})
}

// panel describes the Music panel now (its progress block without the
// time into the track, which is returned alongside, and whether it's moving).
func (b *Bot) panel(inCall bool) (panel, progress map[string]interface{}, elapsed time.Duration, running bool) {
	add := map[string]interface{}{"type": "input", "action": "add", "placeholder": "Paste a link, or search YouTube…", "button": "Add"}
	if !inCall {
		return map[string]interface{}{"blocks": []interface{}{
			map[string]interface{}{"type": "text", "style": "muted",
				"text": "Not in a call. Join a voice channel it can play in, then add something — or type /" + b.cfg.PluginID + " play in a channel."},
			add,
		}}, nil, 0, false
	}
	v := b.pl.view()
	var blocks []interface{}
	if v.current == nil {
		blocks = append(blocks, map[string]interface{}{"type": "text", "style": "muted", "text": "Nothing's playing. Add something below."})
	} else {
		t := v.current
		sub := "Asked for by " + t.requester
		if t.Uploader != "" {
			sub = t.Uploader + " · " + sub
		}
		running = !v.paused
		progress = map[string]interface{}{"type": "progress", "duration_ms": t.Duration.Milliseconds(), "running": running, "live": t.Live}
		blocks = append(blocks, map[string]interface{}{"type": "title", "text": t.Title, "sub": sub, "link": t.URL}, progress)
	}
	playIcon, playLabel := "pause", "Pause"
	if v.paused {
		playIcon, playLabel = "play_arrow", "Resume"
	}
	nothing := v.current == nil
	blocks = append(blocks, map[string]interface{}{"type": "buttons", "buttons": []interface{}{
		map[string]interface{}{"action": "toggle", "icon": playIcon, "label": playLabel, "style": "primary", "disabled": nothing},
		map[string]interface{}{"action": "skip", "icon": "skip_next", "label": "Skip", "disabled": nothing},
		map[string]interface{}{"action": "stop", "icon": "stop", "label": "Stop", "disabled": nothing && len(v.queue) == 0},
		autoplayButton(b.auto.enabled()),
		map[string]interface{}{"action": "leave", "icon": "call_end", "label": "Leave the call", "style": "danger"},
	}}, add)

	const shown = 25
	var items []interface{}
	var total time.Duration
	for i, t := range v.queue {
		total += t.Duration
		if i >= shown {
			continue
		}
		sub := t.requester
		if t.auto {
			sub = "autoplay"
		}
		if t.Live {
			sub = "live · " + sub
		} else if t.Duration > 0 {
			sub = clockTime(t.Duration) + " · " + sub
		}
		items = append(items, map[string]interface{}{"title": t.Title, "sub": sub, "actions": []interface{}{
			map[string]interface{}{"action": "remove", "value": strconv.Itoa(i + 1), "icon": "close", "label": "Remove from the queue"},
		}})
	}
	title := "Up next"
	if n := len(v.queue); n > 0 {
		title = fmt.Sprintf("Up next · %d track%s", n, plural(n))
		if total > 0 {
			title += " · " + clockTime(total)
		}
	}
	list := map[string]interface{}{"type": "list", "title": title, "empty": "Nothing queued.", "items": items}
	if more := len(v.queue) - shown; more > 0 {
		list["more"] = fmt.Sprintf("…and %d more", more)
	}
	blocks = append(blocks, list)
	return map[string]interface{}{"blocks": blocks}, progress, v.elapsed, running
}

// autoplayButton: the panel's autoplay toggle — highlighted while it's on
// (the app's "on" style; an older app shows it plain, its label says).
func autoplayButton(on bool) map[string]interface{} {
	btn := map[string]interface{}{"action": "autoplay", "icon": "sync", "label": "Autoplay is off — turn on to keep playing songs like these when the queue runs out"}
	if on {
		btn["label"], btn["style"] = "Autoplay is on — turn off", "on"
	}
	return btn
}

// noticeBoard: the text channel it says things in (where it was last asked
// to play).
func (b *Bot) noticeBoard() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.noticeTo
}
