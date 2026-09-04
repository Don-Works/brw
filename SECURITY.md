# Security policy

## Supported versions

Security fixes are made against the latest released version of `brw`. Upgrade
to the newest release before reporting an issue that may already be resolved.

## Reporting a vulnerability

Do not disclose a suspected vulnerability in a public issue. Use the
[private vulnerability reporting form](https://github.com/Don-Works/brw/security/advisories/new)
and include:

- the affected version and browser transport;
- the security boundary or data at risk;
- minimal reproduction steps; and
- any suggested mitigation.

Do not include credentials, cookies, private recipes, or captured customer/page
data. Ask for a secure transfer channel first if reproduction requires a
sensitive artifact.

Ordinary bugs and feature requests belong in the public issue tracker. The
expected deployment boundary is documented in
[`docs/remote-control.md`](docs/remote-control.md): the native HTTP listener is
not an Internet-facing authentication service and must remain on loopback or
behind an authenticated encrypted tunnel.
