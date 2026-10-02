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

Open `http://127.0.0.1:17310/approvals` in an operator browser the agent does not control, then enter the token. The screen holds it in memory rather than in the URL or persistent browser storage. Review the pending action and its page evidence, then approve or deny it. Browser notifications are optional and require the operator's permission; there is no push or mobile delivery integration. For a remote browser host, keep its listener on loopback and use an SSH tunnel:

```sh
ssh -N -L 17310:127.0.0.1:17310 browser-host
```

The agent's first gated call returns a structured `approval_required` error with a request ID and status URL. Pending duplicates reuse the same request. The agent can continue other work while the operator decides; the daemon does not hold the original browser call open. After approval, retry the exact tool and arguments with the returned `approval_id`. The operator token is never a tool argument. A denied or expired request cannot authorize execution.

Approval is bound to the requested action and page state, expires after ten minutes, and is invalidated by a relevant state change. It is single-use: consumption is persisted before execution starts. If execution fails or its outcome is unknown, do not automatically retry a consumed approval. Inspect the resulting page and transaction state before requesting a new action; browser DOM evidence cannot attest a backend transaction or roll it back.

`--approval-mode risky` is the default. Risk detection uses available labels and page evidence on a best-effort basis; it cannot establish that every action is harmless. Opaque actions and unseen references are handled conservatively. `--approval-mode all` gates mutations more conservatively. Mutating sequences and `recipe_run` cannot safely bind one approval to their complete effects and are refused: split the work into individual visible actions or hand it over to the operator. Ordinary ungated and read calls do not take approval evidence snapshots or perform approval-store I/O.

The operator credential is a deployment boundary. An unrestricted process running as the same OS user can read that user's credential files and bypass this separation. Isolate the operator credentials and daemon from an untrusted agent process using separate accounts or equivalent filesystem/process controls. Never open the inbox in a browser profile the agent controls.
