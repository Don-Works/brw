package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Don-Works/brw/internal/browser"
	"github.com/Don-Works/brw/internal/brwidentity"
	"github.com/Don-Works/brw/internal/navpolicy"
	"github.com/Don-Works/brw/internal/readability"
	"github.com/Don-Works/brw/internal/snapshot"
	"github.com/Don-Works/brw/internal/usagelog"
)

// Version is the build version reported over MCP initialize (serverInfo.version).
// It is injected at build time via -ldflags "-X .../internal/mcp.Version=<v>"
// (see the Makefile and scripts/package-*), so the version an agent sees always
// matches the binary it is talking to instead of a hand-edited constant that
// silently drifts from the released build. Defaults to "dev" for a plain
// `go build` / `go test`.
var Version = "dev"

type Server struct {
	manager     browser.Controller
	toolProfile string // "all" (default) or "core"
	navPolicy   *navpolicy.Policy
	idleExit    time.Duration
	usage       *usagelog.Recorder
	sessionID   string
	identity    brwidentity.Identity
	console     consoleBuffer
	unlocked    unlockedTools

	// notify pushes a JSON-RPC notification to the client. Serve installs it;
	// it is nil before Serve runs and on transports that cannot push.
	notifyMu sync.Mutex
	notify   func(method string, params any)
}

// SetIdentity records which workspace/profile/browser this server drives, so the
// brw_identity tool can answer "which browser am I controlling?" over MCP. An
// agent enumerates the brw_* namespaces (one per browser profile) and calls
// brw_identity on each to map a namespace to a concrete profile instead of
// guessing from the namespace label. Empty (the default) means the daemon was
// launched without a profile policy; brw_identity still answers, just sparsely.
func (s *Server) SetIdentity(identity brwidentity.Identity) {
	s.identity = identity
}

// SetIdleExit makes Serve return cleanly after no request has arrived for d.
// Zero (the default) disables it. Intended for the stateless --upstream-http
// proxy mode where the process is disposable and a supervisor (or the next
// session) respawns it on demand: a parent that abandons the process without
// closing its stdin would otherwise pin it alive forever.
func (s *Server) SetIdleExit(d time.Duration) {
	s.idleExit = d
}

// SetUsageRecorder installs the metadata-only operational ledger. Tool
// arguments and result content are never passed to it; errors are reduced to a
// stable category and one-way fingerprint.
func (s *Server) SetUsageRecorder(recorder *usagelog.Recorder) {
	s.usage = recorder
}

// SetNavigationPolicy installs an opt-in allow/deny guardrail enforced on
// URL-opening tools (brw_open, brw_open_incognito) and brw_replay_request. A nil
// or empty policy is a no-op.
func (s *Server) SetNavigationPolicy(p *navpolicy.Policy) {
	s.navPolicy = p
}

const (
	defaultSnapshotMode  = "frontier"
	defaultSnapshotLimit = 40
	defaultFindLimit     = 20
)

// coreToolNames is the lean, common-flow tool surface. It hides the long tail
// behind the default "all" profile while keeping the verbs an agent needs for
// common read/click/type/select/navigate/scroll/drag/upload/hover flows.
var coreToolNames = map[string]bool{
	"brw_identity":       true,
	"brw_open":           true,
	"brw_list_tabs":      true,
	"brw_focus_tab":      true,
	"brw_read":           true,
	"brw_snapshot":       true,
	"brw_find":           true,
	"brw_click":          true,
	"brw_click_text":     true,
	"brw_type":           true,
	"brw_fill":           true,
	"brw_select":         true,
	"brw_press":          true,
	"brw_scroll":         true,
	"brw_hover":          true,
	"brw_drag":           true,
	"brw_upload_file":    true,
	"brw_navigate":       true,
	"brw_navigate_to":    true,
	"brw_wait_for":       true,
	"brw_batch":          true,
	"brw_observe":        true,
	"brw_screenshot":     true,
	"brw_emulate_device": true,
}

// minimalToolNames is the smallest surface that still completes ordinary web
// work: find a page, see its controls, act on them, confirm the result. Every
// tool omitted here remains callable — the profile only narrows what tools/list
// advertises, and the catalogue is re-sent on every request, so a narrower
// profile is a per-turn saving for the whole session.
//
// brw_batch earns its place despite the size of its schema: it collapses a
// multi-step flow into one round trip, which saves more than its definition
// costs. brw_observe earns its place because it is what an agent reads instead
// of re-snapshotting.
var minimalToolNames = map[string]bool{
	"brw_open":        true,
	"brw_navigate_to": true,
	"brw_read":        true,
	"brw_snapshot":    true,
	"brw_find":        true,
	"brw_click":       true,
	"brw_fill":        true,
	"brw_select":      true,
	"brw_press":       true,
	"brw_wait_for":    true,
	"brw_observe":     true,
	"brw_batch":       true,
}

// toolProfiles maps a profile name to its allowed set. A nil set means every
// tool is advertised.
var toolProfiles = map[string]map[string]bool{
	"all":     nil,
	"core":    coreToolNames,
	"minimal": minimalToolNames,
	// auto starts from the minimal set and grows as brw_tools discloses more.
	autoProfile: minimalToolNames,
}

// ToolProfileNames lists the selectable profiles, for CLI help and validation.
func ToolProfileNames() []string {
	names := make([]string, 0, len(toolProfiles))
	for name := range toolProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ValidToolProfile reports whether name selects a known profile.
func ValidToolProfile(name string) bool {
	_, ok := toolProfiles[name]
	return ok
}

func New(manager browser.Controller) *Server {
	return &Server{manager: manager, toolProfile: "all", sessionID: usagelog.NewID()}
}

// checkNavPolicy enforces the optional navigation guardrail. Returns nil when no
// policy is set or the URL is permitted. A relative replay URL (no host) passes;
// the policy only gates absolute network destinations.
func (s *Server) checkNavPolicy(rawURL string) error {
	if s.navPolicy.Empty() {
		return nil
	}
	return s.navPolicy.Check(rawURL)
}

// prepareNavigation canonicalizes the exact URL that will be handed to the
// controller, then applies the navigation policy to that same value.
func (s *Server) prepareNavigation(rawURL string) (string, error) {
	return s.navPolicy.CheckNavigation(rawURL)
}

// NewWithToolProfile builds a server exposing only the named tool profile in
// tools/list ("core" for the lean surface, anything else for the full surface).
// All tools remain callable regardless of profile; the profile only narrows what
// tools/list advertises.
func NewWithToolProfile(manager browser.Controller, profile string) *Server {
	if profile == "" {
		profile = "all"
	}
	return &Server{manager: manager, toolProfile: profile, sessionID: usagelog.NewID()}
}

// advertisedTools returns the tool list for tools/list, narrowed to the active
// profile. "core" filters to coreToolNames; any other value returns everything.
func (s *Server) advertisedTools() []map[string]any {
	all := tools()
	allowed, known := toolProfiles[s.toolProfile]
	// An unknown profile advertises everything rather than nothing: a typo in a
	// client config should degrade to the full surface, never to a mute server.
	if !known || allowed == nil {
		return all
	}
	auto := s.toolProfile == autoProfile
	// Snapshot the unlocked set once. Taking the lock per tool let an unlock
	// landing mid-iteration produce a catalogue that omitted a tool unlocked
	// before the scan reached it while including one unlocked after — a
	// self-inconsistent list, and one the client has no reason to refetch.
	unlocked := map[string]bool{}
	if auto {
		unlocked = s.unlocked.snapshot()
	}
	filtered := make([]map[string]any, 0, len(allowed)+len(unlocked)+1)
	if auto {
		filtered = append(filtered, discoveryTool())
	}
	for _, t := range all {
		name, _ := t["name"].(string)
		if allowed[name] || unlocked[name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// supportsListChanged reports whether this server can grow its catalogue mid
// session. Only auto mode does, and advertising the capability otherwise would
// promise a notification that never comes.
func (s *Server) supportsListChanged() bool {
	return s.toolProfile == autoProfile
}

// setNotifier installs the push channel Serve owns.
func (s *Server) setNotifier(fn func(method string, params any)) {
	s.notifyMu.Lock()
	s.notify = fn
	s.notifyMu.Unlock()
}

// announceToolsChanged tells the client its catalogue is stale. Best effort: a
// client that never refetches still works, because unadvertised tools remain
// callable.
func (s *Server) announceToolsChanged() {
	s.notifyMu.Lock()
	fn := s.notify
	s.notifyMu.Unlock()
	if fn != nil {
		fn("notifications/tools/list_changed", nil)
	}
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// notification is a JSON-RPC message with no id, which the client must not
// answer. Params is omitted when nil so a bare notification stays bare.
type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mimeType,omitempty"`
}

// inbound carries one stdin message (or terminal read error) from the reader
// goroutine to the Serve loop, along with the stdio framing mode in effect
// when it was read.
type inbound struct {
	body []byte
	mode stdioMode
	err  error
}

// ErrIdleExit is returned by Serve when the idle-exit deadline (SetIdleExit)
// elapses with no incoming request. Callers should treat it as a clean,
// intentional shutdown, not a failure.
var ErrIdleExit = errors.New("mcp: idle-exit deadline reached with no requests")

func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	serveCtx, stop := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		stop()
		workers.Wait()
	}()

	// Stdin is read on its own goroutine so the loop can react to ctx
	// cancellation (SIGTERM/SIGINT, parent death) while a read is blocked.
	// Before this, a supervisor's polite SIGTERM was swallowed: NotifyContext
	// cancelled ctx, but Serve sat in a blocking read forever and the process
	// leaked — one zombie per abandoned session. The goroutine reads at most
	// one message ahead (unbuffered channel). Requests are dispatched concurrently
	// after framing so a brw_cancel (or MCP cancellation notification) arriving on
	// this same stdio stream can interrupt a long plan instead of waiting behind it.
	msgs := make(chan inbound)
	go func() {
		reader := bufio.NewReader(in)
		mode := stdioModeUnknown
		for {
			body, nextMode, err := readMessage(reader, mode)
			if nextMode != stdioModeUnknown {
				mode = nextMode
			}
			select {
			case msgs <- inbound{body: body, mode: mode, err: err}:
			case <-serveCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	// Multiple request goroutines may finish out of order (legal JSON-RPC), but a
	// frame itself must remain atomic. This lock prevents interleaved JSON or
	// Content-Length headers on the shared writer.
	var writeMu sync.Mutex
	write := func(mode stdioMode, value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeMessage(out, mode, value)
	}

	// Notifications are written on the same framed stream as responses, so they
	// have to follow whatever framing the client established. Track the mode of
	// the last message read; before the first one, line framing is the safe
	// default because it is what a bare-JSON client sends.
	var modeMu sync.Mutex
	notifyMode := stdioModeLine
	s.setNotifier(func(method string, params any) {
		modeMu.Lock()
		mode := notifyMode
		modeMu.Unlock()
		_ = write(mode, notification{JSONRPC: "2.0", Method: method, Params: params})
	})
	defer s.setNotifier(nil)

	type activeRequest struct {
		cancel context.CancelFunc
	}
	var inflightMu sync.Mutex
	inflight := map[string]*activeRequest{}
	type completion struct{ err error }
	completed := make(chan completion)

	// Idle-exit timer: fires only when enabled AND no message has arrived for
	// s.idleExit. The deadline check re-arms on wakeup so a fire queued while a
	// request was being handled cannot cause a premature exit.
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	lastActivity := time.Now()
	if s.idleExit > 0 {
		idleTimer = time.NewTimer(s.idleExit)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}

	active := 0
	inputClosed := false
	input := (<-chan inbound)(msgs)
	for {
		if inputClosed && active == 0 {
			return nil
		}
		var msg inbound
		select {
		case <-ctx.Done():
			return ctx.Err()
		case done := <-completed:
			active--
			lastActivity = time.Now()
			if done.err != nil {
				return done.err
			}
			if idleTimer != nil {
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(s.idleExit)
			}
		case <-idleC:
			if active > 0 {
				idleTimer.Reset(s.idleExit)
				continue
			}
			// The tick may have been queued while a request was being handled
			// (lastActivity moved since the timer was armed) — re-arm for the
			// remainder instead of exiting under an active client.
			if remaining := s.idleExit - time.Since(lastActivity); remaining > 0 {
				idleTimer.Reset(remaining)
				continue
			}
			return ErrIdleExit
		case msg = <-input:
		}
		if msg.err != nil {
			if msg.err == io.EOF {
				inputClosed = true
				input = nil
				continue
			}
			return msg.err
		}
		if msg.mode != stdioModeUnknown {
			modeMu.Lock()
			notifyMode = msg.mode
			modeMu.Unlock()
		}
		if len(bytes.TrimSpace(msg.body)) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(msg.body, &req); err != nil {
			if err := write(msg.mode, response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()}}); err != nil {
				return err
			}
			lastActivity = time.Now()
			continue
		}

		// Notifications have no response. MCP's cancellation notification targets
		// the JSON-RPC request id, so cancel its derived context immediately.
		if len(req.ID) == 0 {
			if key := cancelledRequestKey(req); key != "" {
				inflightMu.Lock()
				entry := inflight[key]
				inflightMu.Unlock()
				if entry != nil {
					entry.cancel()
				}
			}
			lastActivity = time.Now()
			continue
		}

		requestCtx, cancelRequest := context.WithCancel(serveCtx)
		key := requestIDKey(req.ID)
		entry := &activeRequest{cancel: cancelRequest}
		inflightMu.Lock()
		if previous := inflight[key]; previous != nil {
			previous.cancel()
		}
		inflight[key] = entry
		inflightMu.Unlock()

		active++
		workers.Add(1)
		go func(req request, mode stdioMode, key string, entry *activeRequest) {
			defer workers.Done()
			defer cancelRequest()
			result, rpcErr := s.handle(requestCtx, req.Method, req.Params)
			err := write(mode, response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr})
			inflightMu.Lock()
			if inflight[key] == entry {
				delete(inflight, key)
			}
			inflightMu.Unlock()
			select {
			case completed <- completion{err: err}:
			case <-serveCtx.Done():
			}
		}(req, msg.mode, key, entry)
	}
}

func requestIDKey(raw json.RawMessage) string {
	return string(bytes.TrimSpace(raw))
}

func cancelledRequestKey(req request) string {
	if req.Method != "notifications/cancelled" && req.Method != "$/cancelRequest" {
		return ""
	}
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		ID        json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return ""
	}
	if len(params.RequestID) != 0 {
		return requestIDKey(params.RequestID)
	}
	return requestIDKey(params.ID)
}

type stdioMode int

const (
	stdioModeUnknown stdioMode = iota
	stdioModeFramed
	stdioModeLine
)

func readMessage(r *bufio.Reader, mode stdioMode) ([]byte, stdioMode, error) {
	if mode == stdioModeLine {
		line, err := readLineAllowEOF(r)
		if err != nil {
			return nil, mode, err
		}
		return bytes.TrimSpace(line), mode, nil
	}

	line, err := readLineAllowEOF(r)
	if err != nil {
		return nil, mode, err
	}
	trimmed := strings.TrimSpace(string(line))
	for trimmed == "" {
		line, err = readLineAllowEOF(r)
		if err != nil {
			return nil, mode, err
		}
		trimmed = strings.TrimSpace(string(line))
	}

	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || !strings.Contains(trimmed, ":") {
		return []byte(trimmed), stdioModeLine, nil
	}

	headers := map[string]string{}
	for {
		if trimmed == "" {
			break
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			return nil, mode, fmt.Errorf("invalid MCP stdio header %q", trimmed)
		}
		headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		line, err = readLineAllowEOF(r)
		if err != nil {
			return nil, mode, err
		}
		trimmed = strings.TrimSpace(string(line))
	}

	rawLen, ok := headers["content-length"]
	if !ok {
		return nil, mode, errors.New("missing Content-Length header")
	}
	length, err := strconv.Atoi(rawLen)
	if err != nil || length < 0 {
		return nil, mode, fmt.Errorf("invalid Content-Length %q", rawLen)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, mode, err
	}
	return body, stdioModeFramed, nil
}

func readLineAllowEOF(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err == nil {
		return line, nil
	}
	if err == io.EOF && len(line) > 0 {
		return line, nil
	}
	return nil, err
}

func writeMessage(w io.Writer, mode stdioMode, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if mode == stdioModeFramed {
		if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	return err
}

func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		// Forward the MCP client's display name to the shared daemon (when the
		// manager is the HTTP proxy), where it titles this session's per-agent
		// Chrome tab group. Best-effort identity garnish — never fails initialize.
		var init struct {
			ClientInfo struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		if len(params) > 0 && json.Unmarshal(params, &init) == nil && init.ClientInfo.Name != "" {
			if namer, ok := s.manager.(interface{ SetAgentName(string) }); ok {
				namer.SetAgentName(init.ClientInfo.Name)
			}
		}
		return map[string]any{
			"protocolVersion": "2025-06-18",
			"serverInfo": map[string]any{
				"name":    "brw",
				"version": Version,
			},
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": s.supportsListChanged()},
			},
		}, nil
	case "tools/list":
		return map[string]any{"tools": s.advertisedTools()}, nil
	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &call); err != nil {
			return nil, invalid(err)
		}
		started := time.Now()
		result, rpcErr := s.callTool(ctx, call.Name, call.Arguments)
		s.recordToolUsage(call.Name, started, result, rpcErr)
		return result, rpcErr
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

// activeTabResolver is the optional capability a Controller may implement to
// resolve the genuinely focused tab once per top-level tool call. Only the
// extension Bridge implements it (its per-call active-tab resolution is the
// multiplier we are collapsing); the direct-CDP Manager and the HTTP proxy do
// not, so they are left entirely unchanged.
type activeTabResolver interface {
	ResolveActiveTabID(context.Context) string
}

// tabAgnosticTools lists tools that must NOT trigger a one-shot active-tab
// resolution: tab-management verbs (which manage focus themselves) and the
// batch/plan runners (which resolve once internally and re-pin per step after a
// focus_tab/open). list_tabs in particular must stay free of the extra round
// trip the task brief calls out.
var tabAgnosticTools = map[string]bool{
	"brw_identity":        true,
	"brw_list_tabs":       true,
	"brw_list_tab_groups": true,
	"brw_focus_tab":       true,
	"brw_close_tab":       true,
	"brw_open":            true,
	"brw_open_incognito":  true,
	"brw_close_context":   true,
	"brw_group_tabs":      true,
	"brw_ungroup_tabs":    true,
	"brw_batch":           true,
	"brw_plan":            true,
	"brw_cancel":          true,
	"brw_trace":           true,
	"brw_clear_trace":     true,
}

// pinActiveTabForTool resolves the active tab once (when the controller supports
// it and the tool acts on the active tab) and pins it into the context via
// browser.WithTabID. A no-op when the controller does not implement
// activeTabResolver, the tool is tab-management/batch, or resolution fails.
func pinActiveTabForTool(ctx context.Context, manager browser.Controller, name string) context.Context {
	if tabAgnosticTools[name] {
		return ctx
	}
	resolver, ok := manager.(activeTabResolver)
	if !ok {
		return ctx
	}
	if tabID := resolver.ResolveActiveTabID(ctx); tabID != "" {
		return browser.WithTabID(ctx, tabID)
	}
	return ctx
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) (any, *rpcError) {
	name = canonicalToolName(name)

	// Extract optional tab_id from any tool call and inject into context
	var tabProbe struct {
		TabID string `json:"tab_id"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &tabProbe)
	}
	if tabProbe.TabID != "" {
		ctx = browser.WithTabID(ctx, tabProbe.TabID)
	} else {
		// No explicit tab_id: for tools that act on the active tab, resolve it
		// ONCE here and pin it into the context so every downstream page call
		// short-circuits instead of re-resolving the active tab per sub-call
		// (the extension bridge otherwise issues get_active_tab_id 3-11x per
		// logical tool call). Tab-management tools and the batch/plan runners are
		// excluded: they manage focus themselves or pin internally per step.
		ctx = pinActiveTabForTool(ctx, s.manager, name)
	}
	switch name {
	case discoveryToolName:
		var req struct {
			Query string `json:"query"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		result, err := s.discoverTools(req.Query)
		if err != nil {
			return nil, invalid(err)
		}
		// Tell the client its catalogue moved, but only when it actually did:
		// a notification per search would churn a client that refetches on it.
		if result.Unlocked > 0 && s.supportsListChanged() {
			s.announceToolsChanged()
		}
		return toolJSON(result, nil)
	case "brw_identity":
		// Process-level config, deliberately independent of the browser: it
		// answers even when no bridge is connected or the browser has zero
		// windows, because its whole job is to tell an agent which profile this
		// namespace drives before it touches a tab. tabAgnosticTools keeps it
		// off the active-tab resolution path so it never blocks on the bridge.
		return toolJSON(map[string]any{
			"identity":  s.identity,
			"version":   Version,
			"connected": !s.identity.Empty(),
		}, nil)
	case "brw_open":
		var req struct {
			URL        string `json:"url"`
			Group      string `json:"group"`
			GroupID    string `json:"group_id"`
			GroupColor string `json:"group_color"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		normalizedURL, err := s.prepareNavigation(req.URL)
		if err != nil {
			return toolError(err), nil
		}
		req.URL = normalizedURL
		if req.Group != "" || req.GroupID != "" {
			return toolJSON(s.manager.OpenInGroup(ctx, req.URL, browser.TabGroupOptions{
				GroupID: req.GroupID,
				Name:    req.Group,
				Color:   req.GroupColor,
			}))
		}
		return toolJSON(s.manager.Open(ctx, req.URL))
	case "brw_open_incognito":
		var req struct {
			URL string `json:"url"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		normalizedURL, err := s.prepareNavigation(req.URL)
		if err != nil {
			return toolError(err), nil
		}
		req.URL = normalizedURL
		return toolJSON(s.manager.OpenIncognito(ctx, req.URL))
	case "brw_close_context":
		var req struct {
			BrowserContextID       string `json:"context_id"`
			LegacyBrowserContextID string `json:"browser_context_id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.CloseContext(ctx, contextIDArg(req.BrowserContextID, req.LegacyBrowserContextID)))
	case "brw_list_tabs":
		return toolJSON(s.manager.ListTabs(ctx))
	case "brw_list_tab_groups":
		return toolJSON(s.manager.ListTabGroups(ctx))
	case "brw_focus_tab":
		var req struct {
			ID    string `json:"id"`
			TabID string `json:"tab_id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.FocusTab(ctx, tabIDArg(req.TabID, req.ID)))
	case "brw_close_tab":
		var req struct {
			ID    string `json:"id"`
			TabID string `json:"tab_id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.CloseTab(ctx, tabIDArg(req.TabID, req.ID)))
	case "brw_emulate_device":
		var req browser.DeviceEmulationOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.EmulateDevice(ctx, req))
	case "brw_read":
		var req readability.ReadOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if err := req.Validate(); err != nil {
			return nil, invalid(err)
		}
		read, err := s.manager.Read(ctx)
		if err != nil {
			return toolError(err), nil
		}
		// An unmatched section is an argument error, not a silently empty read:
		// the agent asked for a part of the page that is not there, and needs to
		// know which parts are.
		if req.Section != "" {
			// Distinguish "that section is not on this page" from "this browser
			// backend cannot address sections at all". An older upstream daemon
			// in proxy mode returns headings without offsets; treating that as a
			// miss would send the agent hunting for a name that is right there.
			if !readability.SectionsAddressable(read.Headings) {
				return nil, invalid(fmt.Errorf(
					"section addressing is unavailable on this backend (the page read carried no heading offsets); page with offset/max_chars instead, or update the brw daemon this session proxies to"))
			}
			if _, ok := readability.FindSectionSpan(read.Headings, len([]rune(read.Main)), req.Section); !ok {
				return nil, invalid(fmt.Errorf("no section matching %q; available sections: %s",
					req.Section, strings.Join(readability.SectionNames(read.Headings), ", ")))
			}
		}
		return toolJSON(readability.Window(read, req), nil)
	case "brw_read_data":
		return toolJSON(s.manager.ReadData(ctx))
	case "brw_snapshot":
		var req snapshot.SnapshotOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		req = normalizeMCPSnapshotOptions(req)
		snap, err := s.manager.Snapshot(ctx, req)
		if err != nil {
			return toolError(err), nil
		}
		if strings.EqualFold(req.Format, "compact") {
			return map[string]any{"content": []toolContent{{Type: "text", Text: snapshot.RenderCompact(snap)}}}, nil
		}
		return toolJSON(snap, nil)
	case "brw_find":
		var req snapshot.FindOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		req = normalizeMCPFindOptions(req)
		return toolJSON(s.manager.Find(ctx, req))
	case "brw_click":
		var req struct {
			Ref        string   `json:"ref"`
			X          *float64 `json:"x"`
			Y          *float64 `json:"y"`
			Button     string   `json:"button"`
			ClickCount int      `json:"click_count"`
			Snapshot   bool     `json:"snapshot"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if req.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		// Plain left single-click on a ref keeps the fast in-page click path.
		// Any non-default button/count, or a coordinate target, routes through
		// the decomposed CDP click so right/double/triple/middle clicks and
		// canvas coordinate clicks all share one tool.
		if browser.IsDefaultLeftSingleRefClick(req.Button, req.ClickCount, req.Ref, req.X, req.Y) {
			return toolJSON(s.manager.Click(ctx, req.Ref))
		}
		return toolJSON(s.manager.ClickButton(ctx, browser.ClickButtonOptions{
			MousePoint: browser.MousePoint{Ref: req.Ref, X: req.X, Y: req.Y},
			Button:     req.Button,
			ClickCount: req.ClickCount,
		}))
	case "brw_drag":
		var req struct {
			From   browser.MousePoint `json:"from"`
			To     browser.MousePoint `json:"to"`
			Steps  int                `json:"steps"`
			Button string             `json:"button"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		opts := browser.DragOptions{
			From:   req.From,
			To:     req.To,
			Steps:  req.Steps,
			Button: req.Button,
		}
		if err := opts.Validate(); err != nil {
			return toolError(err), nil
		}
		return toolJSON(s.manager.Drag(ctx, opts))
	case "brw_mouse_down":
		opts, err := parseMouseButtonArgs(args)
		if err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.MouseDown(ctx, opts))
	case "brw_mouse_up":
		opts, err := parseMouseButtonArgs(args)
		if err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.MouseUp(ctx, opts))
	case "brw_click_text":
		var req snapshot.ClickTextOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		var snapReq struct {
			Snapshot bool `json:"snapshot"`
		}
		_ = json.Unmarshal(args, &snapReq)
		if snapReq.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(s.manager.ClickText(ctx, req))
	case "brw_navigate":
		var req struct {
			Direction string `json:"direction"`
			Snapshot  bool   `json:"snapshot"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if req.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(s.manager.Navigate(ctx, req.Direction))
	case "brw_navigate_to":
		var req struct {
			URL      string `json:"url"`
			Snapshot bool   `json:"snapshot"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		normalizedURL, err := s.prepareNavigation(req.URL)
		if err != nil {
			return toolError(err), nil
		}
		req.URL = normalizedURL
		if req.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(s.manager.NavigateTo(ctx, req.URL))
	case "brw_hover":
		var req struct {
			Ref      string `json:"ref"`
			Snapshot bool   `json:"snapshot"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if req.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(s.manager.Hover(ctx, req.Ref))
	case "brw_type":
		var req struct {
			Ref      string `json:"ref"`
			Text     string `json:"text"`
			Snapshot bool   `json:"snapshot"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if req.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(s.manager.Type(ctx, req.Ref, req.Text))
	case "brw_fill":
		req := snapshot.FillOptions{Replace: true}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		// Accept Playwright-style {value:"…"} as an alias for text so agents
		// don't silently clear the field (empty text + replace:true).
		req.Text = req.EffectiveText()
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(args, &raw)
		if _, hasText := raw["text"]; !hasText {
			if _, hasValue := raw["value"]; !hasValue {
				return toolError(errors.New("fill requires text (or value as a Playwright-style alias for text)")), nil
			}
		}
		var snapReq struct {
			Snapshot bool `json:"snapshot"`
		}
		_ = json.Unmarshal(args, &snapReq)
		if snapReq.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(s.manager.Fill(ctx, req))
	case "brw_upload_file":
		var req snapshot.UploadOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		// The url source makes the daemon fetch an arbitrary http(s) target on
		// the browser host (SSRF reach). Gate it with the same navigation policy
		// as brw_open so the allowlist/blocklist can't be sidestepped via upload.
		if req.URL != "" {
			if err := s.checkNavPolicy(req.URL); err != nil {
				return toolError(err), nil
			}
		}
		return toolJSON(s.manager.UploadFile(ctx, req))
	case "brw_select":
		var req struct {
			Ref      string `json:"ref"`
			Value    string `json:"value"`
			Snapshot bool   `json:"snapshot"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if req.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(s.manager.Select(ctx, req.Ref, req.Value))
	case "brw_press":
		var req struct {
			Key      string `json:"key"`
			Repeat   int    `json:"repeat"`
			Snapshot bool   `json:"snapshot"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		repeat, err := normalizeRepeat(req.Repeat)
		if err != nil {
			return nil, invalid(err)
		}
		if req.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(repeatAction(ctx, repeat, func(ctx context.Context) (browser.ActionResult, error) {
			return s.manager.Press(ctx, req.Key)
		}))
	case "brw_scroll":
		var req struct {
			Direction string `json:"direction"`
			Repeat    int    `json:"repeat"`
			Snapshot  bool   `json:"snapshot"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		repeat, err := normalizeRepeat(req.Repeat)
		if err != nil {
			return nil, invalid(err)
		}
		if req.Snapshot {
			ctx = browser.WithWantSnapshot(ctx)
		}
		return toolJSON(repeatAction(ctx, repeat, func(ctx context.Context) (browser.ActionResult, error) {
			return s.manager.Scroll(ctx, req.Direction)
		}))
	case "brw_screenshot":
		var req struct {
			Annotate bool   `json:"annotate"`
			Ref      string `json:"ref"`
			Region   *struct {
				X      float64 `json:"x"`
				Y      float64 `json:"y"`
				Width  float64 `json:"width"`
				Height float64 `json:"height"`
			} `json:"region"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		// A ref or region implies an annotated (Set-of-Marks) crop even if annotate
		// was omitted — the whole point of the crop is the ref legend.
		if req.Annotate || strings.TrimSpace(req.Ref) != "" || req.Region != nil {
			aopts := browser.AnnotatedScreenshotOptions{Mode: "frontier", Ref: req.Ref}
			if req.Region != nil {
				aopts.Region = browser.ScreenshotRegion{X: req.Region.X, Y: req.Region.Y, Width: req.Region.Width, Height: req.Region.Height}
			}
			shot, err := s.manager.ScreenshotAnnotated(ctx, aopts)
			if err != nil {
				return toolError(err), nil
			}
			return map[string]any{
				"content": []toolContent{{Type: "image", Data: shot.Base64, MIMEType: shot.MIMEType}},
				"legend":  shot.Legend,
			}, nil
		}
		shot, err := s.manager.Screenshot(ctx)
		if err != nil {
			return toolError(err), nil
		}
		return map[string]any{
			"content": []toolContent{{Type: "image", Data: shot.Base64, MIMEType: shot.MIMEType}},
		}, nil
	case "brw_screenshot_element":
		var req struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		shot, err := s.manager.ScreenshotElement(ctx, req.Ref)
		if err != nil {
			return toolError(err), nil
		}
		return map[string]any{
			"content": []toolContent{{Type: "image", Data: shot.Base64, MIMEType: shot.MIMEType}},
		}, nil
	case "brw_wait_for":
		var req struct {
			Condition string `json:"condition"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.WaitFor(ctx, req.Condition, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_evaluate":
		var req struct {
			Expression string `json:"expression"`
			Offset     int    `json:"offset"`
			MaxBytes   int    `json:"max_bytes"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		value, err := s.manager.Evaluate(ctx, req.Expression)
		return evaluateResult(value, err, req.Offset, req.MaxBytes)
	case "brw_network_requests":
		var req struct {
			Filter  string `json:"filter"`
			Pattern string `json:"pattern"`
			Limit   int    `json:"limit"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		matcher, err := compileURLPattern(req.Pattern)
		if err != nil {
			return nil, invalid(err)
		}
		entries, err := s.manager.NetworkRequests(ctx, req.Filter)
		if err != nil {
			return toolError(err), nil
		}
		return toolJSON(filterNetworkRequests(entries, matcher, req.Limit), nil)
	case "brw_network_capture":
		var req struct {
			Filter  string `json:"filter"`
			Pattern string `json:"pattern"`
			Limit   int    `json:"limit"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		matcher, err := compileURLPattern(req.Pattern)
		if err != nil {
			return nil, invalid(err)
		}
		entries, err := s.manager.NetworkCapture(ctx, req.Filter)
		if err != nil {
			return toolError(err), nil
		}
		return toolJSON(filterCapturedRequests(entries, matcher, req.Limit), nil)
	case "brw_replay_request":
		var req struct {
			Method   string            `json:"method"`
			URL      string            `json:"url"`
			Headers  map[string]string `json:"headers"`
			Body     string            `json:"body"`
			Offset   int               `json:"offset"`
			MaxBytes int               `json:"max_bytes"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if err := s.checkNavPolicy(req.URL); err != nil {
			return toolError(err), nil
		}
		return toolJSON(s.manager.ReplayRequest(ctx, browser.ReplayRequestParams{
			Method:   req.Method,
			URL:      req.URL,
			Headers:  req.Headers,
			Body:     req.Body,
			Offset:   req.Offset,
			MaxBytes: req.MaxBytes,
		}))
	case "brw_plan":
		var req struct {
			Steps []browser.PlanStep `json:"steps"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		// The navigation guardrail must gate every path that can open a URL, not
		// just brw_open — otherwise a blocked/off-allowlist domain is reachable by
		// wrapping it in an "open" plan step. Check up front so a blocked
		// destination fails the whole plan before any step runs.
		for i := range req.Steps {
			st := &req.Steps[i]
			if strings.EqualFold(st.Action, "open") && st.URL != "" {
				normalizedURL, err := s.prepareNavigation(st.URL)
				if err != nil {
					return toolError(err), nil
				}
				st.URL = normalizedURL
			}
		}
		return toolJSON(s.manager.ExecutePlan(ctx, req.Steps))
	case "brw_batch":
		var req struct {
			Steps []browser.BatchStep `json:"steps"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		// Same guardrail as brw_plan: gate "open" steps so brw_batch cannot be
		// used to sidestep the navigation policy that brw_open enforces.
		for i := range req.Steps {
			st := &req.Steps[i]
			if strings.EqualFold(st.Action, "open") && st.URL != "" {
				normalizedURL, err := s.prepareNavigation(st.URL)
				if err != nil {
					return toolError(err), nil
				}
				st.URL = normalizedURL
			}
		}
		return toolJSON(s.manager.ExecuteBatch(ctx, req.Steps))
	case "brw_cancel":
		var req struct {
			Token string `json:"token"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Cancel(ctx, req.Token))
	case "brw_observe":
		return toolJSON(s.manager.Observe(ctx))
	case "brw_page_tools":
		return toolJSON(s.manager.Evaluate(ctx, snapshot.PageToolsScript))
	case "brw_call_page_tool":
		var req struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if strings.TrimSpace(req.Name) == "" {
			return toolError(errors.New("name is required; call brw_page_tools to list available page tools")), nil
		}
		return toolJSON(s.manager.Evaluate(ctx, snapshot.CallPageToolScript(req.Name, req.Arguments)))
	case "brw_group_tabs":
		var req struct {
			TabIDs  []string `json:"tab_ids"`
			Name    string   `json:"name"`
			Color   string   `json:"color"`
			GroupID string   `json:"group_id"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.GroupTabs(ctx, req.TabIDs, browser.TabGroupOptions{
			GroupID: req.GroupID,
			Name:    req.Name,
			Color:   req.Color,
		}))
	case "brw_ungroup_tabs":
		var req struct {
			TabIDs []string `json:"tab_ids"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.UngroupTabs(ctx, req.TabIDs))
	case "brw_assert_visible":
		var req struct {
			Ref       string `json:"ref"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.AssertVisible(ctx, req.Ref, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_assert_text":
		var req struct {
			Ref       string `json:"ref"`
			Text      string `json:"text"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.AssertText(ctx, req.Ref, req.Text, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_assert_value":
		var req struct {
			Ref       string `json:"ref"`
			Value     string `json:"value"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.AssertValue(ctx, req.Ref, req.Value, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_assert_hidden":
		var req struct {
			Ref       string `json:"ref"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.AssertHidden(ctx, req.Ref, time.Duration(req.TimeoutMS)*time.Millisecond))
	case "brw_commit":
		var req struct {
			Ref string `json:"ref"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolOK(s.manager.CommitField(ctx, req.Ref))
	case "brw_notify":
		var req struct {
			Kind    string `json:"kind"`
			Title   string `json:"title"`
			Message string `json:"message"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.Notify(ctx, browser.NotifyOptions{Kind: req.Kind, Title: req.Title, Message: req.Message}))
	case "brw_click_xy":
		var req struct {
			X float64 `json:"x"`
			Y float64 `json:"y"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.ClickXY(ctx, req.X, req.Y))
	case "brw_window_resize":
		var req browser.WindowResizeOptions
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		if err := browser.ValidateWindowResize(req); err != nil {
			return nil, invalid(err)
		}
		if _, err := browser.NormalizeWindowState(req.State); err != nil {
			return nil, invalid(err)
		}
		return toolJSON(s.manager.ResizeWindow(ctx, req))
	case "brw_window_bounds":
		return toolJSON(s.manager.WindowBounds(ctx))
	case "brw_console":
		var req consoleQuery
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		matcher, err := compileURLPattern(req.Pattern)
		if err != nil {
			return nil, invalid(err)
		}
		fresh, err := s.manager.ConsoleMessages(ctx)
		if err != nil {
			return toolError(err), nil
		}
		s.console.ingest(fresh)
		messages, matched, truncated := s.console.take(req, matcher)
		s.console.mu.Lock()
		retained := len(s.console.messages)
		s.console.mu.Unlock()
		return toolJSON(consoleResult{
			Messages:  messages,
			Returned:  len(messages),
			Matched:   matched,
			Retained:  retained,
			Truncated: truncated,
		}, nil)
	case "brw_downloads":
		return toolJSON(s.manager.Downloads(ctx))
	case "brw_trace":
		var req struct {
			Format        string `json:"format"`
			Guards        *bool  `json:"guards"`
			IncludeFailed *bool  `json:"include_failed"`
		}
		if err := unmarshalArgs(args, &req); err != nil {
			return nil, invalid(err)
		}
		trace := s.manager.GetTrace()
		switch strings.ToLower(strings.TrimSpace(req.Format)) {
		case "", "entries":
			return toolJSON(trace, nil)
		case "batch":
			return toolJSON(browser.TraceToBatch(trace, browser.ReplayOptions{
				Guards:        req.Guards,
				IncludeFailed: req.IncludeFailed,
			}), nil)
		default:
			return nil, invalid(fmt.Errorf("unknown format %q (valid: entries, batch)", req.Format))
		}
	case "brw_clear_trace":
		s.manager.ClearTrace()
		return toolOK(nil)
	default:
		return nil, &rpcError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", name)}
	}
}

func normalizeMCPSnapshotOptions(opts snapshot.SnapshotOptions) snapshot.SnapshotOptions {
	// Shared with the HTTP surface so both transports default identically.
	return snapshot.NormalizeOptions(opts)
}

func normalizeMCPFindOptions(opts snapshot.FindOptions) snapshot.FindOptions {
	if opts.Limit <= 0 {
		opts.Limit = defaultFindLimit
	}
	return opts
}

func parseMouseButtonArgs(args json.RawMessage) (browser.MouseButtonOptions, error) {
	var req struct {
		Ref    string   `json:"ref"`
		X      *float64 `json:"x"`
		Y      *float64 `json:"y"`
		Button string   `json:"button"`
	}
	if err := unmarshalArgs(args, &req); err != nil {
		return browser.MouseButtonOptions{}, err
	}
	return browser.MouseButtonOptions{
		MousePoint: browser.MousePoint{Ref: req.Ref, X: req.X, Y: req.Y},
		Button:     req.Button,
	}, nil
}

func unmarshalArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 || string(args) == "null" {
		args = []byte("{}")
	}
	return json.Unmarshal(args, dst)
}

// tabIDArg reconciles the historical `id` parameter of brw_focus_tab /
// brw_close_tab with the `tab_id` parameter every other page tool uses.
// Callers that pass {tab_id:"..."} (consistent with the rest of the surface)
// were previously silently ignored, leaving an empty id that the extension
// bridge coerced to tab 0. Prefer `tab_id`, fall back to `id` for backward
// compatibility.
// tabIDArg resolves the canonical tab id from the preferred tab_id field and its
// deprecated id alias (brw_focus_tab / brw_close_tab). Precedence is
// intentional graceful promotion: a non-empty tab_id always wins, and id is used
// only as a fallback. If a caller supplies both with different values, tab_id is
// used and id is silently ignored — documented in the tool schemas where id is
// labelled "Deprecated alias for tab_id".
func tabIDArg(tabID, id string) string {
	if strings.TrimSpace(tabID) != "" {
		return tabID
	}
	return id
}

func contextIDArg(contextID, legacyBrowserContextID string) string {
	if strings.TrimSpace(contextID) != "" {
		return contextID
	}
	return legacyBrowserContextID
}

// defaultEvaluateMaxBytes bounds the serialized brw_evaluate result returned to
// the client. Historically an oversized result came back EMPTY (the payload was
// silently dropped past ~11KB by a downstream size limit); we now truncate with
// an explicit marker so the caller always gets the leading bytes plus the total
// length, and can page through the rest with offset/max_bytes.
const defaultEvaluateMaxBytes = 64 * 1024

// evaluateResult serializes a brw_evaluate value and applies offset/max_bytes
// windowing. An oversized window is truncated with an explicit suffix marker
// (never returned empty). offset/max_bytes are clamped to sane ranges; passing
// neither yields the leading defaultEvaluateMaxBytes of the result.
func evaluateResult(value any, err error, offset, maxBytes int) (any, *rpcError) {
	if err != nil {
		return toolError(err), nil
	}
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return toolError(marshalErr), nil
	}

	total := len(data)
	if offset < 0 {
		offset = 0
	}
	if maxBytes <= 0 {
		maxBytes = defaultEvaluateMaxBytes
	}

	// offset past the end yields an explicit empty window, not a confusing nil.
	if offset >= total {
		text := fmt.Sprintf("…[truncated: offset %d is at or beyond end; returned 0 of %d bytes]", offset, total)
		return map[string]any{"content": []toolContent{{Type: "text", Text: text}}}, nil
	}

	// Clamp the window WITHOUT overflowing offset+maxBytes: max_bytes is
	// caller-controlled and could be near math.MaxInt, which would wrap
	// negative and panic data[offset:end]. offset < total is guaranteed above,
	// so total-offset is a safe positive bound.
	end := total
	if maxBytes < total-offset {
		end = offset + maxBytes
	}
	window := string(data[offset:end])
	truncated := offset > 0 || end < total

	if !truncated {
		// Small (or fully-covered) result: behave exactly like toolJSON so
		// structured clients still get structuredContent for object payloads.
		result := map[string]any{
			"content": []toolContent{{Type: "text", Text: window}},
		}
		if isJSONObject(data) {
			result["structuredContent"] = value
		}
		return result, nil
	}

	marker := fmt.Sprintf("\n…[truncated: returned %d of %d bytes (offset %d); request more with offset=%d, max_bytes=N]",
		end-offset, total, offset, end)
	return map[string]any{
		"content": []toolContent{{Type: "text", Text: window + marker}},
	}, nil
}

func toolJSON[T any](value T, err error) (any, *rpcError) {
	if err != nil {
		return toolError(err), nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return toolError(err), nil
	}
	result := map[string]any{
		"content": []toolContent{{Type: "text", Text: string(data)}},
	}
	// Per MCP, structuredContent MUST be a JSON object. Tools whose payload can
	// be an array or scalar — notably brw_evaluate returning a string/number,
	// or list tools returning a top-level array — would otherwise emit a
	// non-object structuredContent that strict clients reject
	// with an "expected record" schema error, forcing wasteful retries. Only
	// attach structuredContent when the payload actually serializes to an object;
	// the text content always carries the full result regardless.
	if isJSONObject(data) {
		result["structuredContent"] = value
	}
	return result, nil
}

// isJSONObject reports whether data is a JSON object (ignoring leading whitespace).
func isJSONObject(data []byte) bool {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

func toolOK(err error) (any, *rpcError) {
	if err != nil {
		return toolError(err), nil
	}
	return map[string]any{"content": []toolContent{{Type: "text", Text: `{"ok":true}`}}}, nil
}

func toolError(err error) any {
	out := map[string]any{
		"isError": true,
		"content": []toolContent{{Type: "text", Text: err.Error()}},
	}
	// Attach a machine-readable code for the transport-level failure classes that
	// used to cascade into "Multiple consecutive errors detected" connector wedges
	// (issue #11 P1-5). A wedged/blocked tab now time-boxes to a deadline error;
	// surfacing code:"timeout" with retryable:true lets the connector back off and
	// retry the ONE call instead of poisoning the session. Genuine tool errors
	// (bad ref, navpolicy denial) carry no code and are left as plain text.
	if code := classifyToolError(err); code != "" {
		out["structuredContent"] = map[string]any{
			"error":     code,
			"message":   err.Error(),
			"retryable": toolErrorRetryable(code),
		}
	}
	return out
}

// classifyToolError maps a transport/timeout failure to a stable code, or returns
// "" for an ordinary tool error that callers should treat as a hard failure.
func classifyToolError(err error) string {
	class := usagelog.ClassifyError(err)
	if class == "tool" {
		return ""
	}
	return class
}

// toolErrorRetryable reports whether a classified error is worth re-issuing. A
// caller-cancelled call is not (the caller went away); transient transport/load
// failures are.
func toolErrorRetryable(code string) bool {
	return usagelog.Retryable(code)
}

func invalid(err error) *rpcError {
	return &rpcError{Code: -32602, Message: err.Error()}
}

func canonicalToolName(name string) string {
	if strings.HasPrefix(name, "browser_") {
		return "brw_" + strings.TrimPrefix(name, "browser_")
	}
	return name
}

func tools() []map[string]any {
	return []map[string]any{
		tool("brw_open", "Open a URL in a visible Chrome/Chromium tab and exclusively lease it to this session. With no group/group_id the tab lands in this session's per-agent tab group automatically; pass group only for a deliberately different run-scoped group. Close every tab you opened before finishing unless handing it to the human; never close pre-existing tabs. On the extension bridge tabs open in the BACKGROUND, so brw never stomps the human's current tab. To use an existing tab, pass the tab_id of one brw_list_tabs marks available — never one marked leased.", object(map[string]any{
			"url":         stringSchema("URL to open. Scheme defaults to https."),
			"group":       stringSchema("Optional Chrome tab group title overriding the automatic per-agent group. Keep it short, run-scoped, and free of secrets; when set without group_id, the extension reuses an existing same-title group or creates one."),
			"group_id":    stringSchema("Optional existing Chrome tab group id from brw_list_tabs or brw_list_tab_groups. When set, the new tab is added to that group."),
			"group_color": stringSchema("Optional group color: grey, blue, red, yellow, green, pink, purple, cyan, orange."),
		}, []string{"url"})),
		tool("brw_open_incognito", "Open a URL in a brand-new INCOGNITO browser context: a fully isolated session with its own cookies, storage, and cache that shares nothing with the normal profile or other contexts (the CDP equivalent of an incognito window). Returns the new tab including its context_id. WHEN DONE, call brw_close_context with that context_id to dispose the whole context (closes every tab in it and discards its data). DIRECT-CDP TRANSPORT ONLY: on the extension-bridge transport (driving the user's existing signed-in Chrome) this returns an error — use a dedicated direct-CDP profile for incognito. Ideal for clean-room / logged-out internal testing.", object(map[string]any{
			"url": stringSchema("URL to open in the new incognito context. Scheme defaults to https."),
		}, []string{"url"})),
		tool("brw_close_context", "Dispose an incognito browser context created by brw_open_incognito: closes every tab inside it and discards its isolated cookies/storage. Pass the context_id returned by brw_open_incognito. Direct-CDP transport only.", object(map[string]any{
			"context_id":         stringSchema("The context_id returned by brw_open_incognito."),
			"browser_context_id": stringSchema("Deprecated alias for context_id."),
		}, []string{"context_id"})),
		tool("brw_identity", "Report which browser profile THIS brw namespace drives, so you can pick the right one instead of guessing from the namespace label. brw exposes one namespace per browser profile (brw, brw_chromium, brw_chromium_work, …) and that set grows — enumerate them, then call brw_identity on each to map a namespace to a concrete browser+profile. Returns {identity:{workspace, profile, user_data_dir, profile_directory, mode}, version, connected}. Needs no tab and no connected bridge — safe to call first, even when the browser has no windows open. When the user's request implies a specific browser (their work Chrome, their personal Chromium, …), use this to confirm the match before you open or touch a tab.", object(nil, nil)),
		tool("brw_list_tabs", "List controllable Chrome/Chromium browser targets, including owner-redacted lease metadata. lease.status is mine for this session's tabs, leased for a tab under another session's control, or available for an unclaimed tab. Never operate on leased tabs; call brw_open for a fresh tab instead. lease.group_drift on your own tab means a human moved it out of your per-agent tab group (ownership unchanged; regroup with brw_group_tabs + expected_group_id only if tidiness matters). discarded/frozen flag tabs whose renderer Chrome has reclaimed or paused; brw auto-revives them before driving. Popup windows and Chrome tab-group metadata are included when the extension bridge reports them.", object(nil, nil)),
		tool("brw_list_tab_groups", "List visible Chrome tab groups with ids, titles, colors, collapsed state, window ids, and member tab ids. Extension-bridge transport only; direct CDP cannot inspect Chrome tab groups.", object(nil, nil)),
		tool("brw_focus_tab", "Claim and focus an available or already-mine Chrome/Chromium target, then make it this session's default for following reads/actions. A target marked leased by brw_list_tabs belongs to another session and is rejected with non-retryable tab_contended; open a new tab instead.", object(map[string]any{
			"tab_id": stringSchema("Target id from brw_list_tabs (preferred, consistent with other tools)."),
			"id":     stringSchema("Deprecated alias for tab_id."),
		}, nil)),
		tool("brw_close_tab", "Close a controllable Chrome/Chromium target. Use it to clean up tabs opened by this run; never close a tab that existed before the run unless the user explicitly asked.", object(map[string]any{
			"tab_id": stringSchema("Target id from brw_list_tabs (preferred, consistent with other tools)."),
			"id":     stringSchema("Deprecated alias for tab_id."),
		}, nil)),
		tool("brw_emulate_device", "Apply Chrome DevTools device emulation to a tab for responsive/mobile testing. NOT OS window resizing: media queries, viewport meta, DPR, touch, and optional UA/platform overrides behave like DevTools mobile mode. Width/height are CSS viewport pixels. Pass clear:true to reset. Reload after applying if the app picks mobile/desktop only at initial load.", object(map[string]any{
			"device":              stringEnumSchema("Device preset: iphone_se (default when omitted), iphone_12, iphone_13, iphone_14, iphone_14_pro_max, pixel_5, pixel_7, galaxy_s20, ipad_mini, ipad; responsive/custom with width+height; or clear/reset/off/none/desktop to reset. Only these presets exist — for any other device pass responsive/custom with explicit width+height rather than guessing a model name.", "iphone_se", "iphone_12", "iphone_13", "iphone_14", "iphone_14_pro_max", "pixel_5", "pixel_7", "galaxy_s20", "ipad_mini", "ipad", "responsive", "custom", "desktop", "clear", "reset", "off", "none"),
			"width":               integerSchema("Custom CSS viewport width in pixels. Overrides preset width when supplied."),
			"height":              integerSchema("Custom CSS viewport height in pixels. Overrides preset height when supplied."),
			"device_scale_factor": map[string]any{"type": "number", "description": "Device pixel ratio / DPR. Defaults to the preset DPR or 2 for custom mobile emulation."},
			"mobile":              boolSchema("Enable Chrome mobile emulation semantics: viewport meta, overlay scrollbars, text autosizing, and related behavior. Defaults true for presets/custom."),
			"touch":               boolSchema("Enable touch emulation and mouse-to-touch forwarding. Defaults true when mobile is true."),
			"user_agent":          stringSchema("Optional user-agent override. Presets supply a mobile UA; custom mobile uses a generic Android Chrome UA unless overridden."),
			"platform":            stringSchema("Optional navigator.platform override. Presets supply iPhone/iPad/Linux armv8l values."),
			"max_touch_points":    integerSchema("Maximum emulated touch points. Defaults to 5 when touch is enabled."),
			"orientation":         stringEnumSchema("portrait or landscape. When set, preset dimensions are swapped as needed.", "portrait", "landscape"),
			"clear":               boolSchema("Reset DevTools device metrics/touch emulation for this tab and restore original user agent/platform if brw captured them before applying emulation."),
			"tab_id":              stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_read", "Return semantic page content: main text, headings, links, forms, tables, and metadata. Prose is bounded by default and paged via next_offset — a long article is several cheap reads, not one huge one. Narrow with include to skip what you do not need (include:[\"headings\",\"links\"] is a cheap page map).", object(map[string]any{
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
			"include": map[string]any{
				"type":        "array",
				"items":       stringEnumSchema("Section name.", "main", "headings", "links", "forms", "tables", "metadata"),
				"description": "Sections to return. Omit for all of them.",
			},
			"section":      stringSchema("Return only this heading's span, ending at the next heading of the same or higher level. Matches a heading name case-insensitively, exact match preferred over substring. The cheap pattern for a long document: read include:[\"headings\"] for the outline, then fetch the one section you need instead of paging the whole page. An unmatched name is an error listing the available sections."),
			"max_chars":    integerSchema("Cap on returned prose characters. Defaults to 20000; -1 returns the whole document. When it truncates, main_truncated is set and next_offset gives the offset for the following page. Applied within section when one is given."),
			"offset":       integerSchema("Character offset into the prose, for paging with next_offset."),
			"max_links":    integerSchema("Cap on returned links. Defaults to 300; -1 for no cap."),
			"max_headings": integerSchema("Cap on returned headings. Defaults to 100; -1 for no cap."),
		}, nil)),
		tool("brw_read_data", "Extract embedded structured page data (Next.js __NEXT_DATA__, JSON-LD, microdata, Open Graph) as a compact normalized object without DOM rendering.", object(map[string]any{
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_snapshot", "Return interactive controls with stable refs. Defaults to a bounded visible/actionable viewport frontier; use mode:\"all\" for full-page debugging (returns every matching element including offscreen/hidden controls — useful for comprehensive page analysis), and add include_hidden:true only when hidden inputs are needed. Metadata includes total_candidates for the full count before filtering.", object(map[string]any{
			"tab_id":               stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
			"mode":                 stringEnumSchema("frontier (default, scored visible/actionable controls) or all (full matching list, including offscreen/currently invisible matching controls) or form_lens (form fields with validation state only).", "frontier", "all", "form_lens"),
			"query":                stringSchema("Case-insensitive substring match across ref, role, name, tag, type, href, and value."),
			"text":                 stringSchema("Alias for query-style text filtering."),
			"role":                 stringSchema("ARIA/semantic role to include, for example button or textbox."),
			"limit":                integerSchema("Maximum number of elements to return. Defaults to 40 in frontier mode."),
			"viewport_only":        boolSchema("Only return elements intersecting the viewport. Forced true in default frontier mode."),
			"include_hidden":       boolSchema("Include input[type=hidden] fields as role hidden for explicit debugging. Defaults false."),
			"include_ax":           boolSchema("Include full accessibility-tree enrichment. Expensive; defaults false."),
			"include_frames":       boolSchema("Surface CROSS-ORIGIN iframes (embedded editors/widgets the normal walk cannot reach) as elements with source:[\"frame\"] and a cx/cy click point — click them with brw_click_xy at cx/cy. Same-origin iframes are always read regardless. Inner controls, when reachable, also appear as f<i>:e<j>. Off by default."),
			"text_content":         boolSchema("Also match against full visible text content (innerText), surfacing prose-bearing elements like headings, paragraphs, and list items — not just interactive-element metadata. Opt-in; defaults false."),
			"visual_islands":       boolSchema("Detect semantically-opaque visual content (canvas/svg/video/large image/custom widget) and emit each as an element with source:[\"visual\"], visual_type, and visual_hint. Off by default."),
			"visual_islands_limit": integerSchema("Cap on detected visual islands before merging into the element list. Defaults to 10."),
			"since":                integerSchema("Pass a prior snapshot's metadata.version for a DELTA: 'elements' carries ONLY added+changed elements, metadata.delta=true, and a 'delta' object lists {added, removed, changed} refs. Any mismatch (version, options, navigation) returns a full snapshot. Omit for a full snapshot."),
			"format":               stringEnumSchema("Output shape: json (default, structured object) or compact (one terse text line per element: ref role \"name\" + key state). compact uses markedly fewer tokens — prefer it for small models. Presentation only; element selection and deltas are unchanged.", "json", "compact"),
		}, nil)),
		tool("brw_find", "Find matching semantic element refs without dumping the full page.", object(map[string]any{
			"tab_id":         stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
			"query":          stringSchema("Case-insensitive substring match across ref, role, name, tag, type, href, and value. Set text_content:true to also match visible prose text."),
			"text":           stringSchema("Alias for query-style text filtering."),
			"role":           stringSchema("ARIA/semantic role to include, for example button or textbox."),
			"limit":          integerSchema("Maximum number of elements to return."),
			"viewport_only":  boolSchema("Only return elements intersecting the viewport."),
			"include_hidden": boolSchema("Include input[type=hidden] fields as role hidden for explicit debugging. Defaults false."),
			"text_content":   boolSchema("Also match against full visible text content (innerText), surfacing prose-bearing elements like headings, paragraphs, and list items — not just interactive-element metadata. Opt-in; defaults false."),
		}, nil)),
		tool("brw_click", "Click a semantic element ref (or x,y coordinates) from brw_snapshot. Defaults to a left single-click; set button to right (opens context menus) or middle, and click_count to 2 (double-click) or 3 (triple-click selects a line). When the click opens a new tab, the response includes new_tab_id with the freshly opened tab's id.", object(map[string]any{
			"ref":         stringSchema("Element ref, for example e18. Provide ref or x,y."),
			"x":           map[string]any{"type": "number", "description": "X coordinate in viewport pixels. Use with y instead of ref for canvas/coordinate clicks."},
			"y":           map[string]any{"type": "number", "description": "Y coordinate in viewport pixels. Use with x instead of ref for canvas/coordinate clicks."},
			"button":      stringEnumSchema("Mouse button: left (default), right, or middle.", "left", "right", "middle"),
			"click_count": integerSchema("Click count: 1 (default), 2 for double-click, 3 to triple-click (select a line)."),
			"snapshot":    boolSchema("Include a full page snapshot in the response. Use this to avoid a separate brw_snapshot call after the action — the response gains a 'snapshot' field with the same structure as brw_snapshot output."),
			"tab_id":      stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_drag", "Press at a source (ref or x,y), move to a target (ref or x,y) over several steps, then release. Use for sliders/range inputs, drag-and-drop reorder, and canvas/map panning.", object(map[string]any{
			"from":   mousePointSchema("Drag source. Provide either ref or x and y."),
			"to":     mousePointSchema("Drag target. Provide either ref or x and y."),
			"steps":  integerSchema("Number of intermediate mouse-move steps between source and target. Defaults to 12."),
			"button": stringEnumSchema("Mouse button held during the drag: left (default), right, or middle.", "left", "right", "middle"),
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"from", "to"})),
		tool("brw_mouse_down", "Press and hold a mouse button at a ref or x,y without releasing (the press half of a press-and-hold). Pair with brw_mouse_up.", object(map[string]any{
			"ref":    stringSchema("Element ref to press at. Provide ref or x,y."),
			"x":      map[string]any{"type": "number", "description": "X coordinate in viewport pixels."},
			"y":      map[string]any{"type": "number", "description": "Y coordinate in viewport pixels."},
			"button": stringEnumSchema("Mouse button: left (default), right, or middle.", "left", "right", "middle"),
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_mouse_up", "Release a held mouse button at a ref or x,y (the release half of a press-and-hold). Pair with brw_mouse_down.", object(map[string]any{
			"ref":    stringSchema("Element ref to release at. Provide ref or x,y."),
			"x":      map[string]any{"type": "number", "description": "X coordinate in viewport pixels."},
			"y":      map[string]any{"type": "number", "description": "Y coordinate in viewport pixels."},
			"button": stringEnumSchema("Mouse button: left (default), right, or middle.", "left", "right", "middle"),
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_click_text", "Click the best visible actionable element whose accessible name or visible text matches text. Useful for controls like \"Check out\" when refs are stale or custom components hide internals. Below-fold matches are scrolled into view before clicking by default. When the click opens a new tab, the response includes new_tab_id.", object(map[string]any{
			"text":        stringSchema("Visible text or accessible name to click."),
			"role":        stringSchema("Optional role filter, for example button, link, option, or menuitem."),
			"exact":       boolSchema("Require an exact normalized text/name match instead of allowing substring matches."),
			"auto_scroll": boolSchema("Scroll a below-fold match into view before clicking (default true). Set false to click only elements already in the viewport."),
			"snapshot":    boolSchema("Include a full page snapshot in the response."),
			"tab_id":      stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"text"})),
		tool("brw_navigate", "Navigate the active tab's session history: back, forward, or reload. Uses the page navigation history (no URL needed); returns a post-navigation observation.", object(map[string]any{
			"direction": stringEnumSchema("back (previous history entry), forward (next history entry), or reload (re-fetch the current document).", "back", "forward", "reload"),
			"snapshot":  boolSchema("Include a full page snapshot in the response."),
			"tab_id":    stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"direction"})),
		tool("brw_navigate_to", "Navigate brw's current working tab to a URL, wait for the page to load, and return a post-navigation observation. Unlike brw_open, this reuses the working tab instead of creating another. In the default isolation mode brw operates in its OWN tab(s): if it has not opened one yet, this opens a fresh tab rather than navigating whatever tab you are on. To navigate one of YOUR existing tabs, pass its tab_id (from brw_list_tabs).", object(map[string]any{
			"url":      stringSchema("URL to navigate to. Scheme defaults to https."),
			"snapshot": boolSchema("Include a full page snapshot in the response."),
			"tab_id":   stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"url"})),
		tool("brw_hover", "Hover over a semantic element ref to trigger mouseenter/mouseover/pointermove events.", object(map[string]any{
			"ref":      stringSchema("Element ref, for example e18."),
			"snapshot": boolSchema("Include a full page snapshot in the response."),
			"tab_id":   stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref"})),
		tool("brw_evaluate", "Run arbitrary JavaScript in the page context and return the JSON-serializable result. Supports async expressions. Large results are TRUNCATED with an explicit '…[truncated: returned N of M bytes]' marker (never silently empty); use offset/max_bytes to page through them. Note: fetch() runs under the current page's Content-Security-Policy, so cross-origin calls must be made from a tab whose origin permits them (otherwise they fail with a CSP/'Failed to fetch' error).", object(map[string]any{
			"expression": stringSchema("JavaScript expression to evaluate. May use await for async operations."),
			"offset":     map[string]any{"type": "integer", "description": "Byte offset into the serialized result to start returning from. Defaults to 0. Use with the marker on a truncated response to page forward."},
			"max_bytes":  map[string]any{"type": "integer", "description": "Maximum bytes of the serialized result to return in this call. Defaults to 65536. The response is truncated (with a marker) rather than dropped when the result is larger."},
			"tab_id":     stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"expression"})),
		tool("brw_network_requests", "Return network resource requests captured by the Performance API (performance.getEntriesByType). A busy page issues hundreds — narrow with pattern or filter before reading.", object(map[string]any{
			"filter":  stringSchema("Case-insensitive substring to filter request URLs."),
			"pattern": stringSchema("Regular expression the request URL must match. Applied after filter."),
			"limit":   integerSchema("Maximum requests to return, taken from the most recent. Defaults to 100; -1 for no cap."),
			"tab_id":  stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_network_capture", "Install an in-page interceptor wrapping fetch and XMLHttpRequest, then drain and return captured requests (method, url, request headers/body, status, ok, response snippet, started_at, duration_ms). Call once to start capturing, then again after triggering page activity to read what was recorded. Bodies and snippets are truncated.", object(map[string]any{
			"filter":  stringSchema("Case-insensitive substring to filter captured request URLs."),
			"pattern": stringSchema("Regular expression the request URL must match. Applied after filter."),
			"limit":   integerSchema("Maximum requests to return, taken from the most recent. Defaults to 100; -1 for no cap."),
			"tab_id":  stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_replay_request", "Re-execute a request in-page via fetch(url, {method, headers, body}) and return a byte-windowed response body plus body_total_bytes/body_truncated/next_offset. Use offset/max_bytes to page bodies larger than the 64 KiB window. SAFETY: a MUTATING replay (POST/PUT/PATCH/DELETE) whose URL looks like checkout, payment, purchase, or order placement is BLOCKED; idempotent GET/HEAD is always allowed.", object(map[string]any{
			"method":    stringSchema("HTTP method, for example GET or POST. Defaults to GET."),
			"url":       stringSchema("Request URL. May be relative to the current page."),
			"headers":   map[string]any{"type": "object", "description": "Optional request headers as a string-to-string map.", "additionalProperties": stringSchema("Header value.")},
			"body":      stringSchema("Optional request body. Ignored for GET/HEAD."),
			"offset":    integerSchema("UTF-8 byte offset into the response body. Defaults to 0; use next_offset from a truncated result for the following window."),
			"max_bytes": integerSchema("Maximum response-body bytes to return. Defaults to 65536 and is capped at 1048576."),
			"tab_id":    stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"url"})),
		tool("brw_type", "Type text into a semantic element ref.", object(map[string]any{
			"ref":      stringSchema("Element ref, for example e17."),
			"text":     stringSchema("Text to insert."),
			"snapshot": boolSchema("Include a full page snapshot in the response."),
			"tab_id":   stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref", "text"})),
		tool("brw_fill", "Replace or append text in a semantic text field by ref or query and return a post-action observation. Also sets a native range slider (<input type=range>), number, or date input to an exact value in ONE call (prefer this over repeated brw_press arrow keys for sliders). If the ref exists but is not a text input, the error suggests using brw_type instead.", object(map[string]any{
			"ref":      stringSchema("Element ref, for example e17. Optional when query is supplied."),
			"query":    stringSchema("Find a fillable target by semantic name when ref is not supplied."),
			"role":     stringSchema("Optional role filter when using query, normally textbox or searchbox."),
			"text":     stringSchema("Text to put in the field. Prefer this over value."),
			"value":    stringSchema("Playwright-style alias for text. Accepted so {value:\"…\"} fills the field instead of silently clearing it."),
			"replace":  boolSchema("Replace existing field content instead of appending. Defaults to true."),
			"snapshot": boolSchema("Include a full page snapshot in the response."),
			"tab_id":   stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_upload_file", "Set a file on a semantic file input by ref or query and return a post-action observation. Provide the file from EXACTLY ONE source: path/paths (already on the browser host), bytes_base64 (inline contents), or url (the daemon fetches it). Temp files are cleaned up automatically after a grace period.", object(map[string]any{
			"ref":          stringSchema("Element ref for input[type=file]. Optional when query is supplied."),
			"query":        stringSchema("Find a file input by semantic name when ref is not supplied. Defaults to file."),
			"role":         stringSchema("Optional role filter when using query."),
			"path":         stringSchema("Single local file path on the browser host."),
			"paths":        map[string]any{"type": "array", "items": stringSchema("Local file path on the browser host."), "description": "One or more local file paths on the browser host."},
			"bytes_base64": stringSchema("Inline file contents as base64. The daemon writes them to a temp file; use filename to control the name the page sees."),
			"url":          stringSchema("http(s) URL the daemon fetches before uploading."),
			"filename":     stringSchema("Name the page sees for a bytes_base64/url upload. Defaults to the url basename."),
			"click_ref":    stringSchema("Ref of the button that reveals the file input, when no static input exists. brw intercepts the native dialog so it never opens; works across iframes. Provide click_ref OR click_text, not both."),
			"click_text":   stringSchema("Like click_ref, but identifies the trigger button by its visible/accessible text. brw intercepts the native dialog so it never opens; works across iframes."),
			"tab_id":       stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_select", "Set a native select or custom listbox/combobox value by semantic element ref. Value may be the option value/data-value or visible option label.", object(map[string]any{
			"ref":      stringSchema("Element ref for a select, combobox, or listbox trigger."),
			"value":    stringSchema("Option value, data-value, or visible option label to select."),
			"snapshot": boolSchema("Include a full page snapshot in the response."),
			"tab_id":   stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref", "value"})),
		tool("brw_press", "Press a keyboard key in the active tab.", object(map[string]any{
			"key":      stringSchema("Key name or chord, for example Enter, Tab, Escape, ArrowDown, Meta+Enter."),
			"repeat":   integerSchema("Press the key this many times (1-100) in one call, instead of one call per press. Only the final observation is returned."),
			"snapshot": boolSchema("Include a full page snapshot in the response."),
			"tab_id":   stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"key"})),
		tool("brw_scroll", "Scroll the active page or scroll container in a direction.", object(map[string]any{
			"direction": stringEnumSchema("up, down, left, or right.", "up", "down", "left", "right"),
			"repeat":    integerSchema("Scroll this many times (1-100) in one call, instead of one call per scroll. Only the final observation is returned."),
			"snapshot":  boolSchema("Include a full page snapshot in the response."),
			"tab_id":    stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"direction"})),
		tool("brw_screenshot", "Visual fallback — you almost never need this. brw is semantic-first: brw_snapshot/brw_find expose every control with a ref, brw_read returns page prose/result/status/badge text, and EVERY action (click/type/fill/select/press/drag) returns a post-action observation that confirms its effect (changed elements, new values, navigation). To VERIFY an outcome (a cart badge, a result message, a swapped item, an editor's text), read that observation or call brw_read — do NOT screenshot to check. Reserve brw_screenshot for opaque visual content with no DOM text (canvas, maps, charts, image-only widgets). Set annotate:true for a Set-of-Marks capture: in-viewport elements get labelled boxes carrying the SAME refs brw_snapshot returns, plus a legend mapping each ref to its box, role, and name — so you can read a label off the image and act on it with brw_click. Pass ref OR region for a tight annotated crop (far fewer vision tokens on a dense page); both imply annotate. The overlay never mutates the page.", object(map[string]any{
			"tab_id":   stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
			"annotate": boolSchema("Draw Set-of-Marks ref labels over frontier elements and return a ref->box legend. Defaults false (plain screenshot)."),
			"ref":      stringSchema("Crop to this element's box (smaller image, fewer vision tokens). Implies annotate."),
			"region": map[string]any{
				"type":        "object",
				"description": "Viewport-space clip rectangle in CSS pixels. Implies annotate.",
				"properties": map[string]any{
					"x":      map[string]any{"type": "number", "description": "Left edge in viewport pixels."},
					"y":      map[string]any{"type": "number", "description": "Top edge in viewport pixels."},
					"width":  map[string]any{"type": "number", "description": "Clip width in pixels."},
					"height": map[string]any{"type": "number", "description": "Clip height in pixels."},
				},
			},
		}, nil)),
		tool("brw_screenshot_element", "Capture a PNG screenshot of a semantic element ref for visual fallback/debugging.", object(map[string]any{
			"ref":    stringSchema("Element ref from brw_snapshot."),
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref"})),
		tool("brw_wait_for", "Wait for page readiness, URL/title/text substring, or ref availability.", object(map[string]any{
			"condition":  stringSchema("Condition to wait for: ready or page_ready (document interactive/complete), load (alias of ready), committed (interactive/complete AND a real navigated URL, not about:blank), text:<substring>, not_text:<substring>, url:<substring>, not_url:<substring>, title:<substring>, not_title:<substring>, ref:<brw-ref>, not_ref:<brw-ref>, or a plain text substring of body innerText."),
			"timeout_ms": map[string]any{"type": "integer", "description": "Timeout in milliseconds. Defaults to the daemon timeout (typically 20s)."},
			"tab_id":     stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"condition"})),
		tool("brw_plan", "Execute a sequence of browser operations in one round-trip. Steps run sequentially and stop on first failure. Steps that produce data carry it under result; snapshot steps also populate snapshot. Prefer brw_batch, which returns one observation instead of per-step payloads.", object(map[string]any{
			"steps": map[string]any{
				"type":        "array",
				"description": "Ordered list of steps to execute.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":      stringEnumSchema("One of: click, type, fill, select, press, scroll, hover, wait, snapshot, read, open, focus_tab.", "click", "type", "fill", "select", "press", "scroll", "hover", "wait", "snapshot", "read", "open", "focus_tab"),
						"ref":         stringSchema("Element ref for click, type, fill, select, hover."),
						"text":        stringSchema("Text for type and fill actions."),
						"value":       stringSchema("Option value for select. For fill, also accepted as a Playwright-style alias for text."),
						"direction":   stringEnumSchema("Scroll direction: up, down, left, right.", "up", "down", "left", "right"),
						"condition":   stringSchema("Wait condition (load, text:..., ref:..., url:..., etc)."),
						"timeout_ms":  map[string]any{"type": "integer", "description": "Timeout for wait action in milliseconds."},
						"url":         stringSchema("URL for open action."),
						"id":          stringSchema("Tab id for focus_tab action."),
						"key":         stringSchema("Key name for press action (Enter, Tab, Escape, etc)."),
						"expect_ref":  stringSchema("Validate this ref exists before running the action (fail-fast)."),
						"expect_role": stringSchema("Validate the expect_ref element has this role."),
					},
					"required": []string{"action"},
				},
			},
		}, []string{"steps"})),
		tool("brw_batch", "PREFERRED for multi-step flows: chain click, type, fill, select, press, scroll, hover, wait, open, focus_tab, and inline assertions (assert_visible, assert_text, assert_value, assert_hidden) in ONE round-trip, returning a single observation at the end. Use this instead of individual brw_click/brw_type/brw_fill calls whenever you need 2+ actions. Steps run sequentially; interleave assertions to fail fast.", object(map[string]any{
			"steps": map[string]any{
				"type":        "array",
				"description": "Ordered list of actions and assertions to execute.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action":     stringEnumSchema("One of: click, click_text, type, fill, select, press, scroll, hover, wait, open, navigate_to, focus_tab, assert_visible, assert_text, assert_value, assert_hidden. open spawns a new tab; navigate_to drives the batch's existing working tab.", "click", "click_text", "type", "fill", "select", "press", "scroll", "hover", "wait", "open", "navigate_to", "focus_tab", "assert_visible", "assert_text", "assert_value", "assert_hidden"),
						"ref":        stringSchema("Element ref for click, type, fill, select, hover, and assert_* actions."),
						"text":       stringSchema("Text for type and fill actions, or expected text for assert_text."),
						"value":      stringSchema("Option value for select / assert_value. For fill, also accepted as a Playwright-style alias for text."),
						"direction":  stringEnumSchema("Scroll direction: up, down, left, right.", "up", "down", "left", "right"),
						"condition":  stringSchema("Wait condition (load, text:..., ref:..., url:..., etc)."),
						"timeout_ms": map[string]any{"type": "integer", "description": "Timeout for wait/assert actions in milliseconds."},
						"url":        stringSchema("URL for open action."),
						"id":         stringSchema("Tab id for focus_tab action."),
						"key":        stringSchema("Key name for press action (Enter, Tab, Escape, etc)."),
					},
					"required": []string{"action"},
				},
			},
		}, []string{"steps"})),
		tool("brw_cancel", "Cooperatively stop in-flight long-running operations (brw_plan, brw_batch, and their waits) for an operation token. Omit token (or pass \"*\") to stop everything; pass tab_id to stop work targeting that tab. The cancelled operation returns a normal result reporting steps_completed and cancelled=true rather than erroring. Returns how many operations were signalled.", object(map[string]any{
			"token":  stringSchema("Operation token to cancel. Omit or use \"*\" to cancel all in-flight operations."),
			"tab_id": stringSchema("Optional tab id. When set (and no explicit token), cancels operations targeting that tab."),
		}, nil)),
		tool("brw_observe", "Lightweight change detector: returns version, URL, title, focused ref, and frontier element changes since last observe. Use this INSTEAD of brw_snapshot to check whether a page action had an effect — it's faster and returns fewer tokens. Call brw_snapshot only when you need fresh refs to act on.", object(map[string]any{
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_page_tools", "List WebMCP tools the current page exposes via navigator.modelContext (W3C Web Machine Context). When a site cooperates, calling its declared tools is far more reliable and token-efficient than driving the DOM — prefer them when present. Returns {supported, tools:[{name, description, inputSchema}]}; supported:false means the page exposes none (or brw's WebMCP runtime is not enabled with --enable-webmcp).", object(map[string]any{
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_call_page_tool", "Invoke a WebMCP page tool by name with arguments matching its inputSchema (discover them via brw_page_tools). Returns {ok, result} on success or {ok:false, error}. Use this instead of clicking through the UI when the page declares a tool for the task.", object(map[string]any{
			"name":      stringSchema("The page tool name from brw_page_tools."),
			"arguments": map[string]any{"type": "object", "description": "Arguments object passed to the tool, matching its inputSchema.", "additionalProperties": true},
			"tab_id":    stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"name"})),
		tool("brw_group_tabs", "Group tabs into a named Chrome tab group, or move them into an existing group_id.", object(map[string]any{
			"tab_ids":  map[string]any{"type": "array", "items": stringSchema("Tab id."), "description": "Tab IDs to group."},
			"name":     stringSchema("Group name shown in Chrome tab strip. Used when creating/reusing by title, or renaming a group_id target."),
			"color":    stringSchema("Group color: grey, blue, red, yellow, green, pink, purple, cyan, orange."),
			"group_id": stringSchema("Optional existing Chrome tab group id. When set, the tabs are moved into that group."),
		}, []string{"tab_ids"})),
		tool("brw_ungroup_tabs", "Remove tabs from their Chrome tab group.", object(map[string]any{
			"tab_ids": map[string]any{"type": "array", "items": stringSchema("Tab id."), "description": "Tab IDs to ungroup."},
		}, []string{"tab_ids"})),
		tool("brw_assert_visible", "Assert that an element ref is visible. Retries until visible or timeout (web-first assertion).", object(map[string]any{
			"ref":        stringSchema("Element ref from brw_snapshot."),
			"timeout_ms": integerSchema("Timeout in milliseconds. Defaults to 5000."),
			"tab_id":     stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref"})),
		tool("brw_assert_text", "Assert that an element ref contains the expected text (case-insensitive substring). Retries until matched or timeout.", object(map[string]any{
			"ref":        stringSchema("Element ref from brw_snapshot."),
			"text":       stringSchema("Expected text substring (case-insensitive)."),
			"timeout_ms": integerSchema("Timeout in milliseconds. Defaults to 5000."),
			"tab_id":     stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref", "text"})),
		tool("brw_assert_value", "Assert that an element ref has the expected value (exact match). Retries until matched or timeout.", object(map[string]any{
			"ref":        stringSchema("Element ref from brw_snapshot."),
			"value":      stringSchema("Expected value (exact match)."),
			"timeout_ms": integerSchema("Timeout in milliseconds. Defaults to 5000."),
			"tab_id":     stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref", "value"})),
		tool("brw_assert_hidden", "Assert that an element ref is hidden or absent from the DOM. Retries until hidden or timeout.", object(map[string]any{
			"ref":        stringSchema("Element ref from brw_snapshot."),
			"timeout_ms": integerSchema("Timeout in milliseconds. Defaults to 5000."),
			"tab_id":     stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref"})),
		tool("brw_commit", "Commit a form field: submits the enclosing form (via submit button or requestSubmit) or presses Enter if no form. Use after filling a field that requires explicit submission.", object(map[string]any{
			"ref":    stringSchema("Element ref from brw_snapshot."),
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"ref"})),
		tool("brw_notify", "Raise a desktop notification to pull the human operator back at a hand-off point (needs_input for MFA/CAPTCHA/purchase confirmation), on completion (done), or on failure (error) — useful when the user has tabbed away. With the Chrome extension bridge this uses chrome.notifications and surfaces even when the tab is backgrounded; on a direct-CDP session it falls back to the in-page Notification API (best-effort, subject to page focus/permission). The result reports the honest delivery channel (extension, page, or unavailable).", object(map[string]any{
			"kind":    stringSchema("Hand-off classification: needs_input (default), done, or error."),
			"title":   stringSchema("Short notification heading. Defaults to a kind-appropriate title."),
			"message": stringSchema("Notification body text."),
		}, nil)),
		tool("brw_click_xy", "Click at specific viewport coordinates (x, y). Returns the element that was clicked. Use for canvas interactions or when semantic refs are not available.", object(map[string]any{
			"x":      map[string]any{"type": "number", "description": "X coordinate in viewport pixels."},
			"y":      map[string]any{"type": "number", "description": "Y coordinate in viewport pixels."},
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, []string{"x", "y"})),
		tool("brw_window_resize", "Move or resize the REAL OS browser window. Not the same as brw_emulate_device, which overrides viewport metrics inside the renderer for responsive testing — this changes the window a human sees, which is what desktop-aware layouts and window-manager-sensitive apps key off. Returns the geometry Chrome settled on, so no follow-up read is needed; Chrome clamps to the display, so clamped:true means the applied size differs from the one requested.", object(map[string]any{
			"width":  integerSchema("Window width in pixels."),
			"height": integerSchema("Window height in pixels."),
			"left":   integerSchema("Window left edge in screen pixels."),
			"top":    integerSchema("Window top edge in screen pixels."),
			"state":  stringEnumSchema("Window state. Passing width/height together with maximized or fullscreen sizes the window first, then applies the state.", "normal", "minimized", "maximized", "fullscreen"),
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_window_bounds", "Return the tab window/viewport geometry for mapping a SCREEN pixel (from an OS/desktop screenshot) into viewport CSS pixels for brw_click_xy. Fields (CSS px unless noted): device_pixel_ratio, screen_x/screen_y (viewport top-left in screen coords), inner_width/inner_height (viewport), outer_width/outer_height (window), scroll_x/scroll_y, screen_width/screen_height. Map with: viewport_css_x = screen_device_x / device_pixel_ratio - screen_x (same for y).", object(map[string]any{
			"tab_id": stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
		}, nil)),
		tool("brw_console", "Return buffered console messages (log, warn, error, info) from the page. Console output is the most verbose thing a page produces — filter with only_errors or pattern rather than pulling every line into context. Filtered-out messages stay buffered and are still readable by a later, wider call.", object(map[string]any{
			"tab_id":      stringSchema("Tab id from brw_list_tabs. Omit for the active tab."),
			"only_errors": boolSchema("Return only error-severity messages. Usually what you want when diagnosing a failure."),
			"level":       stringSchema("Return only this exact level, for example warn."),
			"pattern":     stringSchema("Regular expression the message text must match."),
			"limit":       integerSchema("Maximum messages to return, taken from the most recent. Defaults to 100; -1 for no cap."),
			"clear":       boolSchema("Drop the returned messages from the buffer. Defaults true. Set false to re-read them later."),
		}, nil)),
		tool("brw_downloads", "Return and drain tracked file downloads with url, suggested_filename, state (inProgress/completed/canceled), received_bytes, total_bytes, guid, and path. The buffer clears after reading. Branch on supported=false, which only an extension build predating download support returns.", object(nil, nil)),
		tool("brw_trace", "Return the action trace: recent actions with their refs, the element each one acted on, timing, and outcomes. format:\"batch\" instead returns the same flow as a ready-to-run brw_batch steps array — do a flow once, get a deterministic replay script with no model in the loop. Each ref action is preceded by an assert step checking the ref still points at the element that was recorded, so a replay against a changed page fails loudly instead of acting on the wrong element. Coordinate-driven actions (drag, click_xy) and history navigation are not replayable and are reported under skipped_reasons rather than dropped silently.", object(map[string]any{
			"format":         stringEnumSchema("entries (default, the raw log) or batch (a brw_batch steps array that reproduces the flow).", "entries", "batch"),
			"guards":         boolSchema("format:batch only. Insert an assert step before each action to verify its ref still points at the recorded element. Defaults true; a replay that clicks the wrong element in silence is worse than one that fails."),
			"include_failed": boolSchema("format:batch only. Keep steps whose action failed when recorded. Defaults true so the export is a faithful record; set false to export only what worked."),
		}, nil)),
		tool("brw_clear_trace", "Clear the action trace buffer.", object(nil, nil)),
	}
}

func tool(name, description string, schema map[string]any) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": schema}
}

func object(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func stringSchema(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func stringEnumSchema(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

func boolSchema(description string) map[string]any {
	return map[string]any{"type": "boolean", "description": description}
}

func integerSchema(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

// mousePointSchema describes a drag endpoint: a semantic ref OR x,y coordinates.
func mousePointSchema(description string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": description,
		"properties": map[string]any{
			"ref": stringSchema("Element ref, for example e18."),
			"x":   map[string]any{"type": "number", "description": "X coordinate in viewport pixels."},
			"y":   map[string]any{"type": "number", "description": "Y coordinate in viewport pixels."},
		},
	}
}
