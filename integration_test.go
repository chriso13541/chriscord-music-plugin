//go:build integration

package main

// integration_test.go — runs the real bot against a stand-in for the
// Chriscord server (the parts the bot uses: sign-in, channel keys, the
// WebSocket, voice signalling with a real WebRTC peer, key relay), with a
// pretend member "alice" who types commands and listens. Checks that what
// reaches the call is the bot's audio, decryptable with the key it gave
// alice, at real-time pace; and the commands, the queue, and leaving when
// alone.
//
//	go test -tags integration -v -run TestBot
//
// Needs ffmpeg and yt-dlp (BOT_YTDLP, or it fetches one into a temp dir).

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	chriscordbot "github.com/chriso13541/chriscord-bot-sdk"
	"github.com/gorilla/websocket"
	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)

// ── The stand-in server ───────────────────────────────────────────────────────

type mockUser struct {
	name, pub   string
	encKey, sig *string
	conn        *websocket.Conn
	wmu         sync.Mutex
	local       func(map[string]interface{}) // a pretend member's inbox (alice)
}

type boxRec struct {
	gen                    int
	recipient, sender, box string
}

type mock struct {
	t      *testing.T
	mu     sync.Mutex
	users  map[string]*mockUser // by name
	tokens map[string]string    // token → name
	nonces map[string]string
	voice  map[string]string // user → board
	latest map[string]int
	boxes  map[string][]boxRec // board → boxes
	pcs    map[string]*webrtc.PeerConnection
	tap    chan []byte // RTP payloads the bot sends
	vtap   chan []byte // the bot's picture: access units, as sent (sealed)
	screen []bool      // its voice_screen messages
	spk    []bool
	status []string // the bot's presence blobs, in order
	pfp    []byte   // the bot's uploaded picture
	pfpAt  float64
	srv    *httptest.Server
	ui     chan map[string]interface{} // replies and panels from the plugin, from alice's lane
	// alice's lane to the plugin (her app's end)
	alicePriv ed25519.PrivateKey
	lane      *laneKeys
	// the signed member list (made once both are signed in)
	listOnce sync.Once
	list     map[string]interface{}
}

func newMock(t *testing.T) *mock {
	m := &mock{t: t, users: map[string]*mockUser{}, tokens: map[string]string{}, nonces: map[string]string{},
		voice: map[string]string{}, latest: map[string]int{}, boxes: map[string][]boxRec{},
		pcs: map[string]*webrtc.PeerConnection{}, tap: make(chan []byte, 100000), vtap: make(chan []byte, 1000), ui: make(chan map[string]interface{}, 10000)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/challenge", m.challenge)
	mux.HandleFunc("POST /api/join", m.join)
	mux.HandleFunc("PUT /api/e2ee/enc-key", m.encKey)
	mux.HandleFunc("GET /api/boards/{b}/keys", m.getKeys)
	mux.HandleFunc("POST /api/boards/{b}/keys", m.postKeys)
	mux.HandleFunc("GET /api/e2ee/pending", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"pending":[]}`)) })
	mux.HandleFunc("POST /api/e2ee/boxes", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("GET /api/roster", m.roster)
	mux.HandleFunc("/ws", m.ws)
	m.srv = httptest.NewServer(mux)
	return m
}

// roster: the server's signed member list (required): its owner's, with
// alice, and the plugin — which can read #text-1 (for !play typed in chat)
// but, being a plugin, never hears calls.
func (m *mock) roster(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	music, alice := m.users["Music"], m.users["alice"]
	m.mu.Unlock()
	if music == nil || alice == nil {
		jsonOut(w, map[string]interface{}{"id": "", "entries": []interface{}{}, "more": false})
		return
	}
	m.listOnce.Do(func() {
		_, owner, _ := ed25519.GenerateKey(rand.Reader)
		ownerKey := hex.EncodeToString(owner.Public().(ed25519.PublicKey))
		body, _ := json.Marshal(map[string]interface{}{
			"v": 1, "id": "0123456789abcdef0123456789abcdef", "at": time.Now().Unix(), "by": ownerKey, "op": "start",
			"policy":  map[string]string{"approvers": "owner", "need": "1"},
			"members": []map[string]string{{"key": strings.ToLower(alice.pub), "kind": "member"}, {"key": strings.ToLower(music.pub), "kind": "plugin"}},
			"access": []map[string]interface{}{
				{"board": "text-1", "all": true, "readers": []string{strings.ToLower(music.pub)}},
				{"board": "voice-1", "all": true}, {"board": "voice-2", "all": true},
			},
		})
		sig := ed25519.Sign(owner, append([]byte("chriscord-roster-v1\x00"), body...))
		m.list = map[string]interface{}{"body": string(body), "sigs": []map[string]string{{"key": ownerKey, "sig": hex.EncodeToString(sig)}}, "seq": 1}
	})
	after := r.URL.Query().Get("after")
	entries := []interface{}{m.list}
	if after != "" && after != "0" {
		entries = []interface{}{}
	}
	jsonOut(w, map[string]interface{}{"id": "0123456789abcdef0123456789abcdef", "entries": entries, "more": false})
}

func jsonOut(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (m *mock) me(r *http.Request) *mockUser {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.users[m.tokens[r.Header.Get("X-Session-Token")]]
}

func (m *mock) challenge(w http.ResponseWriter, r *http.Request) {
	var b struct {
		PublicKey string `json:"public_key"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	n := make([]byte, 32)
	rand.Read(n)
	m.mu.Lock()
	m.nonces[hex.EncodeToString(n)] = b.PublicKey
	m.mu.Unlock()
	jsonOut(w, map[string]string{"nonce": hex.EncodeToString(n)})
}

// join takes bots only, as the real server does for an added bot: with a
// certificate from its maker for the key that signed the challenge.
func (m *mock) join(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Username, Nonce, Signature, Client string
		PublicKey                          string                `json:"public_key"`
		Bot                                *chriscordbot.BotCert `json:"bot"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	if b.Bot == nil || b.Bot.Check() != nil || b.Bot.BotKey != b.PublicKey || !strings.HasPrefix(b.Client, "chriscord-music-plugin/") {
		w.WriteHeader(401)
		jsonOut(w, map[string]string{"error": "not a bot this server has added"})
		return
	}
	b.Username = "Music"
	pk, _ := hex.DecodeString(b.PublicKey)
	n, _ := hex.DecodeString(b.Nonce)
	sig, _ := hex.DecodeString(b.Signature)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.nonces[b.Nonce] != b.PublicKey || !ed25519.Verify(pk, n, sig) {
		w.WriteHeader(401)
		jsonOut(w, map[string]string{"error": "bad signature"})
		return
	}
	delete(m.nonces, b.Nonce)
	if m.users[b.Username] == nil {
		m.users[b.Username] = &mockUser{name: b.Username, pub: b.PublicKey}
	}
	tok := hex.EncodeToString(n[:16])
	m.tokens[tok] = b.Username
	jsonOut(w, map[string]interface{}{"token": tok, "username": b.Username, "encrypted_presence": true, "bot": true})
}

func (m *mock) addLocal(name string, priv ed25519.PrivateKey, inbox func(map[string]interface{})) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.users[name] = &mockUser{name: name, pub: hex.EncodeToString(priv.Public().(ed25519.PublicKey)), local: inbox}
	tok := "tok-" + name
	m.tokens[tok] = name
	return tok
}

func (m *mock) encKey(w http.ResponseWriter, r *http.Request) {
	u := m.me(r)
	var b map[string]string
	json.NewDecoder(r.Body).Decode(&b)
	k, s := b["enc_key"], b["sig"]
	m.mu.Lock()
	u.encKey, u.sig = &k, &s
	m.mu.Unlock()
	m.broadcast(map[string]interface{}{"type": "e2ee_changed"})
	w.Write([]byte(`{}`))
}

func (m *mock) memberLocked(name string) textMember {
	u := m.users[name]
	return textMember{Username: u.name, PublicKey: u.pub, EncKey: u.encKey, Sig: u.sig}
}

func (m *mock) getKeys(w http.ResponseWriter, r *http.Request) {
	u := m.me(r)
	board := r.PathValue("b")
	m.mu.Lock()
	defer m.mu.Unlock()
	resp := map[string]interface{}{"latest": m.latest[board], "rotate": false}
	var boxes []map[string]interface{}
	for _, b := range m.boxes[board] {
		if strings.EqualFold(b.recipient, u.pub) {
			boxes = append(boxes, map[string]interface{}{"generation": b.gen, "box": b.box, "sender": m.memberLocked(b.sender)})
		}
	}
	var members []textMember
	for n := range m.users {
		members = append(members, m.memberLocked(n))
	}
	resp["boxes"], resp["members"] = boxes, members
	jsonOut(w, resp)
}

func (m *mock) postKeys(w http.ResponseWriter, r *http.Request) {
	u := m.me(r)
	board := r.PathValue("b")
	var b struct {
		Generation int                               `json:"generation"`
		Boxes      []struct{ Recipient, Box string } `json:"boxes"`
	}
	json.NewDecoder(r.Body).Decode(&b)
	m.mu.Lock()
	if b.Generation != m.latest[board]+1 {
		m.mu.Unlock()
		w.WriteHeader(409)
		jsonOut(w, map[string]string{"error": "generation taken"})
		return
	}
	m.latest[board] = b.Generation
	for _, x := range b.Boxes {
		m.boxes[board] = append(m.boxes[board], boxRec{b.Generation, x.Recipient, u.name, x.Box})
	}
	m.mu.Unlock()
	w.Write([]byte(`{}`))
}

// send delivers to one user.
func (m *mock) send(u *mockUser, v map[string]interface{}) {
	if u.local != nil {
		go u.local(v)
		return
	}
	data, _ := json.Marshal(v)
	u.wmu.Lock() // (conn is changed holding both m.mu and wmu)
	if u.conn != nil {
		u.conn.WriteMessage(websocket.TextMessage, data)
	}
	u.wmu.Unlock()
}

func (m *mock) broadcast(v map[string]interface{}) {
	m.mu.Lock()
	us := make([]*mockUser, 0, len(m.users))
	for _, u := range m.users {
		us = append(us, u)
	}
	m.mu.Unlock()
	for _, u := range us {
		m.send(u, v)
	}
}

func (m *mock) to(name string, v map[string]interface{}) {
	m.mu.Lock()
	u := m.users[name]
	m.mu.Unlock()
	if u != nil {
		v["target"] = name
		m.send(u, v)
	}
}

func (m *mock) voiceState() {
	m.mu.Lock()
	ch := map[string][]string{}
	for u, b := range m.voice {
		ch[b] = append(ch[b], u)
	}
	m.mu.Unlock()
	m.broadcast(map[string]interface{}{"type": "voice_state", "channels": ch, "reconnecting": map[string]interface{}{}})
}

func (m *mock) ws(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	name := m.tokens[r.URL.Query().Get("token")]
	u := m.users[name]
	m.mu.Unlock()
	if u == nil {
		return
	}
	c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	m.mu.Lock()
	u.wmu.Lock()
	u.conn = c
	u.wmu.Unlock()
	m.mu.Unlock()
	m.voiceState()
	for {
		_, raw, err := c.ReadMessage()
		if err != nil {
			m.mu.Lock()
			delete(m.voice, name)
			u.wmu.Lock()
			if u.conn == c {
				u.conn = nil
			}
			u.wmu.Unlock()
			m.mu.Unlock()
			m.voiceState()
			return
		}
		var v map[string]interface{}
		json.Unmarshal(raw, &v)
		m.handle(name, v)
	}
}

// handle: one message from a member (the bot, or alice via sendAs).
func (m *mock) handle(name string, v map[string]interface{}) {
	str := func(k string) string { s, _ := v[k].(string); return s }
	switch str("type") {
	case "message":
		m.broadcast(map[string]interface{}{"type": "message", "data": map[string]interface{}{
			"id": fmt.Sprint(time.Now().UnixNano()), "board_id": str("board_id"), "username": name, "content": str("content"),
		}})
	case "join_voice":
		m.mu.Lock()
		m.voice[name] = str("board_id")
		m.mu.Unlock()
		m.voiceState()
	case "leave_voice":
		m.mu.Lock()
		delete(m.voice, name)
		pc := m.pcs[name]
		delete(m.pcs, name)
		m.mu.Unlock()
		if pc != nil {
			pc.Close()
		}
		m.voiceState()
	case "voice_e2ee":
		m.mu.Lock()
		board := m.voice[name]
		pub := m.users[name].pub
		var targets []string
		for u, b := range m.voice {
			if b == board && u != name && (str("to") == "" || str("to") == u) {
				targets = append(targets, u)
			}
		}
		m.mu.Unlock()
		for _, t := range targets {
			msg := map[string]interface{}{"type": "voice_e2ee", "board_id": board, "from": name, "from_key": pub, "data": v["data"]}
			if str("to") != "" {
				msg["to"] = str("to")
			}
			m.mu.Lock()
			u := m.users[t]
			m.mu.Unlock()
			m.send(u, msg)
		}
	case "voice_offer":
		go m.offer(name, str("sdp"))
	case "voice_ice":
		m.mu.Lock()
		pc := m.pcs[name]
		m.mu.Unlock()
		if pc != nil {
			idx := uint16(0)
			if f, ok := v["sdp_mline_index"].(float64); ok {
				idx = uint16(f)
			}
			mid := str("sdp_mid")
			pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: str("candidate"), SDPMid: &mid, SDPMLineIndex: &idx})
		}
	case "presence":
		if name == "Music" {
			m.mu.Lock()
			m.status = append(m.status, str("blob"))
			m.mu.Unlock()
		}
	case "pfp_info":
		m.mu.Lock()
		have := m.pfpAt
		m.mu.Unlock()
		if at, _ := v["pfp_updated_at"].(float64); at > 0 && at != have {
			m.to(name, map[string]interface{}{"type": "pfp_request"})
		}
	case "pfp_upload":
		data, _ := base64.StdEncoding.DecodeString(str("pfp_data"))
		m.mu.Lock()
		m.pfp, m.pfpAt = data, v["pfp_updated_at"].(float64)
		m.mu.Unlock()
	case "voice_screen":
		m.mu.Lock()
		m.screen = append(m.screen, v["screen"] == true)
		m.mu.Unlock()
	case "speaking":
		m.mu.Lock()
		m.spk = append(m.spk, v["speaking"] == true)
		m.mu.Unlock()
	case "plugin_reply", "plugin_panel":
		// In the clear: only for apps without lanes, so not here.
		m.t.Errorf("the plugin sent %s in the clear: %v", v["type"], v)
	case "plugin_lane", "plugin_lane_gone":
		if name != "Music" || str("to") != "alice" {
			m.t.Errorf("a lane message from %s to %s", name, str("to"))
			return
		}
		m.mu.Lock()
		k := m.lane
		if k == nil || k.SID != str("sid") {
			m.mu.Unlock()
			return
		}
		if v["type"] == "plugin_lane_gone" {
			m.lane = nil
			m.mu.Unlock()
			m.t.Logf("the plugin forgot alice's lane")
			return
		}
		n, _ := v["n"].(float64)
		plain, err := k.open(laneFromPlugin, uint64(n), str("data"))
		m.mu.Unlock()
		if err != nil {
			m.t.Errorf("a lane message didn't open: %v", err)
			return
		}
		// As the app shows them: plugin_reply and plugin_panel.
		var lm struct {
			T, To, Content, Text, Board string
			Panel                       json.RawMessage
			Public                      bool
		}
		json.Unmarshal(plain, &lm)
		if lm.T == "notice" && lm.Public {
			// An announcement in a channel: alice's app shows it there.
			m.mu.Lock()
			local := m.users["alice"].local
			m.mu.Unlock()
			local(map[string]interface{}{"type": "notice", "board_id": lm.Board, "text": lm.Text})
			return
		}
		out := map[string]interface{}{}
		switch lm.T {
		case "reply":
			out = map[string]interface{}{"type": "plugin_reply", "to": lm.To, "content": lm.Content}
			// Just for her, where she asked: alice reads it like the rest.
			m.mu.Lock()
			local := m.users["alice"].local
			m.mu.Unlock()
			local(map[string]interface{}{"type": "notice", "text": lm.Content})
		case "notice":
			out = map[string]interface{}{"type": "plugin_reply", "to": "", "content": lm.Text}
		case "panel":
			out = map[string]interface{}{"type": "plugin_panel", "data": lm.Panel}
			if lm.Board != "" {
				out["board_id"] = lm.Board
			}
		}
		select {
		case m.ui <- out:
		default:
		}
	}
}

// openLane: alice's app opens a lane to the plugin (sealed for its
// published encryption key, signed by her).
func (m *mock) openLane(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	u := m.users["Music"]
	pub, enc := u.pub, *u.encKey
	m.mu.Unlock()
	raw, _ := hex.DecodeString(enc)
	var encKey [32]byte
	copy(encKey[:], raw)
	k, eph, hello, err := laneOpenFromApp(pub, encKey, m.alicePriv, "alice")
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.lane = k
	m.mu.Unlock()
	m.to("Music", map[string]interface{}{"type": "plugin_lane", "from": "alice", "sid": k.SID, "n": 0, "eph": eph, "data": hello})
}

// app: alice using the plugin from the app, on her lane (opened again if
// the plugin forgot it).
var appSeq int

func (m *mock) app(kind string, fields map[string]interface{}) string {
	appSeq++
	id := fmt.Sprintf("req-%d", appSeq)
	m.mu.Lock()
	open := m.lane != nil
	m.mu.Unlock()
	if !open {
		m.openLane(m.t)
	}
	v := map[string]interface{}{"t": "command", "id": id}
	if kind == "plugin_action" {
		v["t"] = "action"
	}
	for k, x := range fields {
		if k == "board_id" {
			k = "board"
		}
		v[k] = x
	}
	plain, _ := json.Marshal(v)
	m.mu.Lock()
	n, data := m.lane.seal(laneToPlugin, plain)
	sid := m.lane.SID
	m.mu.Unlock()
	m.to("Music", map[string]interface{}{"type": "plugin_lane", "from": "alice", "sid": sid, "n": n, "data": data})
	return id
}

// expectUI waits for a plugin_reply to id containing want (id "": a
// plugin_panel whose JSON contains every one of want's "|"-separated parts),
// and returns it as JSON.
func (m *mock) expectUI(t *testing.T, id, want string, within time.Duration) string {
	t.Helper()
	end := time.After(within)
	var seen []string
	for {
		select {
		case v := <-m.ui:
			data, _ := json.Marshal(v)
			s := string(data)
			seen = append(seen, s)
			if id != "" && v["type"] == "plugin_reply" && v["to"] == id && strings.Contains(fmt.Sprint(v["content"]), want) {
				return s
			}
			if id == "" && v["type"] == "plugin_panel" {
				ok := true
				for _, part := range strings.Split(want, "|") {
					ok = ok && strings.Contains(s, part)
				}
				if ok {
					return s
				}
			}
		case <-end:
			if len(seen) > 6 {
				seen = seen[len(seen)-6:]
			}
			t.Fatalf("no %q from the plugin (reply to %q); last seen:\n%s", want, id, strings.Join(seen, "\n"))
		}
	}
}

func (m *mock) offer(name, sdp string) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		m.t.Errorf("mock pc: %v", err)
		return
	}
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if t.Kind() == webrtc.RTPCodecTypeVideo {
			// The bot's picture: H.264 access units, put back together.
			var depack codecs.H264Packet
			var au []byte
			for {
				p, _, err := t.ReadRTP()
				if err != nil {
					return
				}
				part, err := depack.Unmarshal(p.Payload)
				if err != nil {
					continue
				}
				au = append(au, part...)
				if p.Marker {
					select {
					case m.vtap <- au:
					default:
					}
					au = nil
				}
			}
		}
		for {
			p, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			m.tap <- append([]byte(nil), p.Payload...)
		}
	})
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		m.to(name, map[string]interface{}{"type": "voice_ice", "candidate": j.Candidate, "sdp_mid": j.SDPMid, "sdp_mline_index": j.SDPMLineIndex})
	})
	m.mu.Lock()
	if old := m.pcs[name]; old != nil {
		old.Close()
	}
	m.pcs[name] = pc
	m.mu.Unlock()
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
		m.t.Errorf("mock remote: %v", err)
		return
	}
	ans, _ := pc.CreateAnswer(nil)
	pc.SetLocalDescription(ans)
	m.to(name, map[string]interface{}{"type": "voice_answer", "sdp": ans.SDP})
}

// ── Alice ─────────────────────────────────────────────────────────────────────

type alice struct {
	m     *mock
	text  *textCrypto
	call  *callCrypto
	inbox chan string // what the bot says, decrypted
	seen  []string    // said, but not looked for yet
}

func newAlice(t *testing.T, m *mock) *alice {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := &alice{m: m, inbox: make(chan string, 100)}
	tok := m.addLocal("alice", priv, a.receive)
	m.alicePriv = priv
	ap := newAPI(m.srv.URL)
	ap.setToken(tok)
	a.text = newTextCrypto(ap, priv)
	a.text.setUsername("alice")
	a.call = newCallCrypto(priv, func(v interface{}) error {
		data, _ := json.Marshal(v)
		var mv map[string]interface{}
		json.Unmarshal(data, &mv)
		m.handle("alice", mv)
		return nil
	})
	a.text.ensurePublished()
	return a
}

func (a *alice) receive(v map[string]interface{}) {
	switch v["type"] {
	case "message":
		d := v["data"].(map[string]interface{})
		if d["username"] == "alice" {
			return
		}
		text, ok := a.text.open(d["board_id"].(string), d["username"].(string), d["content"].(string))
		if !ok {
			text = "<couldn't open>"
		}
		a.inbox <- text
	case "notice":
		a.inbox <- v["text"].(string)
	case "voice_state":
		ch := map[string][]string{}
		raw, _ := json.Marshal(v["channels"])
		json.Unmarshal(raw, &ch)
		a.call.updateRoster(ch)
	case "voice_e2ee":
		raw, _ := json.Marshal(v)
		a.call.handleMessage(raw)
	}
}

func (a *alice) say(t *testing.T, board, text string) {
	c, err := a.text.seal(board, text)
	if err != nil {
		t.Fatalf("alice can't encrypt: %v", err)
	}
	a.m.handle("alice", map[string]interface{}{"type": "message", "board_id": board, "content": c})
}

// cmd: alice types "/music <line>" in board — sent on her lane, as the app
// sends it (the plugin reads no chat, and takes nothing in the clear).
func (a *alice) cmd(t *testing.T, board, line string) {
	t.Helper()
	name, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	args := map[string]string{}
	if key := commandArg[name]; key != "" && arg != "" {
		args[key] = arg
	}
	a.m.app("plugin_command", map[string]interface{}{"board_id": board, "command": name, "args": args})
}

// never: the bot doesn't say anything containing want for a while.
func (a *alice) never(t *testing.T, want string, d time.Duration) {
	t.Helper()
	end := time.After(d)
	for {
		select {
		case s := <-a.inbox:
			if strings.Contains(s, want) {
				t.Fatalf("the bot said %q", s)
			}
			a.seen = append(a.seen, s)
		case <-end:
			return
		}
	}
}

// expect waits for the bot to say something containing want.
func (a *alice) expect(t *testing.T, want string, within time.Duration) string {
	t.Helper()
	for i, s := range a.seen {
		if strings.Contains(s, want) {
			a.seen = append(a.seen[:i], a.seen[i+1:]...)
			return s
		}
	}
	deadline := time.After(within)
	for {
		select {
		case s := <-a.inbox:
			t.Logf("bot says: %q", s)
			if strings.Contains(s, want) {
				return s
			}
			a.seen = append(a.seen, s)
		case <-deadline:
			t.Fatalf("bot never said %q", want)
		}
	}
}

// open decrypts one of the bot's audio frames with the keys it gave alice.
func (a *alice) open(frame []byte) ([]byte, bool) {
	sealed, kid, _, err := frameHeader(kindAudio, frame)
	if err != nil {
		return nil, false
	}
	a.call.mu.Lock()
	p := a.call.peers["Music"]
	var keys []*peerKey
	if p != nil {
		keys = append(keys, p.keys...)
	}
	a.call.mu.Unlock()
	for _, k := range keys {
		if byte(k.kid) == kid {
			pt, err := openSealed(k.aead, kindAudio, sealed)
			return pt, err == nil
		}
	}
	return nil, false
}

// botStatus opens the bot's latest status blob as alice: "emoji text", or
// "" for none.
func (a *alice) botStatus(t *testing.T) (string, int) {
	a.m.mu.Lock()
	n := len(a.m.status)
	var blob string
	if n > 0 {
		blob = a.m.status[n-1]
	}
	a.m.mu.Unlock()
	if blob == "" {
		return "<none sent>", 0
	}
	text, ok := a.text.open(presenceKeyID, "Music", blob)
	if !ok {
		t.Fatalf("alice can't open the bot's status")
	}
	var p presencePlain
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("status isn't JSON: %v", err)
	}
	if len(blob) < 2000 {
		t.Fatalf("status blob isn't padded (%d bytes)", len(blob))
	}
	return strings.TrimSpace(p.E + " " + p.C), len(blob)
}

func (a *alice) expectStatus(t *testing.T, want string) { //nolint:unused // kept for bots, which may post one
	t.Helper()
	var got string
	waitFor(t, "the bot's status to be "+want, 10*time.Second, func() bool {
		got, _ = a.botStatus(t)
		return got == want
	})
}

// ── The test ──────────────────────────────────────────────────────────────────

func TestBot(t *testing.T) {
	work := t.TempDir()
	// Two short test tracks served over HTTP (yt-dlp's generic extractor).
	media := filepath.Join(work, "media")
	os.MkdirAll(media, 0755)
	for i, freq := range []int{440, 660} {
		out := filepath.Join(media, fmt.Sprintf("tone%d.mp3", i+1))
		if b, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i",
			fmt.Sprintf("sine=frequency=%d:duration=20", freq), "-c:a", "libmp3lame", out).CombinedOutput(); err != nil {
			t.Fatalf("making test audio: %v %s", err, b)
		}
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ln, http.FileServer(http.Dir(media)))
	mediaURL := "http://" + ln.Addr().String()

	m := newMock(t)
	defer m.srv.Close()
	const text, voiceA = "text-1", "voice-1"
	a := newAlice(t, m)

	cfg := config{ServerURL: m.srv.URL, Token: testToken(t), DataDir: filepath.Join(work, "bot"),
		AloneAfter: 3 * time.Second, YtdlpEvery: time.Hour, YtdlpPath: os.Getenv("BOT_YTDLP"), FFmpeg: "ffmpeg",
		AudioFormat: "mp3", Bitrate: "96k", Channels: 1, Volume: 0.5, MaxQueue: 50, MaxPlaylist: 10,
		PluginID: "music"}
	os.MkdirAll(filepath.Join(cfg.DataDir, "downloads"), 0700)
	// A non-square picture: it should arrive as a 512 px square PNG.
	if b, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=1200x800",
		"-frames:v", "1", filepath.Join(cfg.DataDir, "pfp.jpg")).CombinedOutput(); err != nil {
		t.Fatalf("making a test picture: %v %s", err, b)
	}
	y := newYtdlp(cfg)
	if !y.ready() {
		if err := y.update(); err != nil {
			t.Fatalf("no yt-dlp: %v", err)
		}
	}
	bot := startBot(t, cfg, y)
	// The bot publishes its key; then alice's first message makes the
	// channel's key, sealed for both of them.
	waitFor(t, "the bot to sign in and publish its key", 10*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		u := m.users["Music"]
		return u != nil && u.encKey != nil
	})

	// Alice's app opens its lane to the plugin: its announcements (what
	// it says in channels) and panel come on it.
	waitFor(t, "the plugin to take the signed member list", 10*time.Second, func() bool { return bot.c.RosterState() == "active" })
	m.openLane(t)

	waitFor(t, "the bot's picture", 10*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.pfp != nil })
	m.mu.Lock()
	img, format, err := image.Decode(bytes.NewReader(m.pfp))
	m.mu.Unlock()
	if err != nil || format != "png" || img.Bounds().Dx() != 512 || img.Bounds().Dy() != 512 {
		t.Fatalf("picture should be a 512x512 PNG, got %s %v (%v)", format, img.Bounds(), err)
	}

	// Not in a call yet: the bot won't come.
	a.cmd(t, text, "play "+mediaURL+"/tone1.mp3")
	a.expect(t, "Join a voice channel first", 10*time.Second)

	// The same from the app (/music play), on her lane: the answer is just
	// for her; and the panel for everyone says it's not in a call.
	m.expectUI(t, "", `"blocks"|Not in a call|"action":"add"`, 10*time.Second)
	id := m.app("plugin_command", map[string]interface{}{"board_id": text, "command": "play", "args": map[string]string{"query": mediaURL + "/tone1.mp3"}})
	m.expectUI(t, id, "Join a voice channel first", 10*time.Second)
	id = m.app("plugin_command", map[string]interface{}{"board_id": text, "command": "help", "args": map[string]string{}})
	m.expectUI(t, id, "/music play <link or words>", 10*time.Second)

	// Alice joins voice; then !play.
	a.call.begin(voiceA, "alice")
	m.handle("alice", map[string]interface{}{"type": "join_voice", "board_id": voiceA})
	a.cmd(t, text, "play "+mediaURL+"/tone1.mp3")
	a.cmd(t, text, "play "+mediaURL+"/tone2.mp3")
	a.expect(t, "Now playing: tone1", 30*time.Second)
	a.expect(t, "#1 in line", 10*time.Second)

	// Audio arrives, decrypts with alice's key from the bot, at 50 packets a second.
	drain(m.tap)
	start := time.Now()
	var got [][]byte
	bad := 0
	for time.Since(start) < 2*time.Second {
		select {
		case f := <-m.tap:
			if pt, ok := a.open(f); ok {
				got = append(got, pt)
			} else {
				bad++
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Logf("2 s of the call: %d frames opened, %d didn't", len(got), bad)
	if len(got) < 85 || len(got) > 115 {
		t.Fatalf("expected ~100 packets in 2 s (20 ms each), got %d", len(got))
	}
	// …and is real Opus that ffmpeg can decode into the tone.
	checkDecodes(t, got, work)

	// A picture in the call (the SDK's video.go): shown like a screen
	// share, encrypted with the bot's call keys — alice opens every frame,
	// and they decode.
	checkVideo(t, m, a, bot, work)

	// Pause: nothing sent; resume: it carries on.
	a.cmd(t, text, "pause")
	a.expect(t, "Paused", 10*time.Second)
	time.Sleep(300 * time.Millisecond)
	drain(m.tap)
	time.Sleep(time.Second)
	if n := len(m.tap); n > 2 {
		t.Fatalf("paused, but %d packets went out", n)
	}
	a.cmd(t, text, "queue")
	a.expect(t, "Up next:\n1. tone2", 10*time.Second)
	a.cmd(t, text, "resume")
	a.expect(t, "Resumed", 10*time.Second)
	time.Sleep(time.Second)
	if n := len(m.tap); n < 30 {
		t.Fatalf("resumed, but only %d packets in a second", n)
	}

	// Skip → the next track.
	a.cmd(t, text, "skip")
	a.expect(t, "Skipped tone1", 10*time.Second)
	a.expect(t, "Now playing: tone2", 20*time.Second)

	// The panel, about the call: what's playing, moving; then the app's
	// buttons and commands.
	panel := m.expectUI(t, "", `"board_id":"voice-1"|tone2|"type":"progress"|"running":true|"action":"toggle"`, 10*time.Second)
	if !strings.Contains(panel, `"elapsed_ms":`) {
		t.Fatalf("the progress has no time: %s", panel)
	}
	m.app("plugin_action", map[string]interface{}{"action": "toggle"})
	m.expectUI(t, "", `tone2|"running":false|"label":"Resume"`, 10*time.Second)
	m.app("plugin_action", map[string]interface{}{"action": "toggle"})
	m.expectUI(t, "", `tone2|"running":true|"label":"Pause"`, 10*time.Second)
	// Queued from the panel's box, then taken out with its ✕.
	m.app("plugin_action", map[string]interface{}{"action": "add", "value": mediaURL + "/tone1.mp3"})
	m.expectUI(t, "", `Up next · 1 track|"action":"remove","icon":"close","label":"Remove from the queue","value":"1"`, 20*time.Second)
	id = m.app("plugin_command", map[string]interface{}{"board_id": text, "command": "queue", "args": map[string]string{}})
	m.expectUI(t, id, "Up next:\n1. tone1", 10*time.Second)
	m.app("plugin_action", map[string]interface{}{"action": "remove", "value": "1"})
	m.expectUI(t, "", `"empty":"Nothing queued."|"items":null`, 10*time.Second)
	// A command that changes things is said in the channel, with who asked.
	m.app("plugin_command", map[string]interface{}{"board_id": text, "command": "pause", "args": map[string]string{}})
	a.expect(t, "Paused. — alice", 10*time.Second)
	m.app("plugin_command", map[string]interface{}{"board_id": text, "command": "resume", "args": map[string]string{}})
	a.expect(t, "Resumed. — alice", 10*time.Second)

	// The connection drops mid-track: back in the same call, still playing.
	m.mu.Lock()
	m.users["Music"].conn.Close()
	m.mu.Unlock()
	waitFor(t, "the bot to drop out of the call", 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.voice["Music"] == "" })
	waitFor(t, "the bot to rejoin the call", 15*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.voice["Music"] == voiceA })
	time.Sleep(3 * time.Second)
	drain(m.tap)
	time.Sleep(time.Second)
	opened := 0
	for len(m.tap) > 0 {
		if _, ok := a.open(<-m.tap); ok {
			opened++
		}
	}
	t.Logf("after reconnecting: %d frames opened in 1 s", opened)
	if opened < 40 {
		t.Fatalf("not playing after reconnecting (%d frames in 1 s)", opened)
	}

	// Clear and disconnect.
	a.cmd(t, text, "play "+mediaURL+"/tone1.mp3")
	a.expect(t, "Queued tone1", 20*time.Second)
	a.cmd(t, text, "clear")
	a.expect(t, "Cleared the queue (1 track)", 10*time.Second)
	a.cmd(t, text, "disconnect")
	a.expect(t, "Left the call", 10*time.Second)
	waitFor(t, "the bot to leave", 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.voice["Music"] == "" })

	// Left alone: gone after BOT_ALONE_TIMEOUT (3 s here).
	a.cmd(t, text, "play "+mediaURL+"/tone1.mp3")
	a.expect(t, "Now playing: tone1", 30*time.Second)
	m.handle("alice", map[string]interface{}{"type": "leave_voice"})
	a.call.end()
	waitFor(t, "the bot to leave once alone", 8*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.voice["Music"] == "" })
	a.expect(t, "on my own", 5*time.Second)
}

// testToken makes a bot (as the app does) and its token.
func testToken(t *testing.T) string {
	_, maker, _ := ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	cert, err := chriscordbot.NewBotCert(maker, pub, "Music", "Plays music in calls", time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	tok, err := chriscordbot.EncodeBotToken(key, cert)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// startBot runs the bot the way main does.
func startBot(t *testing.T, cfg config, y *ytdlp) *Bot {
	c, err := chriscordbot.New(chriscordbot.Options{Server: cfg.ServerURL, Token: cfg.Token, Client: "chriscord-music-plugin/" + version, Logf: logf})
	if err != nil {
		t.Fatal(err)
	}
	b := newBot(cfg, c, y)
	go b.pl.loop()
	go b.ui.loop()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)
	return b
}

func waitFor(t *testing.T, what string, d time.Duration, f func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func drain(c chan []byte) {
	for {
		select {
		case <-c:
		default:
			return
		}
	}
}

// checkDecodes puts the decrypted packets in an Ogg file and has ffmpeg
// decode it, checking the length and that it's the 440 Hz tone (by
// counting zero crossings).
func checkDecodes(t *testing.T, pkts [][]byte, dir string) {
	path := filepath.Join(dir, "heard.ogg")
	w, err := oggwriter.New(path, 48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range pkts {
		w.WriteRTP(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: uint32(i * 960)}, Payload: p})
	}
	w.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffmpeg", "-loglevel", "error", "-i", path, "-f", "s16le", "-ac", "1", "-ar", "48000", "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg couldn't decode what the call carried: %v", err)
	}
	samples := len(out) / 2
	secs := float64(samples) / 48000
	cross := 0
	prev := int16(0)
	for i := 0; i+1 < len(out); i += 2 {
		s := int16(out[i]) | int16(out[i+1])<<8
		if (prev < 0) != (s < 0) {
			cross++
		}
		prev = s
	}
	freq := float64(cross) / 2 / secs
	t.Logf("decoded %.2f s of audio, ~%.0f Hz", secs, freq)
	if secs < 1.5 || freq < 400 || freq > 480 {
		t.Fatalf("that's not 2 s of the 440 Hz tone")
	}
}

// ── YouTube-shaped cases, with a stand-in yt-dlp ─────────────────────────────

// fakeYtdlp answers like yt-dlp does for YouTube: a search whose top result
// is a live stream, a mix with private and deleted videos in it, a video
// whose first download fails with 403, and stream addresses for -g. Every
// "video" is the 440 Hz tone.
const fakeYtdlp = `#!/usr/bin/env python3
import json, os, sys, shutil
args = sys.argv[1:]
target = args[args.index("--") + 1] if "--" in args else ""
TONE = os.environ["FAKE_TONE"]; TONE_URL = os.environ["FAKE_TONE_URL"]; STATE = os.environ["FAKE_STATE"]
def vid(i, title, dur=200, ch="Some Channel", live=None):
    e = {"_type": "url", "ie_key": "Youtube", "id": i, "url": "https://www.youtube.com/shorts/" + i if i.startswith("S") else "https://www.youtube.com/watch?v=" + i, "title": title, "channel": ch, "duration": dur}
    if live: e["live_status"] = None; e["duration"] = None  # as real YouTube search results show live streams
    return e
def vid_id(u):
    return u.split("v=")[1].split("&")[0] if "v=" in u else u
if "-g" in args:
    print(TONE_URL); sys.exit(0)
if "-o" in args and args[args.index("-o") + 1] == "-":  # streaming a live stream to stdout
    if not vid_id(target).startswith("LIVE"): sys.exit(3)
    # Like the real thing, which hands live HLS to an ffmpeg of its own: a
    # child process holding the same stdout/stderr open.
    import subprocess; subprocess.Popen(["sleep", "300"])
    with open(TONE, "rb") as f: shutil.copyfileobj(f, sys.stdout.buffer)
    import time; time.sleep(300)  # a live stream doesn't end
    sys.exit(0)
if "-J" in args:
    if target.startswith("ytsearch"):
        n = int(target[len("ytsearch"):].split(":")[0]); q = target.split(":", 1)[1]
        es = [vid("LIVElofi001", "lofi hip hop radio 📚 beats to relax/study to", live="is_live", ch="Lofi Girl"),
              {"_type": "url", "ie_key": "YoutubeTab", "id": "UCchan", "url": "https://www.youtube.com/channel/UCchan", "title": "Lofi Girl"}]
        es += [vid("VIDlofi%04d" % i, "%s track %d" % (q, i), 120 + i) for i in range(1, n)]
        print(json.dumps({"_type": "playlist", "title": q, "entries": es[:n]})); sys.exit(0)
    if "list=RD" in target:
        es = [vid("MIXseed0001", "Seed song")] + [vid("MIX%08d" % i, "Mix song %d" % i) for i in range(1, 30)]
        es[3]["title"] = "[Private video]"; es[5]["title"] = "[Deleted video]"
        es[7] = vid("SHORT000007", "A short")
        es[2] = vid("VIDflaky403", "Flaky song")
        end = int(args[args.index("--playlist-end") + 1])
        print(json.dumps({"_type": "playlist", "title": "Mix - Seed song", "entries": es[:end]})); sys.exit(0)
    i = vid_id(target)
    e = {"id": i, "title": "Video " + i, "webpage_url": target, "duration": 200, "channel": "Some Channel"}
    if i.startswith("LIVE"): e["is_live"] = True; e["live_status"] = "is_live"
    print(json.dumps(e)); sys.exit(0)
if "--print" in args:  # download
    i = vid_id(target)
    import glob, time, atexit
    if glob.glob(STATE + ".active-*"): open(STATE + ".overlap", "a").write(i + "\n")
    mark = STATE + ".active-" + i; open(mark, "w").close(); atexit.register(lambda: os.path.exists(mark) and os.remove(mark))
    time.sleep(0.3)
    if i.startswith("LIVE"): sys.exit(0)  # left out by --match-filter
    if i == "VIDflaky403" and not os.path.exists(STATE):
        open(STATE, "w").close()
        print("ERROR: [download] Got error: HTTP Error 403: Forbidden", file=sys.stderr); sys.exit(1)
    d = os.path.dirname(args[args.index("-o") + 1]); out = os.path.join(d, i + ".mp3")
    shutil.copy(TONE, out); print(out); sys.exit(0)
print("fake yt-dlp: unexpected " + " ".join(args), file=sys.stderr); sys.exit(2)
`

func TestBotYouTube(t *testing.T) {
	work := t.TempDir()
	media := filepath.Join(work, "media")
	os.MkdirAll(media, 0755)
	tone := filepath.Join(media, "tone.mp3")
	if b, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=30",
		"-c:a", "libmp3lame", tone).CombinedOutput(); err != nil {
		t.Fatalf("making test audio: %v %s", err, b)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go http.Serve(ln, http.FileServer(http.Dir(media)))
	fake := filepath.Join(work, "yt-dlp")
	os.WriteFile(fake, []byte(fakeYtdlp), 0755)
	t.Setenv("FAKE_TONE", tone)
	t.Setenv("FAKE_TONE_URL", "http://"+ln.Addr().String()+"/tone.mp3")
	t.Setenv("FAKE_STATE", filepath.Join(work, "flaky-done"))

	m := newMock(t)
	defer m.srv.Close()
	const text, voice = "text-1", "voice-1"
	a := newAlice(t, m)
	cfg := config{ServerURL: m.srv.URL, Token: testToken(t), DataDir: filepath.Join(work, "bot"),
		AloneAfter: time.Minute, YtdlpEvery: time.Hour, YtdlpPath: fake, FFmpeg: "ffmpeg",
		AudioFormat: "best", Bitrate: "96k", Channels: 1, Volume: 0.5, MaxQueue: 50, MaxPlaylist: 10, PluginID: "music"}
	os.MkdirAll(filepath.Join(cfg.DataDir, "downloads"), 0700)
	bot := startBot(t, cfg, newYtdlp(cfg))
	waitFor(t, "the bot to sign in", 10*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		u := m.users["Music"]
		return u != nil && u.encKey != nil
	})
	waitFor(t, "the plugin to take the signed member list", 10*time.Second, func() bool { return bot.c.RosterState() == "active" })
	m.openLane(t)
	a.call.begin(voice, "alice")
	m.handle("alice", map[string]interface{}{"type": "join_voice", "board_id": voice})

	// !search lists 10 (not the channel), the live one marked; !play 3 picks.
	a.cmd(t, text, "search lofi")
	list := a.expect(t, "Results for \"lofi\"", 20*time.Second)
	if !strings.Contains(list, "1. lofi hip hop radio") ||
		!strings.Contains(list, "10. lofi track 9") || strings.Contains(list, "\n11.") {
		t.Fatalf("search list looks wrong:\n%s", list)
	}
	a.cmd(t, text, "play 3")
	a.expect(t, "Now playing: lofi track 2", 20*time.Second)

	// Commands in the clear — the server making them up, or one told the
	// plugin has no lanes — are ignored, and never answered in the clear
	// (the mock fails the test if the plugin sends plugin_reply).
	for _, kind := range []string{"plugin_command", "plugin_action"} {
		m.to("Music", map[string]interface{}{"type": kind, "id": "forged-" + kind, "user": "alice", "board_id": text,
			"command": "stop", "args": map[string]string{}, "action": "stop"})
	}
	a.never(t, "Stopped", 2*time.Second)
	drain(m.tap)
	time.Sleep(time.Second)
	if n := len(m.tap); n < 40 {
		t.Fatalf("a forged command stopped it (%d packets in 1 s)", n)
	}
	a.cmd(t, text, "pause")
	a.expect(t, "|| Paused. — alice", 10*time.Second)
	a.cmd(t, text, "resume")
	a.expect(t, "Resumed. — alice", 10*time.Second)

	// Words: the top result — here a live stream, streamed rather than downloaded.
	a.cmd(t, text, "play lofi")
	a.expect(t, "Queued lofi hip hop radio", 20*time.Second)
	a.cmd(t, text, "skip")
	a.expect(t, "Now playing: lofi hip hop radio 📚 beats to relax/study to (🔴 live)", 20*time.Second)
	time.Sleep(1500 * time.Millisecond)
	drain(m.tap)
	time.Sleep(time.Second)
	if n := len(m.tap); n < 40 {
		t.Fatalf("the live stream isn't playing (%d packets in 1 s)", n)
	}
	a.cmd(t, text, "np")
	a.expect(t, "🔴 live", 10*time.Second)

	// A mix link, radio-style: private/deleted left out, capped at 10.
	a.cmd(t, text, "play https://music.youtube.com/watch?v=MIXseed0001&list=RDAMVMMIXseed0001&si=abc")
	a.expect(t, "Queued 10 tracks from the mix “Mix - Seed song” (skipped 2 unavailable)", 20*time.Second)
	a.cmd(t, text, "queue")
	q := a.expect(t, "Up next:", 10*time.Second)
	if !strings.Contains(q, "3. Flaky song") || strings.Contains(q, "Private") {
		t.Fatalf("queue looks wrong:\n%s", q)
	}

	// The 403 on "Flaky song" is retried and it plays.
	a.cmd(t, text, "skip") // the live stream → Seed song
	a.expect(t, ">> Skipped lofi hip hop radio", 10*time.Second)
	a.expect(t, "Now playing: Seed song", 20*time.Second)
	a.cmd(t, text, "skip") // → Mix song 1
	a.expect(t, "Now playing: Mix song 1", 20*time.Second)
	a.cmd(t, text, "skip") // → Flaky song (first download got a 403)
	a.expect(t, "Now playing: Flaky song", 30*time.Second)
	if _, err := os.Stat(filepath.Join(work, "flaky-done")); err != nil {
		t.Fatal("the 403 never happened — the test didn't test it")
	}

	// Downloads went one at a time, never several at once.
	if b, err := os.ReadFile(filepath.Join(work, "flaky-done.overlap")); err == nil {
		t.Fatalf("downloads overlapped: %s", b)
	}

	// !stop: nothing playing, queue empty, still in the call.
	a.cmd(t, text, "stop")
	a.expect(t, "Stopped and cleared the queue", 10*time.Second)
	time.Sleep(500 * time.Millisecond)
	drain(m.tap)
	time.Sleep(time.Second)
	if n := len(m.tap); n > 2 {
		t.Fatalf("stopped, but %d packets went out", n)
	}
	m.mu.Lock()
	still := m.voice["Music"] == voice
	m.mu.Unlock()
	if !still {
		t.Fatal("/music stop shouldn't leave the call")
	}
	a.cmd(t, text, "queue")
	a.expect(t, "The queue's empty.", 10*time.Second)
	// It never posts a status: plugins aren't given the key members' statuses
	// are sealed with (the signed member list keeps it from them), so what's
	// playing shows in its panel and announcements instead.
	if got, _ := a.botStatus(t); got != "<none sent>" {
		t.Fatalf("the plugin posted a status: %q", got)
	}
}

func checkVideo(t *testing.T, m *mock, a *alice, bot *Bot, dir string) {
	t.Helper()
	src := filepath.Join(dir, "pic.h264")
	if b, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-t", "2",
		"-c:v", "libx264", "-profile:v", "baseline", "-tune", "zerolatency", "-pix_fmt", "yuv420p", "-x264-params", "aud=1", "-f", "h264", src).CombinedOutput(); err != nil {
		t.Fatalf("making test video: %v %s", err, b)
	}
	data, _ := os.ReadFile(src)
	aud := []byte{0, 0, 0, 1, 9}
	var frames [][]byte
	for len(data) > 0 {
		next := bytes.Index(data[len(aud):], aud)
		if next < 0 {
			frames = append(frames, data)
			break
		}
		frames = append(frames, data[:next+len(aud)])
		data = data[next+len(aud):]
	}
	if err := bot.c.StartVideo(240, 10); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "voice_screen on", 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return len(m.screen) > 0 && m.screen[len(m.screen)-1] })
	for i, f := range frames {
		if err := bot.c.WriteVideoH264(f, time.Duration(i)*100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var out []byte
	opened := 0
	end := time.After(5 * time.Second)
	for opened < len(frames) {
		select {
		case au := <-m.vtap:
			sealed, kid, _, err := frameHeader(kindH264, au)
			if err != nil {
				t.Fatalf("a frame isn't sealed: %v", err)
			}
			a.call.mu.Lock()
			var keys []*peerKey
			if p := a.call.peers["Music"]; p != nil {
				keys = append(keys, p.keys...)
			}
			a.call.mu.Unlock()
			ok := false
			for _, k := range keys {
				if byte(k.kid) == kid {
					pt, err := openSealed(k.aead, kindH264, sealed)
					if err == nil {
						out = append(out, pt...)
						ok = true
					}
				}
			}
			if !ok {
				t.Fatal("alice couldn't open a frame of the picture")
			}
			opened++
		case <-end:
			t.Fatalf("only %d of %d frames of the picture arrived", opened, len(frames))
		}
	}
	dst := filepath.Join(dir, "got.h264")
	os.WriteFile(dst, out, 0600)
	b, err := exec.Command("ffprobe", "-v", "error", "-count_frames", "-select_streams", "v:0", "-show_entries", "stream=nb_read_frames,width,height", "-of", "csv=p=0", dst).CombinedOutput()
	if err != nil || !strings.Contains(string(b), "320,240,") {
		t.Fatalf("the picture doesn't decode: %v %s", err, b)
	}
	t.Logf("picture: %d frames sent, opened and decoded (%s)", len(frames), strings.TrimSpace(string(b)))
	bot.c.StopVideo()
	waitFor(t, "voice_screen off", 5*time.Second, func() bool { m.mu.Lock(); defer m.mu.Unlock(); return !m.screen[len(m.screen)-1] })
}
