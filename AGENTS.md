# agent-browser Workspace

<!-- MCPLEXER:HARNESS-SYNC:BEGIN v1 (codex) -->

Use the 4 top-level tools (`mcpx__search_tools` + `mcpx__execute_code` for batch, `secret__prompt` / `secret__list_refs`).

For the full contract, fetch `mcpx.skill_get({name:"using-mcplexer"})` directly. Use `mcpx.skill_search` only for unknown/deeper playbooks: `mcplexer-features` / `mcplexer-tasks` / `agent-mesh` / `token-preserving-delegation`.

Start `mcpx__search_tools` in summary or exact-tool mode; avoid broad `detail:"full"` searches.

The `using-mcplexer` skill is the source of truth for the contract. If the gateway tools are not mounted, keep repo-local state in `.mcplexer/` and continue locally.

<!-- MCPLEXER:HARNESS-SYNC:END -->

This workspace builds `agent-browser`, a Go daemon that exposes a semantic, MCP-compatible interface to a real visible Chrome/Chromium browser.

Security and product constraints:

- Never use screenshots as the primary model of page state.
- Do not add stealth code intended to bypass site security, fraud checks, CAPTCHA, MFA, or consent gates.
- Use a normal visible browser and persistent profile so the human can inspect and take over.
- Default-profile Chrome auth is a hard product requirement, but Chrome 136+ blocks remote debugging of the default data directory. See `agent-browser/docs/auth-model.md` before changing launch or attach behavior.
