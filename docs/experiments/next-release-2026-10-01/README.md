# Reproduce the next release experiments

These are research fixtures for release `v0.19.0` (`ad5d6a9`), not production
patches. Results and limitations are in [the research report](../../next-release-research-2026-10.md)
and [the raw measurements](../../measurements/next-release-2026-10-01.json).
The Go files have a `.fixture` suffix so exploratory timing tests do not become
part of the default test gate.

From the repository root, copy the fixtures into a disposable checkout:

```sh
brw_research_tree=$(mktemp -d /tmp/brw-research.XXXXXX)
git worktree add --detach "$brw_research_tree" v0.19.0
cp docs/experiments/next-release-2026-10-01/snapshot_test.go.fixture "$brw_research_tree/internal/snapshot/next_snapshot_experiment_test.go"
cp docs/experiments/next-release-2026-10-01/transport_parity_test.go.fixture "$brw_research_tree/internal/extensionbridge/next_transport_parity_test.go"
cd "$brw_research_tree"
GOMAXPROCS=4 go test ./internal/snapshot -run 'TestNext|TestRolePushdown(Randomized|Cold)|TestSnapshotSiblingIndex' -count=1 -v
GOMAXPROCS=4 go test ./internal/extensionbridge -run '^TestNextTransportParity$' -count=1 -v
```

Run browser packages serially. The transport test opens temporary headed and
headless Chromium profiles and loads a copied extension with fixture consent;
it does not use an installed signed-in profile. It requires unbranded Chromium
with unpacked-extension support and a graphical session for the headed arm.
Inspect skips: a skipped cell is missing evidence, not passing parity. The test
logs normalized semantic hashes so outputs can be compared across browser modes.
Its explicit cold snapshot precedes `Read`, which itself snapshots on direct CDP.

The snapshot experiment embeds separate prototype walkers alongside the current
walker. It does not modify the production walker. Timings are instrumented
in-page samples; compare their semantics and complete workflows before landing
either optimization. The crowding case is an observation probe and logs its
result; it is not a task-success assertion.

Back in the original repository, the deterministic reader probes use fake
providers and subprocess fixtures:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 docs/experiments/next-release-2026-10-01/reader_probe.py
```

They write `/tmp/brw-next-reader-probe.json`. The separate model probe makes six
real requests to the local service at `http://127.0.0.1:1234/v1` using
`qwen3.5-4b-mlx`, without changing its load settings. Run it only with that service
available and when local inference is intended:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 docs/experiments/next-release-2026-10-01/model_latency_probe.py
```

It writes `/tmp/brw-next-model-latency.json`. It never executes the proposed
browser actions. Its ambiguous case is expected to expose a model failure in
the recorded cohort; the script reports correctness rather than asserting
qualification. Six requests and first-visible-delta timing do not constitute a
model evaluation or a measured full browser loop.

The existing controller benchmark and honest/sabotaged end-state checks need no
experimental fixture:

```sh
go run ./cmd/brwcheck --bench --repo-root . --bench-out /tmp/brw-research-bench.json
go run ./cmd/brwcheck --eval --eval-verify --repo-root . --eval-out /tmp/brw-research-eval.json
GOMAXPROCS=2 go test ./internal/recipe -run '^$' -fuzz '^FuzzParseNeverPanics$' -fuzztime=25s -parallel=2
```

Preserve outputs before removing the disposable checkout. These commands run
targeted research, not `task check` or release validation.
