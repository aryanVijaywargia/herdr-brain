package signal

import (
	"context"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aryanVijaywargia/herdr-brain/internal/store"
)

// countingTransport counts the HTTP requests of the process.
type countingTransport struct{ n *atomic.Int64 }

func (c countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, http.ErrNotSupported
}

// The rules make 0 model calls (§7.1: local rules, no LLM). Brain has no model client yet
// (the extractor is E13), so the test counts every way out that a model call would take: the
// HTTP requests of the process, and the connections to a proxy that the environment names
// for the process and for git. It also checks that the rules import no network or
// harness package.
func TestNoModelCalls(t *testing.T) {
	gitEnv(t)
	var requests, conns atomic.Int64
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() }) // the test is done with the listener
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			_ = c.Close() // the count is all that the test needs
		}
	}()
	proxy := "http://" + l.Addr().String()
	for _, v := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(v, proxy)
	}
	saved := http.DefaultTransport
	http.DefaultTransport = countingTransport{&requests}
	t.Cleanup(func() { http.DefaultTransport = saved })

	dir := t.TempDir()
	run(t, dir, "init", "-q")
	bad := commit(t, dir, "a.txt", "a", "add a")
	run(t, dir, "revert", "--no-edit", bad)
	conns.Store(0) // only the rules count, not the setup of the repo

	sigs := Find([]store.SignalInput{obs(1, 1, unconf, "no, use pnpm"), obs(2, 2, unconf, "no, use pnpm"),
		obs(3, 1, unconf, proc1), obs(4, 2, unconf, proc2), cmd(5, 1, "pnpm test", 1),
		obs(6, 1, tool, "error: test failed"), blocked(7, 1, assistant, codexApproval)})
	reverts, err := Reverts(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if kinds := len(sigs) + len(reverts); kinds < 7 {
		t.Fatalf("the rules found %d signals, want one of each kind at least", kinds)
	}
	if n, m := requests.Load(), conns.Load(); n != 0 || m != 0 {
		t.Errorf("model calls = %d requests, %d proxy connections, want 0", n, m)
	}
	checkImports(t)
}

// checkImports fails when a file of the rules imports a network package, or a Brain package
// other than the store.
func checkImports(t *testing.T) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		ast, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range ast.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			brain := strings.HasPrefix(p, "github.com/aryanVijaywargia/herdr-brain/")
			if p == "net" || strings.HasPrefix(p, "net/") || brain && !strings.HasSuffix(p, "/internal/store") {
				t.Errorf("%s imports %s, want no network and no Brain package but the store", f, p)
			}
		}
	}
}
