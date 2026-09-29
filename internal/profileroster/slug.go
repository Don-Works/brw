package profileroster

import (
	"fmt"
	"regexp"
	"strings"
)

var slugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Slug normalises a profile name to a filesystem- and namespace-safe id.
func Slug(name string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	if !slugPattern.MatchString(s) {
		return "", fmt.Errorf("profile name %q must be a lowercase slug (a letter, then letters, digits or hyphens)", name)
	}
	return s, nil
}

// Namespace is the MCP namespace a gateway mints for a profile slug.
func Namespace(slug string) string {
	return "brw_" + strings.ReplaceAll(slug, "-", "_")
}

// Workspace is the brwd --workspace name for a slug, one to one with the profile.
func Workspace(slug string) string {
	return "brw-" + slug
}
