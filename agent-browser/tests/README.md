# Browser Robustness Suite

The suite is a runnable coverage matrix for `agent-browserd`.

It intentionally mixes deterministic local fixtures with opt-in public and
authenticated scenarios:

- local fixtures cover semantic read, forms, selects, checkboxes, keyboard input,
  delayed controls, screenshots, canvas/map-like visual fallback, and open Shadow DOM
- public scenarios check stable external sites
- authenticated scenarios check profile carry-over and human-assist behavior for
  Gmail, ad platforms, and financial dashboards

Run the default deterministic suite:

```sh
go build -o bin/agent-browserd ./cmd/browserd
go build -o bin/browsercheck ./cmd/browsercheck
AGENT_BROWSER_WORKSPACE=agent-browser \
AGENT_BROWSER_PROFILE=agent-revitt \
AGENT_BROWSER_PROFILE_POLICY=../.mcplexer/config/browser-profiles.json \
./bin/agent-browserd --http 127.0.0.1:17310
./bin/browsercheck
```

Run public network scenarios too:

```sh
./bin/browsercheck --include-network
```

Run authenticated/manual scenarios only when the visible browser profile is the
intended workspace-approved profile:

```sh
./bin/browsercheck --include-network --include-auth --include-manual
```

Run the preserved-auth IntervalsPro profile scenario after the bridge extension
is installed in `max-gmail`:

```sh
AGENT_BROWSER_WORKSPACE=agent-browser \
AGENT_BROWSER_PROFILE=max-gmail \
AGENT_BROWSER_PROFILE_POLICY=../.mcplexer/config/browser-profiles.json \
agent-browserd --bridge --http 127.0.0.1:17310
./bin/browsercheck --include-network --include-auth --include-manual \
  --only auth-intervalspro-existing-profile-chat
```

Run the IntervalsPro OAuth/chat scenario:

```sh
ICU_USERNAME='max+tester@revitt.co' \
ICU_PASSWORD='...' \
./bin/browsercheck --include-network --include-auth --include-manual \
  --only auth-intervalspro-oauth-chat
```

Optional overrides:

- `INTERVALSPRO_CHAT_URL`, default `https://staging.intervals.pro/chat`
- `INTERVALSPRO_PROFILE_CHAT_URL`, default `https://intervals.pro/chat`
- `INTERVALSPRO_CHAT_HOST`, default `staging.intervals.pro`

Manual scenarios are expected to stop at login, passkey, MFA, CAPTCHA, fraud,
or account-risk prompts and ask the human to take over in the visible browser.
They are not bypass tests.

Run a single scenario:

```sh
./bin/browsercheck --only fixture-form-actions
```
