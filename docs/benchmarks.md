# Benchmarks

Private pre-release head-to-head runs compared `brw` with Claude-in-Chrome on
semantic browser tasks. The raw transcripts are not published, so these results
are directional rather than independently reproducible from this repository.

Observed signal:

- `brw` needed fewer turns.
- `brw` used fewer tokens.
- `brw` took less wall time.
- `brw` had lower estimated cost.
- `brw` needed fewer screenshots because actions return semantic observations.
- Claude-in-Chrome retained an auth advantage when it could use an already-open
  installed Chrome profile.

Interpretation:

- For normal DOM-heavy web tasks, refs plus action observations beat repeated
  screenshot interpretation.
- For auth-heavy tasks, installed-profile access matters. `brw` addresses that
  with the Chrome extension bridge and SSH runtime.
- The full MCP tool surface is intentionally broad. Use `--mcp-tools core` for
  a lean advertised tool set.

Raw transcripts are not shipped. They can contain prompts, paths, local machine
metadata, and third-party page state.
