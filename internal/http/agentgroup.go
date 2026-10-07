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

type agentNameContextKey struct{}

var groupTitleAllowed = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

const maxGroupTitleLen = 24

func sanitizeAgentName(name string) string {
	name = groupTitleAllowed.ReplaceAllString(strings.TrimSpace(name), "-")
	name = strings.Trim(name, "-._")
	if len(name) > maxGroupTitleLen {
		name = strings.Trim(name[:maxGroupTitleLen], "-._")
	}
	return name
}

var tabGroupColors = []string{"grey", "blue", "red", "yellow", "green", "pink", "purple", "cyan", "orange"}

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
	return strings.Contains(err.Error(), browser.ErrTabGroupingUnsupported.Error())
}
