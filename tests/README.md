# Browser Robustness Suite

The suite is a runnable coverage matrix for `brwd`.

It intentionally mixes deterministic local fixtures with opt-in public network
scenarios:

- local fixtures cover semantic read, forms, selects, checkboxes, keyboard input,
  delayed controls, screenshots, canvas/map-like visual fallback, open Shadow DOM,
  and cookie list/set/delete (incl. HttpOnly) over a loopback HTTP fixture origin
- public scenarios check stable external sites

Run the default deterministic suite:

```sh
go build -o bin/brwd ./cmd/brwd
go build -o bin/brwcheck ./cmd/brwcheck
BRW_WORKSPACE=brw \
./bin/brwd --http 127.0.0.1:17310
./bin/brwcheck
```

Run public network scenarios too:

```sh
./bin/brwcheck --include-network
```

Run a single scenario:

```sh
./bin/brwcheck --only fixture-form-actions
```

## Measurement modes

Two modes of the same binary measure rather than gate. Both launch their own
headless Chrome and serve `tests/fixtures` over a loopback origin they start
themselves, so neither needs the daemon, the network or an account: the browser
launches with `--host-resolver-rules="MAP * ~NOTFOUND, EXCLUDE 127.0.0.1"`, so
anything but that origin fails to resolve. Neither measurement runs under
`go test ./...`. Two assertions do: one evaluation task in both modes, as the
guard that the grading can report a failure, and one check that the browser
cannot reach off this machine.

```sh
task bench                # per-command wall time, CDP round trips, transport bytes, observation tokens
task agent-eval           # four agent-level tasks graded on the page's end state
task agent-eval-verify    # the same four with the decisive act removed, which must be caught
```

The recorded first run and what each column means are in
[docs/benchmarks.md](../docs/benchmarks.md).
