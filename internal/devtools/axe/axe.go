// Package axe carries the axe-core accessibility rule engine as an embedded
// asset.
//
// It is embedded rather than fetched because brwd would otherwise have to pull
// executable JavaScript off the public internet and inject it into a page it
// drives on the user's behalf, in that user's signed-in browser profile. A CDN
// outage would then break the audit, and a CDN compromise would run attacker
// code with the page's origin. The cost is ~540 KiB of binary and a manual
// version bump; see README.md for the update procedure.
package axe

import _ "embed"

// Version is the axe-core release Source was taken from. brw never replaces an
// engine a document already has, so this is not a floor on what runs: it is the
// version reported alongside the page engine's own whenever the two could
// differ, which is what lets a caller tell whether a rule id or an impact label
// came from this release or from whatever was already there.
const Version = "4.10.2"

// Source is the complete axe-core UMD bundle. Evaluating it in a page defines
// window.axe. It is a string rather than []byte because every consumer sends it
// to Runtime.evaluate as an expression.
//
//go:embed axe.min.js
var Source string
