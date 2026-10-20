package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Search results go in the panel of whoever searched — after the box, with
// a Play button each — and nobody else's; closing them takes them away.
func TestPanelSearchResults(t *testing.T) {
	u := newPanelUI(nil)
	base := map[string]interface{}{"blocks": []interface{}{
		map[string]interface{}{"type": "title", "text": "Now"},
		map[string]interface{}{"type": "input", "action": "add"},
		map[string]interface{}{"type": "list", "title": "Up next"},
	}}
	tracks := []trackInfo{{Title: "One", Uploader: "A", Duration: 3*time.Hour + time.Minute}, {Title: "Two"}}
	u.setResults("alice", tracks, "Results for “x”")

	mine, _ := json.Marshal(u.forUser(base, "alice"))
	theirs, _ := json.Marshal(u.forUser(base, "bob"))
	if strings.Contains(string(theirs), "Results for") {
		t.Fatal("bob sees alice's results")
	}
	s := string(mine)
	in, res, next := strings.Index(s, `"input"`), strings.Index(s, "Results for"), strings.Index(s, "Up next")
	if !(in >= 0 && in < res && res < next) {
		t.Fatalf("results not after the box: %s", s)
	}
	if !strings.Contains(s, `"action":"pick","icon":"play_arrow","label":"Play this","value":"2"`) ||
		!strings.Contains(s, "A · 3:01:00") || !strings.Contains(s, "close_results") {
		t.Fatalf("results: %s", s)
	}
	if blocks := base["blocks"].([]interface{}); len(blocks) != 3 {
		t.Fatal("the shared panel was changed")
	}
	u.setResults("alice", nil, "")
	if again, _ := json.Marshal(u.forUser(base, "alice")); strings.Contains(string(again), "Results for") {
		t.Fatal("closed results still shown")
	}
}
