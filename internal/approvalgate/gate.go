package approvalgate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Don-Works/brw/internal/approval"
	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/siteconsent"
)

// Gate binds operator approval to one browser request and its observed state.
type Gate struct {
	Store          *approval.Store
	manager        browser.Controller
	mode           string
	operatorOrigin string
	consent        *siteconsent.Guard
}

// SetConsentGuard installs the permissions checked again on the pinned page.
func (g *Gate) SetConsentGuard(guard *siteconsent.Guard) {
	if g != nil {
		g.consent = guard
	}
}

// RequiredError describes a durable approval request without its private content.
type RequiredError struct {
	RequestID   string    `json:"approval_id"`
	Status      string    `json:"status"`
	ExpiresAt   time.Time `json:"expires_at"`
	StatusURL   string    `json:"status_url"`
	ApprovalURL string    `json:"approval_url"`
}

func (e *RequiredError) Error() string {
	if e.Status != "pending" && e.Status != "approved" {
		return fmt.Sprintf("approval_%s: request %s cannot execute; do not replay an uncertain outcome", e.Status, e.RequestID)
	}
	return fmt.Sprintf("approval_required: request %s is %s; a human must decide in the operator inbox; retry the exact action with approval_id only after approval", e.RequestID, e.Status)
}

// ErrorDetails returns the shared machine-readable refusal contract.
func ErrorDetails(err error) map[string]any {
	var required *RequiredError
	if errors.As(err, &required) {
		code := "approval_required"
		if required.Status != "pending" && required.Status != "approved" {
			code = "approval_" + required.Status
		}
		return map[string]any{"error": code, "code": code, "message": required.Error(), "approval_id": required.RequestID, "status": required.Status, "expires_at": required.ExpiresAt, "status_url": required.StatusURL, "approval_url": required.ApprovalURL, "retryable": false}
	}
	var failure *approval.Error
	if errors.As(err, &failure) {
		return map[string]any{"error": "approval_" + failure.Code, "code": "approval_" + failure.Code, "message": failure.Error(), "approval_id": failure.RequestID, "retryable": false}
	}
	return nil
}

// Status returns only the request's public lifecycle metadata.
func (g *Gate) Status(id string) (map[string]any, error) {
	if g == nil {
		return nil, errors.New("approvals are not enabled")
	}
	r, ok := g.Store.Get(id)
	if !ok {
		return nil, &approval.Error{Code: "not_found", RequestID: id, Message: "approval request not found"}
	}
	return map[string]any{"approval_id": r.ID, "status": r.Status, "expires_at": r.ExpiresAt}, nil
}

// New creates an opt-in approval gate without changing the browser controller.
func New(manager browser.Controller, store *approval.Store, mode string) (*Gate, error) {
	if manager == nil || store == nil {
		return nil, errors.New("approvals require a browser controller and durable store")
	}
	if mode == "" {
		mode = "risky"
	}
	if mode != "risky" && mode != "all" {
		return nil, errors.New("approval mode must be risky or all")
	}
	return &Gate{Store: store, manager: manager, mode: mode}, nil
}

// SetOperatorOrigin names the separately authenticated inbox on the browser host.
func (g *Gate) SetOperatorOrigin(origin string) { g.operatorOrigin = strings.TrimRight(origin, "/") }

// CheckURL keeps the operator inbox out of the controlled browser's tool surface.
func (g *Gate) CheckURL(raw string) error {
	if g == nil || g.operatorOrigin == "" {
		return nil
	}
	want, err := url.Parse(g.operatorOrigin)
	if err != nil {
		return errors.New("approval operator origin is invalid")
	}
	normalized, err := navpolicy.NormalizeNavigationURL(raw)
	if err != nil {
		if relative, parseErr := url.Parse(raw); parseErr == nil && !relative.IsAbs() && relative.Host == "" {
			return nil
		}
		return err
	}
	got, err := url.Parse(normalized)
	if err != nil {
		return err
	}
	if effectivePort(got) != effectivePort(want) {
		return nil
	}
	host := siteconsent.HostOfOrigin(got.String())
	ip := net.ParseIP(host)
	if host == siteconsent.HostOfOrigin(want.String()) || host == "localhost" || strings.HasSuffix(host, ".localhost") || ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) || ip == nil && numericHost(host) {
		return errors.New("operator approval UI is not accessible through browser tools; use a separate human browser")
	}
	return nil
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch u.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func numericHost(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	base := 10
	if strings.HasPrefix(last, "0x") {
		last, base = last[2:], 16
	}
	_, err := strconv.ParseUint(last, base, 64)
	return err == nil
}

// CheckTargets excludes the operator origin using tab metadata without page evaluation or approval capture.
func (g *Gate) CheckTargets(ctx context.Context, tool string, raw []byte) (context.Context, error) {
	if g == nil || g.operatorOrigin == "" {
		return ctx, nil
	}
	checks, err := siteconsent.Checks(tool, siteconsent.ParseProbe(raw))
	if err != nil {
		return ctx, err
	}
	var tabs []browser.Tab
	metadataCtx := ctx
	for _, check := range checks {
		if err := g.CheckURL(check.URL); err != nil {
			return ctx, err
		}
		destination, _ := url.Parse(check.URL)
		fromPage := check.FromTab || tool == "brw_replay_request" && check.URL != "" && destination != nil && !destination.IsAbs()
		if !fromPage {
			continue
		}
		if tabs == nil {
			var cancel context.CancelFunc
			metadataCtx, cancel = context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			tabs, err = g.manager.ListTabs(metadataCtx)
			if err != nil {
				return ctx, errors.New("approval cannot resolve the browser tab")
			}
		}
		id := check.TabID
		if id == "" {
			id = browser.TabIDFromContext(ctx)
		}
		var target browser.Tab
		if id == "" {
			target, _ = browser.UntargetedTab(metadataCtx, g.manager, tabs)
		} else {
			for _, tab := range tabs {
				if tab.ID == id {
					target = tab
					break
				}
			}
		}
		if target.ID == "" {
			return ctx, errors.New("approval needs an explicit open tab_id")
		}
		if target.URL == "" {
			return ctx, errors.New("approval cannot verify the browser tab origin")
		}
		if err := g.CheckURL(target.URL); err != nil {
			return ctx, err
		}
		if check.TabID == "" && browser.TabIDFromContext(ctx) == "" {
			ctx = browser.WithTabID(ctx, target.ID)
		}
	}
	return ctx, nil
}

// Check prepares or consumes approval and returns the pinned execution context and arguments.
func (g *Gate) Check(ctx context.Context, tool string, raw json.RawMessage, session string, label siteconsent.LabelFunc, required bool) (context.Context, json.RawMessage, error) {
	if g == nil {
		return ctx, raw, nil
	}
	if tool == "brw_approval_status" || tool == "brw_approval_resume" {
		return ctx, raw, nil
	}
	needed, err := g.needs(tool, raw, label)
	if err != nil {
		return ctx, raw, err
	}
	if !needed && !required {
		ctx, err = g.CheckTargets(ctx, tool, raw)
		if err != nil {
			return ctx, raw, err
		}
		if !bytes.Contains(raw, []byte(`"approval_id"`)) {
			return ctx, raw, nil
		}
		var probe map[string]json.RawMessage
		if json.Unmarshal(raw, &probe) != nil {
			return ctx, raw, errors.New("approval requires valid arguments")
		}
		if _, present := probe["approval_id"]; !present {
			return ctx, raw, nil
		}
	}
	if siteconsent.SequenceTools[tool] || tool == "brw_recipe_run" {
		return ctx, raw, errors.New("approval_split_required: run state-changing steps individually; an approval cannot safely resume or replay a whole batch, plan or recipe")
	}
	if len(raw) > 32768 {
		return ctx, raw, errors.New("approval request exceeds 32 KiB; use human takeover")
	}
	var args map[string]json.RawMessage
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return ctx, raw, errors.New("approval requires an argument object")
	}
	var id string
	if value, ok := args["approval_id"]; ok {
		if err := json.Unmarshal(value, &id); err != nil || id == "" {
			return ctx, raw, errors.New("approval_id must be a nonempty string")
		}
		delete(args, "approval_id")
	}
	clean, err := json.Marshal(args)
	if err != nil {
		return ctx, raw, err
	}
	tabID := browser.TabIDFromContext(ctx)
	if tabID == "" {
		tabs, err := g.manager.ListTabs(ctx)
		if err != nil {
			return ctx, clean, errors.New("approval cannot resolve the browser tab")
		}
		tab, ok := browser.UntargetedTab(ctx, g.manager, tabs)
		if !ok {
			return ctx, clean, errors.New("approval needs an explicit open tab_id")
		}
		tabID = tab.ID
		ctx = browser.WithTabID(ctx, tabID)
	}
	boundArgs := maps.Clone(args)
	boundArgs["tab_id"], _ = json.Marshal(tabID)
	bound, _ := json.Marshal(boundArgs)
	fingerprint := digest([]byte(tool + "\x00" + session + "\x00" + tabID + "\x00" + string(bound)))
	if id != "" {
		req, ok := g.Store.Get(id)
		if !ok {
			return ctx, clean, &approval.Error{Code: "not_found", RequestID: id, Message: "approval request not found"}
		}
		if req.Fingerprint != fingerprint {
			return ctx, clean, &approval.Error{Code: "binding_mismatch", RequestID: id, Message: "approval belongs to a different action, tab or session"}
		}
		if req.Status != "approved" {
			return ctx, clean, g.required(req)
		}
	} else {
		for _, req := range g.Store.List() {
			if req.Fingerprint == fingerprint && time.Now().Before(req.ExpiresAt) && req.Status != "stale" && req.Status != "expired" {
				return ctx, clean, g.required(req)
			}
		}
	}
	state, err := g.capture(ctx, tool, args)
	if err != nil {
		return ctx, clean, err
	}
	if err := g.CheckURL(state.URL); err != nil {
		return ctx, clean, err
	}
	if err := g.consent.Authorize(state.URL, siteconsent.ScopeAct); err != nil {
		return ctx, clean, err
	}
	if id != "" {
		_, err := g.Store.Consume(id, fingerprint, state.Digest)
		if err == nil {
			ctx = approval.WithExecution(ctx)
		}
		return ctx, clean, err
	}
	preview := sanitize(args)
	preview["page"] = state.Preview
	display, _ := json.Marshal(preview)
	parsed, _ := url.Parse(state.URL)
	origin := parsed.Scheme + "://" + parsed.Host
	summary := strings.ReplaceAll(strings.TrimPrefix(tool, "brw_"), "_", " ")
	if state.Target != "" {
		summary += ": " + state.Target
	}
	req, err := g.Store.Enqueue(approval.Request{Fingerprint: fingerprint, Tool: tool, Origin: origin, TabID: tabID, SessionID: session, Summary: summary, StateDigest: state.Digest, Arguments: display})
	if err != nil {
		return ctx, clean, err
	}
	return ctx, clean, g.required(req)
}

func (g *Gate) required(req approval.Request) error {
	return &RequiredError{RequestID: req.ID, Status: req.Status, ExpiresAt: req.ExpiresAt, StatusURL: g.operatorOrigin + "/api/approvals/" + req.ID, ApprovalURL: g.operatorOrigin + "/approvals"}
}

// ReadOnly identifies page observations without decoding or buffering arguments.
func ReadOnly(tool string) bool {
	rule, ok := siteconsent.ToolRules[tool]
	return ok && rule.Scope == siteconsent.ScopeRead && rule.Target == siteconsent.TargetPage && rule.Escalate == nil && !rule.ScriptCondition && tool != "brw_find"
}

func (g *Gate) needs(tool string, raw []byte, label siteconsent.LabelFunc) (bool, error) {
	if ReadOnly(tool) {
		return false, nil
	}
	probe := siteconsent.ParseProbe(raw)
	if siteconsent.SequenceTools[tool] && g.mode == "risky" {
		stepLabel := label
		for _, step := range probe.Steps {
			class, known := siteconsent.StepActions[step.Action]
			if !known {
				return true, nil
			}
			if class == siteconsent.StepNavigate || class == siteconsent.StepRetarget {
				if err := g.CheckURL(step.URL); err != nil {
					return false, err
				}
				stepLabel = nil
				continue
			}
			if class != siteconsent.StepAct {
				if step.Action == "wait" && strings.HasPrefix(strings.TrimSpace(step.Condition), "fn:") {
					return true, nil
				}
				continue
			}
			stepTool := "brw_" + step.Action
			if _, ok := siteconsent.ToolRules[stepTool]; !ok {
				return true, nil
			}
			args, _ := json.Marshal(step)
			needed, err := g.needs(stepTool, args, stepLabel)
			if err != nil || needed {
				return needed, err
			}
		}
		return false, nil
	}
	checks, err := siteconsent.Checks(tool, probe)
	if err != nil {
		return false, err
	}
	if tool == "brw_find" && probe.Action != "" {
		return true, nil
	}
	for _, check := range checks {
		if err := g.CheckURL(check.URL); err != nil {
			return false, err
		}
		if check.Scope != siteconsent.ScopeAct {
			continue
		}
		if g.mode == "all" || siteconsent.SequenceTools[tool] {
			return true, nil
		}
		for _, action := range check.Actions {
			if action.Ref != "" && label != nil {
				name := label(action.Ref)
				if action.FieldLabel {
					action.Request.Fields = append(action.Request.Fields, name)
				} else if action.Request.Label == "" {
					action.Request.Label = name
				}
			}
			words := strings.ToLower(action.Request.Label + " " + action.Request.Text)
			for _, signal := range []string{"delete", "remove account", "deactivate", "grant access", "revoke access", "permissions", "make admin", "transfer ownership"} {
				if strings.Contains(words, signal) {
					return true, nil
				}
			}
			if len(siteconsent.Classify(action.Request, siteconsent.CategorySet{})) != 0 {
				return true, nil
			}
		}
		switch tool {
		case "brw_focus", "brw_highlight":
			continue
		case "brw_type", "brw_fill", "brw_select", "brw_check":
			if probe.Query != "" || (probe.Ref != "" && label != nil && label(probe.Ref) != "") {
				continue
			}
		case "brw_click", "brw_click_text":
			name := strings.TrimSpace(probe.Text + " " + probe.Query)
			if probe.Ref != "" && label != nil {
				name += label(probe.Ref)
			}
			if strings.TrimSpace(name) != "" {
				continue
			}
		}
		return true, nil
	}
	return false, nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func sanitize(args map[string]json.RawMessage) map[string]any {
	out := make(map[string]any, len(args))
	for key, value := range args {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "authorization") || lower == "headers" || lower == "cookies" {
			out[key] = "[redacted; approval remains bound to the exact value]"
			continue
		}
		out[key] = value
	}
	return out
}
