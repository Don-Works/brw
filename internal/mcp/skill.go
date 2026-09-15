package mcp

import (
	"github.com/Don-Works/brw/internal/agentskill"
)

// skillToolName serves the agent skill out of this binary.
//
// The alternative is the copy on disk that `brwctl setup` wrote, which is the
// version of brw the machine had the day setup last ran. Upgrade the daemon and
// that page keeps describing the old tool surface, with nothing to say it has
// gone stale. The served document comes from the running build and carries that
// build's version, so the page and the tools it describes are one thing.
const skillToolName = "brw_skill"

// skillTool is the catalogue entry. It is here rather than inline in tools() so
// the description stays next to the handler it describes.
func skillTool() map[string]any {
	return tool(skillToolName,
		"Return brw's own operating manual, served from the daemon binary you are talking to. Use it when you need more than a tool description gives you: which transport supports what, how tab leases and refs behave, how to drive a signed-in profile without breaking it, or how recipes work. Returns {skill, path, version, source, documents, bytes, content}: content is the markdown, version is the daemon's build, and documents lists every page this skill has. Pass document to fetch one of them (\"SKILL.md\" is the default; \"references/recipes.md\" and \"references/mcplexer-gateway.md\" are the deeper ones). PREFER THIS over any brw skill file on disk: a disk copy was written by whichever brw was installed at the time and can describe a surface this daemon no longer has, while this answer cannot. Needs no tab and no browser.",
		object(map[string]any{
			"document": stringSchema("Which page to return. Default \"SKILL.md\". Use a path from the documents list of a previous call; an unknown path is refused and the refusal names the real ones."),
		}, nil))
}

// serveSkill reads one page of the embedded skill, stamped with this build's
// version. Version is the same variable serverInfo and brw_identity report, so
// an agent can line the manual up against the daemon it came from.
func serveSkill(document string) (agentskill.Document, error) {
	return agentskill.Read(document, Version)
}
