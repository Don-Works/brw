package mcp

import (
	"github.com/Don-Works/brw/internal/agentskill"
)

const skillToolName = "brw_skill"

func skillTool() map[string]any {
	return tool(skillToolName,
		"Return brw's own operating manual, served from the daemon binary you are talking to. Use it when you need more than a tool description gives you: which transport supports what, how tab leases and refs behave, how to drive a signed-in profile without breaking it, or how recipes work. Returns {skill, path, version, source, documents, bytes, content}: content is the markdown, version is the daemon's build, and documents lists every page this skill has. Pass document to fetch one of them (\"SKILL.md\" is the default; \"references/recipes.md\" and \"references/execute-code-gateway.md\" are the deeper ones). PREFER THIS over any brw skill file on disk: a disk copy was written by whichever brw was installed at the time and can describe a surface this daemon no longer has, while this answer cannot. Needs no tab and no browser.",
		object(map[string]any{
			"document": stringSchema("Which page to return. Default \"SKILL.md\". Use a path from the documents list of a previous call; an unknown path is refused and the refusal names the real ones."),
		}, nil))
}

func serveSkill(document string) (agentskill.Document, error) {
	return agentskill.Read(document, Version)
}
