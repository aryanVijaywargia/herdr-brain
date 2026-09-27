---
name: go-quality
description: Go code quality rules for the herdr-brain repo. Use it whenever you write, change or review Go code here (any .go file, a Go test, or go.mod), including the review of a story pull request.
---

# Go quality

Brain is one Go binary, Go 1.23, standard library only. These rules keep the code small,
idiomatic and correct. They follow Effective Go, Go Code Review Comments and the Google and
Uber Go style guides, cut down to what matters in this repo.

- Match the code that is already here before you add a new pattern.
- When you review, cite the rule number: "rule 3: the error is logged and returned".
- A linter finding means a rule is broken: fix the code. Use `//nolint:<linter> // <reason>`
  only when the finding is wrong for this code. Never use a bare `//nolint`.

## Errors

1. One place prints. `main` is `os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))`, and `run`
   is the only function that prints an error and picks the exit code. Every other function
   returns `error`. No `fmt.Fprintf(stderr, ...); return 1` in each branch.
2. Wrap once, with context: `fmt.Errorf("create link dir: %w", err)`. Lowercase, `%w` at the
   end, no trailing period, no "failed to", "unable to" or "error:". Add only what the error
   does not already say: `*os.PathError` and `*os.LinkError` already name the operation and
   the path. A plain `return err` is fine when you have nothing to add.
3. Handle an error once: return it or report it, never both. No log-and-return.
4. Do not drop an error. An error assigned to `_` needs a comment that says why. Check the
   error from `Close` on a file you write.
5. Branch with `errors.Is` and `errors.As`, never on `err.Error()` text. Add an `ErrX` sentinel
   or an error type only when a caller branches on it. Map errors to exit codes in `run` only.
6. `panic` only for a programmer bug, such as an impossible `switch` case. `os.Exit` and
   `log.Fatal` only in `main`: they skip deferred calls.

`if err != nil` is idiomatic and stays. The problem is what the branch does.

Bad: each step prints and exits, and the message does not say which step failed.

```go
func setup(bin, link string, stderr io.Writer) int {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		fmt.Fprintf(stderr, "brain setup: %v\n", err)
		return 1
	}
	if err := os.Symlink(bin, link); err != nil {
		fmt.Fprintf(stderr, "brain setup: %v\n", err)
		return 1
	}
	return 0
}
```

Good: `setup` returns a wrapped error, and `run` prints it and picks the code.

```go
func setup(bin, link string) error {
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("create link dir: %w", err)
	}
	if err := os.Symlink(bin, link); err != nil {
		return fmt.Errorf("link brain binary: %w", err)
	}
	return nil
}

// In run:
	if err := setup(bin, link); err != nil {
		fmt.Fprintf(stderr, "brain setup: %v\n", err)
		return 1
	}
```

## Design (SOLID, DRY, YAGNI)

7. Add an interface only when two implementations exist now, or a test needs a seam. Define
   it where it is used, with only the methods that code calls. Return concrete types.
8. Build what the story's ACs need. No speculative parameters, option structs, hooks, generic
   helpers, config knobs or exported names "for later". Delete dead code; do not comment it
   out.
9. Extract shared code at the third copy, not the second. Blocks that differ only in data
   become a table or a loop.
10. Keep functions short and flat. Return early. No `else` after a `return`. The main path
    stays at the left edge.
11. No mutable package globals and no `init()`. Pass in the `io.Writer`, the paths, the
    environment and the clock (`now func() time.Time`), so tests control them. Constants,
    sentinel errors, compiled regexps and the `-ldflags` `version` are fine.
12. Names: no `util`, `common`, `helpers` or `misc` packages. No stutter: `paths.Dir`, not
    `paths.PathsDir`. MixedCaps, and initialisms keep one case: `ID`, `URL`, `userID`. Short
    names for short scopes (`i`, `b`, `err`), longer names for wider scopes. No `Get` prefix
    on getters. Receivers are one or two letters, the same in every method.

## Anti-slop

13. Comments say why, not what. A doc comment is a full sentence that starts with the name:
    `// Setup links the brain binary into ~/.local/bin.` No comments that restate the code,
    no "Step 1:" comments, no banner comments, no commented-out code.
14. No over-defensive code: no nil checks on values that cannot be nil, no second validation
    of the same input, no `len(s) > 0` guard before a `range`, no "just in case" `recover`.
    Validate at the edge (arguments, files, environment), then trust the values inside.
15. No silent fallbacks. Do not swallow an error and go on with a default value. When the
    spec wants a default for a missing file, check only `errors.Is(err, fs.ErrNotExist)`,
    say why in a comment, and test it.
16. Use the standard library, up to Go 1.23 (the `go` line in go.mod): `slices`, `maps`,
    `strings.Cut`, `cmp.Or`, `min`, `max`, `errors.Join`, `for i := range n`, range over
    functions. Not available yet (Go 1.24 and later): `t.Context`, `t.Chdir`, `b.Loop`,
    `strings.Lines`, `strings.SplitSeq`, `os.Root`, `omitzero`, `sync.WaitGroup.Go`.
    Loop variables are per iteration since Go 1.22, so no `v := v` copies. No new module
    dependency unless the story says so.

## Correctness

17. `defer f.Close()` goes right after the error check of a successful open. If a loop opens
    files, move the loop body into a function, so that each `defer` runs once per iteration.
    For a file you write, return the error from `Close`, or use `os.WriteFile`.
18. A function that can block, or that the user can cancel, takes `ctx context.Context` as
    its first parameter. Never store a `ctx` in a struct. Pass the `ctx` down; do not start a
    new `context.Background()` in the middle of a call chain.
19. Every goroutine has an owner that waits for it, and a way to stop (a `ctx` or a closed
    channel). Protect shared state with a mutex, or give it to one goroutine. No
    fire-and-forget goroutines.
20. Tests: table-driven with `t.Run`; helpers call `t.Helper()`; files go in `t.TempDir()`;
    environment through `t.Setenv`; errors checked with `errors.Is`. A failure message says
    `f(x) = got, want y`. `t.Fatal` only when the test cannot go on. No `time.Sleep`: sync on a
    channel or inject the clock. No assertion libraries.
21. Before you say the work is done: `gofmt -l .` prints nothing, and `go vet ./...`,
    `golangci-lint run` and `go test -race ./...` pass. If golangci-lint is not installed,
    push and read the log of the CI `lint` job. Do not install tools without the owner.

## Lint

`.golangci.yml` enforces part of these rules: errcheck (4), errorlint (5), forcetypeassert
(6), unused and unparam (8), dupl (9), nestif, funlen and revive (10, 12, 13), nilerr (15),
plus the general checks of staticcheck and gocritic. The linter cannot see most of rules 1,
3, 7, 11, 14 and 17 to 20. A reviewer checks those in the diff.
