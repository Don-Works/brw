# Vendored axe-core

| | |
| --- | --- |
| Upstream | <https://github.com/dequelabs/axe-core> |
| Version | 4.10.2 |
| File | `axe.min.js` |
| SHA-256 | `b511cd9dec01c76f4b2ad1723b66b6db37d4c2eb4ed199076e1829d9ee7b75e3` |
| Licence | Mozilla Public License 2.0 (`LICENSE`) |

`axe.min.js` is the unmodified minified distribution published by Deque Systems.
The corresponding Source Code Form is the tagged release at
<https://github.com/dequelabs/axe-core/releases/tag/v4.10.2>, which MPL-2.0
section 3.2 requires recipients be told how to obtain; this file is that notice.

axe-core is MPL-2.0. brw is AGPL-3.0. MPL-2.0 section 1.12 names the GNU
licences as Secondary Licenses and section 3.3 permits the combination, because
this copy carries no "Incompatible With Secondary Licenses" notice. The
`LICENSE` beside this file is the MPL-2.0 text that must travel with it.

## Updating

1. Download `axe.min.js` for the new tag from the upstream release or from
   `https://cdnjs.cloudflare.com/ajax/libs/axe-core/<version>/axe.min.js`.
2. Record `shasum -a 256 axe.min.js` in the table above.
3. Set `Version` in `axe.go` to the same string. `TestEmbeddedAxeMatchesItsDeclaredVersion`
   fails when the constant and the bundle disagree, so the two cannot drift.
4. Re-run the accessibility tests: rule ids and impact labels move between
   axe-core majors, and the fixtures assert on them.
