# Human approvals

`--approvals` adds an asynchronous operator inbox to the browser-host daemon. It is opt-in: existing site-consent and confirmation gates keep their behavior when the flag is absent. It requires `--site-consent`, implies `--confirm-actions`, and requires a loopback HTTP listener. It refuses `--site-consent-prompt` and `--upstream-http`; configure it on the upstream browser host instead.

Create an operator credential outside your checkout and artifact directory. Run this in the operator's environment; do not give the token to the agent or commit it:

```sh
mkdir -p "$HOME/.config/brw/operator"
chmod 700 "$HOME/.config/brw/operator"
(umask 077; openssl rand -hex 32 > "$HOME/.config/brw/operator/approval.token")
brwd --bridge --site-consent --approvals \
  --approval-token-file "$HOME/.config/brw/operator/approval.token" \
  --http 127.0.0.1:17310
```

The token file must be an absolute, non-symlink regular file with mode `0600`, containing at least 32 printable ASCII characters without whitespace; a trailing newline is accepted. Use a randomly generated token. The daemon never logs its contents. `--approval-store` selects an absolute request-store path; by default it is `approvals/requests.json` beside the profile policy. The store's dedicated parent must be a non-symlink `0700` directory and any existing store file must be `0600`. Both token and store must live outside the artifact directory. Site grants are still required; approving an action does not grant a site permission.

Open `http://127.0.0.1:17310/approvals` in an operator browser the agent does not control, then enter the token. The screen holds it in memory rather than in the URL or persistent browser storage. Review the pending action and its page evidence, then approve or deny it. An approved request can be revoked in the inbox until execution consumes it. Browser notifications are optional and require the operator's permission. An unlocked inbox polls every three seconds while visible, or every fifteen seconds in the background when notifications are enabled; browser throttling can delay alerts. there is no push or mobile delivery integration. For a remote browser host, keep its listener on loopback and use an SSH tunnel:

```sh
ssh -N -L 17310:127.0.0.1:17310 browser-host
```

The agent's first gated call returns a structured `approval_required` error with a request ID and status URL. Pending duplicates reuse the same request. The agent can continue other work while the operator decides; the daemon does not hold the original browser call open. Check the lifecycle with `brw_approval_status({approval_id})`; this returns status and expiry without private request contents. After the operator approves, call `brw_approval_resume({approval_id,tool,arguments})` with the original tool name and exact original argument object. Retrying the original tool with the same arguments plus `approval_id` also works. These calls cannot approve or deny a request, and the operator token is never a tool argument. A denied or expired request cannot authorize execution.

For example, preserve the arguments from the failed call rather than rebuilding them from the page:

```json
{"name":"brw_approval_status","arguments":{"approval_id":"REQUEST_ID"}}
{"name":"brw_approval_resume","arguments":{"approval_id":"REQUEST_ID","tool":"brw_click","arguments":{"tab_id":"TAB_ID","ref":"e5"}}}
```

An MCP proxy forwards the approval ID to the browser host using `X-Brw-Approval-Id`; the browser host owns the queue and operator inbox. The default `auto` catalogue does not grow merely because approvals are enabled. Both approval tools remain callable and can be discovered with `brw_tools`; the full `all` catalogue includes them.

Approval is bound to the exact requested action, tab, agent session, and observed page state, expires after ten minutes, and is invalidated by a relevant state change. It is single-use: consumption is persisted before execution starts. A consumed status means execution was authorized once; it does not establish that a backend transaction succeeded. If execution fails or its outcome is unknown, do not automatically retry a consumed approval. Inspect the resulting page and transaction state before requesting a new action; browser DOM evidence cannot attest a backend transaction or roll it back.

`--approval-mode risky` is the default. Risk detection uses available labels and page evidence on a best-effort basis; it cannot establish that every action is harmless. Opaque actions and unseen references are handled conservatively. `--approval-mode all` gates actions classified with the site-consent `act` scope; it is not an OS or filesystem permission boundary. Mutating sequences and `recipe_run` cannot safely bind one approval to their complete effects and are refused: split the work into individual visible actions or hand it over to the operator. Ordinary ungated and read calls do not take approval evidence snapshots or perform approval-store I/O.

The private approval store and authenticated operator inbox contain the requested tool, origin, tab and session identifiers, a preview of its arguments, and observed page title, form values and visible text. Argument keys and form labels that look like secrets are redacted heuristically; other personal data can remain in the preview or text. The full observed state is hashed for binding rather than stored in the request. This is browser evidence, not backend transaction truth. The agent-facing status APIs expose only lifecycle metadata.

The operator credential is a deployment boundary. An unrestricted process running as the same OS user can read that user's credential files and bypass this separation. Isolate the operator credentials and daemon from an untrusted agent process using separate accounts or equivalent filesystem/process controls. Never open the inbox in a browser profile the agent controls.
