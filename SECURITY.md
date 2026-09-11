# Security policy

## Supported versions

Security fixes are made against the latest released version of `brw`. Upgrade
to the newest release before reporting an issue that may already be resolved.

## Verifying a release

Every published artifact has a SHA256 in `SHA256SUMS.txt`, a CycloneDX SBOM, and
a GitHub build provenance attestation:

```sh
gh attestation verify <artifact> --repo Don-Works/brw
```

The checksum proves the file matches the release page. The attestation proves
this repository's release workflow built it from that tag. Needs `gh` 2.49+ and
`gh auth login`.

The macOS `.pkg`, the Windows `.msi` and the Linux `.deb`/`.rpm` are not yet
code-signed, so Gatekeeper reports an unidentified developer and SmartScreen
warns. [`docs/release-signing.md`](docs/release-signing.md) records what that
needs; the signing path is wired and waits on certificates.

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
