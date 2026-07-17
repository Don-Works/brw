package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/usagelog"
)

// Per-agent tab groups. Every tab the daemon opens for a session lands in a
// Chrome tab group derived from that session's identity, so concurrent agents
// each get a visible, named lane in the user's tab strip. The group is a
// VISUAL MIRROR of the lease table, never an enforcement boundary: group
// membership is user-mutable UI state (a human can drag tabs out, Chrome sync
// can rearrange it, popup windows cannot host groups at all), so exclusivity
// stays with the per-tab leases and grouping failures never fail an open.

type agentNameContextKey struct{}

// groupTitleAllowed matches the single characters permitted in a group title.
// Anything else (whitespace runs, punctuation, control chars) collapses to one
// dash, which both keeps titles readable and destroys header-smuggled noise.
var groupTitleAllowed = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

const maxGroupTitleLen = 24

// sanitizeAgentName reduces a caller-supplied display name to something safe
// to show in the user's tab strip. Empty result means "no usable name".
func sanitizeAgentName(name string) string {
	name = groupTitleAllowed.ReplaceAllString(strings.TrimSpace(name), "-")
	name = strings.Trim(name, "-._")
	if len(name) > maxGroupTitleLen {
		name = strings.Trim(name[:maxGroupTitleLen], "-._")
	}
	return name
}

// tabGroupColors is Chrome's full tabGroups color enum.
var tabGroupColors = []string{"grey", "blue", "red", "yellow", "green", "pink", "purple", "cyan", "orange"}

// ownerGroupOptions derives the session's tab-group title and color. The title
// leads with the agent's display name when one was provided (whoami), and
// always ends with a short owner-hash suffix: two agents that report the same
// display name (two claude-code sessions) must still get SEPARATE lanes,
// because the extension reuses same-title groups within a window. The color is
// a stable per-owner pick so a returning session keeps its visual identity.
func ownerGroupOptions(owner, agentName string) browser.TabGroupOptions {
	sum := sha256.Sum256([]byte("brw-agent-group-v1\x00" + owner))
	suffix := hex.EncodeToString(sum[:3])
	title := "brw-" + suffix
	if name := sanitizeAgentName(agentName); name != "" {
		title = name + "-" + suffix
	}
	return browser.TabGroupOptions{
		Name:  title,
		Color: tabGroupColors[int(sum[3])%len(tabGroupColors)],
	}
}

func requestAgentName(r *http.Request) string {
	return sanitizeAgentName(r.Header.Get(usagelog.HeaderAgentName))
}

func agentNameFrom(ctx context.Context) string {
	name, _ := ctx.Value(agentNameContextKey{}).(string)
	return name
}

// openInOwnerGroup opens url in the owner's per-agent tab group, falling back
// to a plain ungrouped open on transports that cannot group (direct CDP has no
// tab-group primitive; a chained daemon reports the same failure over HTTP).
// Grouping is organizational, so an open must never fail for want of it.
func (s *Server) openInOwnerGroup(ctx context.Context, url, owner string) (browser.OpenResult, error) {
	result, err := s.manager.OpenInGroup(ctx, url, ownerGroupOptions(owner, agentNameFrom(ctx)))
	if err != nil && isGroupingUnsupported(err) {
		return s.manager.Open(ctx, url)
	}
	return result, err
}

func isGroupingUnsupported(err error) bool {
	if errors.Is(err, browser.ErrTabGroupingUnsupported) {
		return true
	}
	return strings.Contains(err.Error(), "tab grouping is not supported")
}
