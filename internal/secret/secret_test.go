package secret

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The fake secrets are built at run time, so the source holds no literal that looks like a
// real key to a secret scanner.
var (
	ghToken      = "gh" + "p_" + strings.Repeat("a1B2", 9)
	privateKey   = "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEow" + strings.Repeat("Q", 40) + "\n-----END RSA PRIVATE KEY-----"
	anthropicKey = "sk-" + "ant-api03-" + strings.Repeat("xY9_", 8)
	testKey      = "sk-" + "test-" + strings.Repeat("abcdefghij", 4)
)

// classTest is one positive and one negative input for a class.
type classTest struct {
	class    string
	positive string
	negative string
}

// classTests has one or more cases for each supported class.
var classTests = []classTest{
	{"github-token", "token " + ghToken, "see ghp_ docs and gh pr view 12"},
	{"github-token", "github_" + "pat_" + strings.Repeat("A1_b", 8), "the github_pat_ prefix is new"},
	{"gitlab-token", "gl" + "pat-" + strings.Repeat("Zz9-", 6), "glpat-short"},
	{"anthropic-api-key", "key " + anthropicKey, "prefix sk-ant- only"},
	{"openai-api-key", "sk-" + "proj-" + strings.Repeat("Ab3_", 6), "use sk-learn for models"},
	{"openai-api-key", "sk-" + strings.Repeat("Ab3d", 12), "sk-" + strings.Repeat("a", 31)},
	{"openai-api-key", "my key is " + testKey, "sk-test-short and sk-live- keys"},
	{"openai-api-key", "sk-" + "live-" + strings.Repeat("Q7r_", 6), "the scikit-learn sk-learn-docs page"},
	{"aws-access-key-id", "AKIA" + "IOSFODNN7EXAMPLE", "AKIA plus a few words"},
	{"google-api-key", "AI" + "za" + strings.Repeat("Sy_B-", 7), "AIza is a Google prefix"},
	{"slack-token", "xo" + "xb-" + "1234567890-abcdef", "the xoxb- prefix"},
	{"stripe-key", "sk" + "_live_" + strings.Repeat("4eC3", 6), "sk_live_ and rk_test_ keys"},
	{"npm-token", "np" + "m_" + strings.Repeat("k7Q2", 9), "run npm_config_cache=/tmp npm i"},
	{"private-key", privateKey, "a BEGIN PUBLIC KEY block is fine"},
	{"private-key", "-----BEGIN " + "OPENSSH PRIVATE KEY-----\nb3Blbn", "-----BEGIN CERTIFICATE-----"},
	{"password-pair", "db password=hunter2", "reset the password in the UI"},
	{"password-pair", "PWD: s3cr3t!", "password: "},
	{"env-secret", "DB_TOKEN=abc123", "DB_HOST=localhost"},
	{"env-secret", "  export STRIPE_SECRET_KEY = zz", "export API_KEY="},
	{"env-secret", `python -c "import os; os.environ['X']" API_KEY=abc123`, "Pass the token and the key to the client."},
	{"env-secret", `run "API_KEY=abc123" now`, "git show 24d37fd9c1e0b7a4f3e2d1c0b9a8f7e6d5c4b3a2"},
}

func TestFindClasses(t *testing.T) {
	for _, tt := range classTests {
		t.Run(tt.class+"/positive", func(t *testing.T) {
			if !hasClass(Find(tt.positive), tt.class) {
				t.Errorf("Find(%q) = %v, want a %s hit", tt.positive, Find(tt.positive), tt.class)
			}
		})
		t.Run(tt.class+"/negative", func(t *testing.T) {
			if hits := Find(tt.negative); len(hits) != 0 {
				t.Errorf("Find(%q) = %v, want no hit", tt.negative, hits)
			}
		})
	}
}

func hasClass(hits []Hit, class string) bool {
	for _, h := range hits {
		if h.Class == class {
			return true
		}
	}
	return false
}

func TestFindRange(t *testing.T) {
	text := "use pnpm\n" + privateKey + "\nand " + ghToken + " here"
	hits := Find(text)
	want := []string{privateKey, ghToken}
	if len(hits) != len(want) {
		t.Fatalf("Find() = %v, want %d hits", hits, len(want))
	}
	for i, h := range hits {
		if got := text[h.Start:h.End]; got != want[i] {
			t.Errorf("hit %d (%s) covers %q, want %q", i, h.Class, got, want[i])
		}
	}
}

// TestFindInCommand is the run 7 shape of #392: a sk-test- key in a quoted pair inside a command.
// Both hits cover the key, so the stored text keeps no part of it.
func TestFindInCommand(t *testing.T) {
	text := `python3 -c "import os" "API_KEY=` + testKey + `" && echo done`
	hits := Find(text)
	for _, class := range []string{"openai-api-key", "env-secret"} {
		if !hasClass(hits, class) {
			t.Errorf("Find() = %v, want a %s hit", hits, class)
		}
	}
	for _, h := range hits {
		if got := text[h.Start:h.End]; !strings.Contains(got, testKey) {
			t.Errorf("hit %s covers %q, want the whole key", h.Class, got)
		}
	}
}

func TestFindOverlap(t *testing.T) {
	hits := Find("PASSWORD=x")
	if !hasClass(hits, "env-secret") || !hasClass(hits, "password-pair") {
		t.Errorf("Find(PASSWORD=x) = %v, want env-secret and password-pair", hits)
	}
}

// TestCheckRemember is the filter half of the E4-S2 AC `brain remember "token ghp_…"`: the
// call refuses and names the class.
func TestCheckRemember(t *testing.T) {
	err := Check("token " + ghToken)
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("Check() = %v, want *Error", err)
	}
	if got, want := err.Error(), "the text looks like it has a secret (github-token): remove it and try again"; got != want {
		t.Errorf("Check().Error() = %q, want %q", got, want)
	}
}

func TestCheckClean(t *testing.T) {
	if err := Check("use pnpm, not npm"); err != nil {
		t.Errorf("Check() = %v, want nil", err)
	}
}

func TestCheckClassesOnce(t *testing.T) {
	err := Check(ghToken + " " + ghToken + "\nDB_TOKEN=abc")
	var se *Error
	if !errors.As(err, &se) {
		t.Fatalf("Check() = %v, want *Error", err)
	}
	if got := strings.Join(se.Classes, ","); got != "github-token,env-secret" {
		t.Errorf("Classes = %s, want github-token,env-secret", got)
	}
}

// TestClassesTested checks that the table has a case for every class in the list.
func TestClassesTested(t *testing.T) {
	for _, c := range classes {
		if !slices.ContainsFunc(classTests, func(tt classTest) bool { return tt.class == c.name }) {
			t.Errorf("class %s has no test case", c.name)
		}
	}
}

// TestErrorHidesSecret checks every printed form of the error for every class, also when a
// caller wraps it.
func TestErrorHidesSecret(t *testing.T) {
	for _, tt := range classTests {
		t.Run(tt.class, func(t *testing.T) {
			err := Check(tt.positive)
			if err == nil {
				t.Fatalf("Check(%q) = nil, want an error", tt.positive)
			}
			wrapped := fmt.Errorf("brain remember: %w", err)
			for _, h := range Find(tt.positive) {
				secret := tt.positive[h.Start:h.End]
				for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
					for _, e := range []error{err, wrapped} {
						if out := fmt.Sprintf(format, e); strings.Contains(out, secret) {
							t.Errorf("Sprintf(%s, err) = %q, holds the secret", format, out)
						}
					}
				}
			}
		})
	}
}
