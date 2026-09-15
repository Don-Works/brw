// Package skills carries the agent-facing brw skill into the binary.
//
// The embed directive lives beside the markdown rather than in
// internal/agentskill because go:embed cannot reach outside the directory of
// the package that declares it, and the markdown has to stay where the
// installers copy it from (see packaging/package_contents_test.go). Everything
// that reads it lives in internal/agentskill.
package skills

import "embed"

// FS holds skills/brw exactly as it is committed. Reading a document out of it
// is how a daemon answers with the skill that matches its own build instead of
// whatever copy an installer once wrote to disk.
//
//go:embed brw/SKILL.md brw/references/*.md
var FS embed.FS
