# Browser Robustness Suite

The suite is a runnable coverage matrix for `brwd`.

It intentionally mixes deterministic local fixtures with opt-in public network
scenarios:

- local fixtures cover semantic read, forms, selects, checkboxes, keyboard input,
  delayed controls, screenshots, canvas/map-like visual fallback, and open Shadow DOM
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
