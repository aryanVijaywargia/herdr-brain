package tool

import (
	"strings"
	"testing"
)

// head is the start of a tool.toml before its command and inputs.
const head = `name = "test-package"
description = "Run the tests of one package"
cwd = "~/src/shop"
timeout_s = 600
effects = "runs tests; writes nothing outside the repo"
read_only = false
`

// input is the input table of the input package.
const input = `
[inputs.package]
type = "string"
pattern = "^[a-z0-9@/._-]{1,64}$"
`

func TestParse(t *testing.T) {
	c, err := Parse(head + `argv = ["pnpm", "test", "--filter", "{package}"]` + "\n" + input)
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if c.Name != "test-package" || c.Cwd != "~/src/shop" || c.TimeoutS != 600 || c.ReadOnly ||
		len(c.Argv) != 4 || c.Inputs["package"].Type != "string" || c.Script != "" {
		t.Errorf("Parse() = %+v", c)
	}

	steps := head + `steps = [["pnpm", "install"], ["pnpm", "test", "--filter", "{package}"]]` + "\n" + input
	if _, err := Parse(steps); err != nil {
		t.Errorf("Parse(steps) = %v", err)
	}
	script := head + `argv = ["run.sh", "{package}"]` + "\n" + input + scriptLine + "pnpm test --filter \"$1\" | tail -n 20\n"
	c, err = Parse(script)
	if err != nil {
		t.Fatalf("Parse(run.sh) = %v", err)
	}
	if c.Script != "pnpm test --filter \"$1\" | tail -n 20\n" {
		t.Errorf("Script = %q", c.Script)
	}
	// A shell without -c runs a file: an input is a plain argument there.
	if _, err := Parse(head + `argv = ["sh", "scripts/test.sh", "{package}"]` + "\n" + input); err != nil {
		t.Errorf("Parse(sh file) = %v", err)
	}
}

// AC: each refusal rule has a test, also for an input in a later step.
func TestParseRefuses(t *testing.T) {
	later := func(argv string) string {
		return head + `steps = [["pnpm", "install"], ` + argv + `]` + "\n" + input
	}
	for name, tc := range map[string]struct{ text, want string }{
		"input in the first element":            {head + `argv = ["{package}", "test"]` + "\n" + input, "first element"},
		"input in the first element, later":     {later(`["{package}", "test"]`), "step 2: element 1"},
		"input fills part of an element":        {head + `argv = ["pnpm", "--filter={package}"]` + "\n" + input, "one whole element"},
		"input fills part of an element, later": {later(`["pnpm", "--filter={package}"]`), "step 2: element 2"},
		"two inputs in one element":             {head + `argv = ["pnpm", "{package}{package}"]` + "\n" + input, "one whole element"},
		"input after sh -c":                     {head + `argv = ["sh", "-c", "{package}"]` + "\n" + input, "command position"},
		"input after bash -c, later":            {later(`["bash", "-c", "{package}"]`), "step 2: element 3"},
		"input after /bin/bash -lc":             {head + `argv = ["env", "/bin/bash", "-lc", "{package}"]` + "\n" + input, "command position"},
		"input after eval":                      {head + `argv = ["eval", "{package}"]` + "\n" + input, "command position"},
		"input after eval, later":               {later(`["xargs", "eval", "{package}"]`), "step 2: element 3"},
		"input with no type": {head + `argv = ["pnpm", "{package}"]` + "\n[inputs.package]\npattern = \"^a$\"\n",
			"type \"\""},
		"input with no pattern": {head + `argv = ["pnpm", "{package}"]` + "\n[inputs.package]\ntype = \"string\"\n",
			"no pattern"},
		"input with no table, later": {later(`["pnpm", "{other}"]`), "input other has no type and no pattern"},
		"run.sh with an input in its text": {head + `argv = ["run.sh", "{package}"]` + "\n" + input + scriptLine +
			"pnpm test --filter {package}\n", "{package} is in the text"},
		"run.sh with no step that runs it": {head + `argv = ["pnpm", "{package}"]` + "\n" + input + scriptLine +
			"echo \"$1\"\n", "only for a pipeline"},
		"a step runs a missing run.sh": {head + `argv = ["run.sh", "{package}"]` + "\n" + input, "has none"},
		"bad pattern":                  {head + `argv = ["pnpm", "{package}"]` + "\n[inputs.package]\ntype = \"string\"\npattern = \"(\"\n", "pattern"},
		"unknown type":                 {head + `argv = ["pnpm", "{package}"]` + "\n[inputs.package]\ntype = \"path\"\npattern = \"^a$\"\n", "type"},
		"no read-only restriction":     {strings.Replace(head, "read_only = false\n", "", 1) + `argv = ["pnpm"]`, "read_only is missing"},
		"argv and steps":               {head + `argv = ["pnpm"]` + "\n" + `steps = [["pnpm"]]`, "not both"},
		"no command":                   {head, "not both and not none"},
		"empty argv":                   {head + `argv = []`, "argv is empty"},
		"empty step, later":            {head + `steps = [["pnpm"], []]`, "step 2: argv is empty"},
		"unknown field":                {head + `argv = ["pnpm"]` + "\nlabel = \"read-only\"\n", "unknown field label"},
		"bad name":                     {strings.Replace(head, "test-package", "Test Package", 1) + `argv = ["pnpm"]`, "name"},
		"no timeout":                   {strings.Replace(head, "timeout_s = 600\n", "", 1) + `argv = ["pnpm"]`, "timeout_s"},
		"no effects":                   {strings.Replace(head, "effects = \"runs tests; writes nothing outside the repo\"\n", "", 1) + `argv = ["pnpm"]`, "effects"},
		"no cwd":                       {strings.Replace(head, "cwd = \"~/src/shop\"\n", "", 1) + `argv = ["pnpm"]`, "cwd"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(tc.text)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Parse() = %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// The effects text alone is not the restriction: "read-only" in it leaves read_only false.
func TestEffectsAreNotTheRestriction(t *testing.T) {
	text := strings.Replace(head, "runs tests; writes nothing outside the repo", "read-only", 1) + `argv = ["pnpm"]`
	c, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReadOnly {
		t.Error("ReadOnly = true from the effects text, want false")
	}
}

func TestFiles(t *testing.T) {
	toml := head + `argv = ["run.sh", "{package}"]` + "\n" + input
	text := toml + scriptLine + "echo \"$1\"\n"
	fs := Files(text)
	if fs[TomlName] != toml || fs[ScriptName] != "echo \"$1\"\n" || len(fs) != 2 {
		t.Errorf("Files() = %q", fs)
	}
	if fs[TomlName]+scriptLine+fs[ScriptName] != text {
		t.Error("the files do not join back to the text")
	}
	if fs := Files(toml); fs[TomlName] != toml || len(fs) != 1 {
		t.Errorf("Files(no run.sh) = %q", fs)
	}
}

// AC (S21 part): a change to the command, the schema, the cwd, the timeout, the effects, the
// restriction, the run.sh or the scope changes the contract hash, so the approval of the old
// hash does not cover it.
func TestHashCoversTheContract(t *testing.T) {
	text := head + `argv = ["run.sh", "{package}"]` + "\n" + input + scriptLine + "echo \"$1\"\n"
	base := Hash(text, "workspace", "ws_a14")
	for name, change := range map[string]func(string) string{
		"command": func(s string) string { return strings.Replace(s, `"run.sh", "{package}"`, `"run.sh", "--", "{package}"`, 1) },
		"schema":  func(s string) string { return strings.Replace(s, "{1,64}", "{1,65}", 1) },
		"cwd":     func(s string) string { return strings.Replace(s, "~/src/shop", "~/src/shop2", 1) },
		"timeout": func(s string) string { return strings.Replace(s, "timeout_s = 600", "timeout_s = 601", 1) },
		"effects": func(s string) string { return strings.Replace(s, "runs tests;", "runs tests,", 1) },
		"restriction": func(s string) string {
			return strings.Replace(s, "read_only = false", "read_only = true", 1)
		},
		"run.sh": func(s string) string { return strings.Replace(s, `echo "$1"`, `echo "$1" >&2`, 1) },
	} {
		if Hash(change(text), "workspace", "ws_a14") == base {
			t.Errorf("a change to the %s keeps the hash", name)
		}
	}
	if Hash(text, "global", "") == base || Hash(text, "workspace", "ws_b2") == base {
		t.Error("a change of the scope keeps the hash")
	}
}
