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

// Version is the axe-core release Source was taken from. It is compared against
// the axe.version the page reports, so a stale copy left over from another
// tool's injection is replaced rather than silently used.
const Version = "4.10.2"

// Source is the complete axe-core UMD bundle. Evaluating it in a page defines
// window.axe. It is a string rather than []byte because every consumer sends it
// to Runtime.evaluate as an expression.
//
//go:embed axe.min.js
var Source string
