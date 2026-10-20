package main

import (
	"reflect"
	"testing"
)

func TestParseCommand(t *testing.T) {
	for _, c := range []struct {
		in   string
		cmd  string
		args []string
	}{
		{"/roulette 6 50", "roulette", []string{"6", "50"}},
		{"/join@zcribe_bot 75", "join", []string{"75"}},
		{"/JOIN@Zcribe_Bot", "join", []string{}},
		{"/pull@someone_else_bot", "", nil}, // another bot's command
		{"hello /pull", "", nil},
		{"0xabc", "", nil},
		{"/pass", "pass", []string{}},
	} {
		cmd, args := parseCommand(c.in, "zcribe_bot")
		if cmd != c.cmd || (c.args != nil && !reflect.DeepEqual(args, c.args)) {
			t.Fatalf("%q: got %q %v, want %q %v", c.in, cmd, args, c.cmd, c.args)
		}
	}
}

func TestIDSet(t *testing.T) {
	got, err := idSet(" -5480726566, 8584769303 ,")
	if err != nil || !got[-5480726566] || !got[8584769303] || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := idSet("abc"); err == nil {
		t.Fatal("parsed abc")
	}
}
