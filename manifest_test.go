package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// plugin.json's commands are the ones the plugin knows, with the
// arguments it reads; and its settings are the ones config.go reads.
func TestManifestMatchesTheCode(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID       string `json:"id"`
		Internet bool   `json:"internet"`
		Commands []struct {
			Name string `json:"name"`
			Args []struct {
				Name     string `json:"name"`
				Required bool   `json:"required"`
			} `json:"args"`
		} `json:"commands"`
		Settings []struct {
			Key string `json:"key"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "music" || !m.Internet || len(m.Commands) == 0 {
		t.Fatalf("manifest: %+v", m)
	}
	code, _ := os.ReadFile("commands.go")
	cfg, _ := os.ReadFile("config.go")
	for _, c := range m.Commands {
		if !strings.Contains(string(code), `"`+c.Name+`"`) {
			t.Errorf("command %s isn't handled in commands.go", c.Name)
		}
		switch len(c.Args) {
		case 0:
			if commandArg[c.Name] != "" {
				t.Errorf("%s takes no argument in plugin.json but reads %q", c.Name, commandArg[c.Name])
			}
		case 1:
			if commandArg[c.Name] != c.Args[0].Name {
				t.Errorf("%s: plugin.json's argument is %q, the code reads %q", c.Name, c.Args[0].Name, commandArg[c.Name])
			}
		default:
			t.Errorf("%s: only one argument is read", c.Name)
		}
	}
	for _, s := range m.Settings {
		if !strings.Contains(string(cfg), `"`+s.Key+`"`) {
			t.Errorf("setting %s isn't read in config.go", s.Key)
		}
	}
}
