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
	"encoding/json"
	"fmt"
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
	"play": "query", "search": "words", "pick": "number", "remove": "number",
}

// onEvent: everything from the server; the plugin's own events handled here.
func (b *Bot) onEvent(e chriscordbot.Event) {
	switch e.Type {
	case "plugin_command", "plugin_action":
	default:
		return
	}
	var ev uiEvent
	if err := json.Unmarshal(e.Raw, &ev); err != nil || ev.ID == "" || ev.User == "" {
		return
	}
	tell := func(text string) { b.reply(ev.ID, text) }
	if ev.Type == "plugin_command" {
		cmd := strings.ToLower(ev.Command)
		arg := strings.TrimSpace(ev.Args[commandArg[cmd]])
		logf("app: %s: /%s %s %s", ev.User, b.cfg.PluginID, cmd, arg)
		// What everyone should know goes in the channel it was typed in,
		// with who asked; problems and answers only go to them.
		board := ev.BoardID
		say := func(text string) { b.say(board, text+" — "+ev.User) }
		b.run(cmd, arg, req{user: ev.User, board: board, say: say, tell: tell, prefix: "/" + b.cfg.PluginID + " "})
	} else {
		value := ""
		if ev.Value != nil {
			value = strings.TrimSpace(*ev.Value)
		}
		logf("panel: %s: %s %s", ev.User, ev.Action, value)
		b.action(ev.Action, value, req{user: ev.User, tell: tell, prefix: "/" + b.cfg.PluginID + " "})
	}
	b.ui.poke()
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
		b.run(cmd, arg, req{user: who, board: board, say: say, tell: tell, prefix: "/" + b.cfg.PluginID + " "})
	case "action":
		logf("panel: %s: %s %s", who, m.Action, m.Value)
		b.action(m.Action, strings.TrimSpace(m.Value), req{user: who, tell: tell, prefix: "/" + b.cfg.PluginID + " "})
	default:
		return // a ping
	}
	b.ui.poke()
}

// action: a click in the panel.
func (b *Bot) action(action, value string, r req) {
	switch action {
	case "add":
		b.run("play", value, r)
	case "toggle", "skip", "stop", "clear", "leave", "remove":
		b.run(action, value, r)
	default:
		r.tell("That button doesn't do anything any more — the panel may be out of date.")
	}
}

// reply answers a command or click, just to the person who sent it.
func (b *Bot) reply(id, text string) {
	if err := b.c.SendRaw(map[string]interface{}{"type": "plugin_reply", "to": id, "content": text}); err != nil {
		logf("app: replying: %v", err)
	}
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
}

func newPanelUI(b *Bot) *panelUI { return &panelUI{b: b, wake: make(chan struct{}, 1)} }

// poke: something changed; show it soon.
func (u *panelUI) poke() {
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

// reconnected: the server forgot the panel when the connection dropped.
func (u *panelUI) reconnected() {
	u.mu.Lock()
	u.sig, u.board, u.connected = "", "", true
	u.mu.Unlock()
	u.poke()
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
		if err := s.Send(map[string]interface{}{"t": "panel", "board": board, "panel": panel}); err != nil {
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
	s.Send(map[string]interface{}{"t": "panel", "board": board, "panel": panel})
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
