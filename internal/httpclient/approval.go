package httpclient

import (
	"context"
	"errors"
	"net/url"
	"regexp"
)

var approvalIDPattern = regexp.MustCompile(`^[a-f0-9]{32,64}$`)

// ApprovalStatus reads the lifecycle of one request on the browser host.
func (c *Controller) ApprovalStatus(ctx context.Context, id string) (map[string]any, error) {
	if !approvalIDPattern.MatchString(id) {
		return nil, errors.New("invalid approval request identifier")
	}
	var out map[string]any
	err := c.get(ctx, "/api/approvals/"+url.PathEscape(id), nil, &out)
	return out, err
}
