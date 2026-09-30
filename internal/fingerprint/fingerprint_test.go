package fingerprint

import "testing"

func TestPrompt(t *testing.T) {
	same := [][2]string{
		{"café", "café"},             // NFC: a composed and a decomposed accent
		{"  run make Foo\n", "run make Foo"}, // the space at the ends goes
		{"\tdeploy", "deploy \r\n"},
	}
	for _, c := range same {
		if Prompt(c[0]) != Prompt(c[1]) {
			t.Errorf("Prompt(%q) != Prompt(%q)", c[0], c[1])
		}
	}
	differ := [][2]string{
		{"run make Foo", "run make foo"},       // case stays
		{`run echo "a  b"`, `run echo "a b"`}, // inner space stays
		{"npm i", "npm  i"},
	}
	for _, c := range differ {
		if Prompt(c[0]) == Prompt(c[1]) {
			t.Errorf("Prompt(%q) == Prompt(%q)", c[0], c[1])
		}
	}
	if got := len(Prompt("x")); got != 64 {
		t.Errorf("len = %d, want 64 hex digits", got)
	}
}
