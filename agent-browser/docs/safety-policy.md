# Safety and Access Policy

## Open-Web Commitment

agent-browser does **not** block any website or category by default. The browser
can visit any URL the user's Chrome profile can visit, including financial sites,
health portals, government services, and internal tools.

Safety is achieved through:

1. **Sensitive value redaction** — password, credit card, and authentication
   fields are redacted in snapshots and readability output. The `sensitive` flag
   is set on redacted elements.

2. **Auditability** — every action, snapshot, and tool call is logged with
   timestamps, refs, and outcomes. The MCP protocol itself is the audit trail.

3. **Profile policy** — the `profilepolicy` package allows operators to restrict
   which Chrome profiles can be controlled, but does not restrict which sites
   those profiles can visit.

4. **Kill switch** — the daemon can be stopped at any time. The extension shows
   a visual indicator when automation is active.

5. **Domain-drift warnings** — when a ref is recovered after a page re-render,
   the action result includes a `warning` field indicating the ref was rebound.

6. **Human handoff** — for MFA, CAPTCHA, fraud checks, and other
   human-verification steps, the model should pause and ask the user to complete
   the visible browser step. This is encoded in scenario `human_assist` fields.

## What We Do NOT Do

- No default blocklist of domains or categories
- No content classification or filtering
- No automatic refusal of financial or sensitive sites
- No stealth code to bypass site security, fraud checks, or CAPTCHA

## Redaction Scope

The following field types are redacted (value set to empty string, `sensitive: true`):

- `type="password"`
- `type="hidden"`
- `autocomplete` containing: `current-password`, `new-password`, `one-time-code`,
  `cc-number`, `cc-csc`, `cc-exp`, `cc-exp-month`, `cc-exp-year`, `cc-name`,
  `cc-type`, `cc-given-name`, `cc-family-name`

## Model Guidance

When the model encounters:
- **MFA prompts** → ask the user to complete the step
- **CAPTCHA** → ask the user to complete the step
- **Fraud warnings** → stop and report to the user
- **Payment forms** → proceed with caution, values are redacted in output
- **Login forms** → proceed after confirming credentials are from the user's profile
