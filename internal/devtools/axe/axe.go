// Package axe carries the axe-core accessibility rule engine as an embedded asset.
package axe

import _ "embed"

// Version is the axe-core release Source was taken from.
const Version = "4.14.0"

// Source is the complete axe-core UMD bundle. Evaluating it in a page defines
// window.axe. It is a string rather than []byte because every consumer sends it
// to Runtime.evaluate as an expression.
//
//go:embed axe.min.js
var Source string
