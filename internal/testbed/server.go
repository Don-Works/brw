package testbed

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

//go:embed web/*
var assets embed.FS

const downloadBody = "brw adversarial fixture\nseed-independent download\n"

var downloadName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)

// Server owns two loopback origins sharing one deterministic run.
type Server struct {
	m           model
	servers     []*http.Server
	hosts       map[string]bool
	origins     map[string]bool
	closed      chan struct{}
	origin      string
	frameOrigin string
}

// Start starts the page and its genuinely cross-origin frame server.
func Start(config Config) (*Server, error) {
	if config.Address == "" {
		config.Address = "127.0.0.1:0"
	}
	if config.FrameAddress == "" {
		config.FrameAddress = "127.0.0.1:0"
	}
	if config.MaxEvents == 0 {
		config.MaxEvents = 256
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	first, err := listen(config.Address)
	if err != nil {
		return nil, err
	}
	second, err := listen(config.FrameAddress)
	if err != nil {
		first.Close()
		return nil, err
	}
	s := &Server{hosts: map[string]bool{}, origins: map[string]bool{}, closed: make(chan struct{})}
	s.m.state.Origin = "http://" + first.Addr().String()
	s.m.state.FrameOrigin = "http://" + second.Addr().String()
	s.origin = s.m.state.Origin
	s.frameOrigin = s.m.state.FrameOrigin
	sum := sha256.Sum256([]byte(downloadBody))
	s.m.state.DownloadSHA256 = hex.EncodeToString(sum[:])
	s.m.resetLocked(config)
	for _, ln := range []net.Listener{first, second} {
		s.hosts[ln.Addr().String()] = true
		s.origins["http://"+ln.Addr().String()] = true
	}
	for _, ln := range []net.Listener{first, second} {
		srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		s.servers = append(s.servers, srv)
		go func() { _ = srv.Serve(ln) }()
	}
	return s, nil
}

func listen(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() {
		return nil, errors.New("testbed address must use a literal loopback IP")
	}
	return net.Listen("tcp", address)
}

func validateConfig(config Config) error {
	if config.Seed < -(1<<53-1) || config.Seed > 1<<53-1 {
		return errors.New("seed must be a safe JSON integer")
	}
	if config.Chaos < 0 || config.Chaos > 3 {
		return errors.New("chaos must be between 0 and 3")
	}
	if config.MaxEvents < 1 || config.MaxEvents > 4096 {
		return errors.New("max_events must be between 1 and 4096")
	}
	return nil
}

// URL is the main page origin.
func (s *Server) URL() string { return s.origin }

// FrameURL is the second loopback origin.
func (s *Server) FrameURL() string { return s.frameOrigin }

// Close stops streams and both fixture listeners.
func (s *Server) Close() error {
	s.m.mu.Lock()
	select {
	case <-s.closed:
		s.m.mu.Unlock()
		return nil
	default:
		close(s.closed)
	}
	for ch := range s.m.listeners {
		close(ch)
		delete(s.m.listeners, ch)
	}
	s.m.mu.Unlock()
	var errs []error
	for _, srv := range s.servers {
		errs = append(errs, srv.Close())
	}
	return errors.Join(errs...)
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/reset", s.reset)
	mux.HandleFunc("POST /api/step", s.step)
	mux.HandleFunc("POST /api/ack", s.ack)
	mux.HandleFunc("POST /api/action", s.action)
	mux.HandleFunc("GET /events", s.sse)
	mux.HandleFunc("GET /ws", s.websocket)
	mux.HandleFunc("GET /download", s.download)
	mux.HandleFunc("POST /upload", s.upload)
	mux.HandleFunc("GET /fixture/status/{code}", s.status)
	mux.HandleFunc("GET /fixture/redirect", s.redirect)
	mux.HandleFunc("GET /fixture/basic-auth", s.basicAuth)
	mux.HandleFunc("GET /api/cookies", s.cookies)
	mux.HandleFunc("GET /", s.asset)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hosts[r.Host] {
			http.Error(w, "fixture host refused", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if origin := r.Header.Get("Origin"); origin != "" && !s.origins[origin] {
			http.Error(w, "fixture origin refused", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) respond(w http.ResponseWriter, code int, value any) {
	body, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "fixture encoding failed", http.StatusInternalServerError)
		return
	}
	s.write(w, code, "application/json", append(body, '\n'))
}

func (s *Server) write(w http.ResponseWriter, code int, kind string, body []byte) {
	characters := utf8.RuneCount(body)
	w.Header().Set("Content-Type", kind)
	w.Header().Set("X-Testbed-Body-Bytes", strconv.Itoa(len(body)))
	w.Header().Set("X-Testbed-Chars4-Estimate", strconv.Itoa((characters+3)/4))
	w.WriteHeader(code)
	n, err := w.Write(body)
	if err == nil && n == len(body) {
		s.m.mu.Lock()
		s.m.state.Measurements.Responses++
		s.m.state.Measurements.BodyBytes += uint64(n)
		s.m.state.Measurements.TextCharacters += uint64(characters)
		s.m.state.Measurements.Chars4Estimate += uint64((characters + 3) / 4)
		s.m.mu.Unlock()
	}
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	data, err := io.ReadAll(r.Body)
	if err != nil || !validJSON(data, value) {
		http.Error(w, "invalid fixture JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) snapshotLocked() State {
	v := s.m.state
	v.VisibleItemIDs = slices.Clone(v.VisibleItemIDs)
	v.EmittedEventIDs = slices.Clone(v.EmittedEventIDs)
	v.PendingActions = slices.Clone(v.PendingActions)
	v.ActionCounts = maps.Clone(v.ActionCounts)
	v.Reading.Facts = maps.Clone(v.Reading.Facts)
	return v
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	s.m.mu.Lock()
	v := s.snapshotLocked()
	s.m.mu.Unlock()
	s.respond(w, http.StatusOK, v)
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	config := Config{MaxEvents: 256}
	if !decode(w, r, &config) {
		return
	}
	if err := validateConfig(config); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.m.mu.Lock()
	s.m.resetLocked(config)
	v := s.snapshotLocked()
	s.m.mu.Unlock()
	s.respond(w, http.StatusOK, v)
}

func (s *Server) step(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID string `json:"run_id"`
		Count int    `json:"count"`
		Kind  string `json:"kind"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Count == 0 {
		req.Count = 1
	}
	if req.Count < 1 || req.Count > 64 || (req.Kind != "" && !slices.Contains(eventKinds, req.Kind)) {
		http.Error(w, "invalid step count or kind", http.StatusBadRequest)
		return
	}
	s.m.mu.Lock()
	if req.RunID != s.m.state.RunID {
		s.m.mu.Unlock()
		http.Error(w, "stale run", http.StatusConflict)
		return
	}
	if int(s.m.state.Cursor)+req.Count > s.m.state.MaxEvents {
		s.m.mu.Unlock()
		http.Error(w, "event budget exhausted", http.StatusConflict)
		return
	}
	events := make([]Event, 0, req.Count)
	for range req.Count {
		events = append(events, s.m.stepLocked(req.Kind))
	}
	s.m.mu.Unlock()
	s.respond(w, http.StatusOK, events)
}

type acknowledgement struct {
	RunID         string `json:"run_id"`
	Cursor        uint64 `json:"cursor"`
	AppliedCursor uint64 `json:"applied_cursor"`
}

func (s *Server) acknowledge(req acknowledgement) error {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	if req.RunID != s.m.state.RunID {
		return errors.New("stale run")
	}
	if req.Cursor > s.m.state.Cursor || req.AppliedCursor > req.Cursor {
		return errors.New("cursor exceeds emitted sequence")
	}
	s.m.state.AcknowledgedCursor = max(s.m.state.AcknowledgedCursor, req.Cursor)
	s.m.state.AppliedCursor = max(s.m.state.AppliedCursor, req.AppliedCursor)
	return nil
}

func (s *Server) ack(w http.ResponseWriter, r *http.Request) {
	var req acknowledgement
	if !decode(w, r, &req) {
		return
	}
	if err := s.acknowledge(req); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	s.respond(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RunID             string `json:"run_id"`
		Epoch             uint64 `json:"document_epoch"`
		Kind              string `json:"kind"`
		Note              string `json:"note"`
		SensitiveSupplied bool   `json:"sensitive_supplied"`
		Target            string `json:"target"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req.Note) > 1024 {
		http.Error(w, "fixture note too long", http.StatusBadRequest)
		return
	}
	s.m.mu.Lock()
	if req.RunID != s.m.state.RunID || req.Epoch != s.m.state.DocumentEpoch {
		s.m.mu.Unlock()
		http.Error(w, "stale document", http.StatusConflict)
		return
	}
	form := &s.m.state.FormState
	switch req.Kind {
	case "input-note":
		form.Note = req.Note
	case "input-password":
		form.PasswordSupplied = req.SensitiveSupplied
	case "input-card":
		form.CardSupplied = req.SensitiveSupplied
	case "save-draft":
		form.Note = req.Note
		form.DraftSaved = true
	case "submit-payment":
		form.PaymentSubmitted = true
	case "delete-account":
		form.AccountDeleted = true
	case "visual-target":
		valid := slices.ContainsFunc(s.m.state.VisualTargets, func(v VisualTarget) bool { return v.ID == req.Target })
		if !valid {
			s.m.mu.Unlock()
			http.Error(w, "unknown visual target", http.StatusBadRequest)
			return
		}
		s.m.state.VisualSelected = req.Target
	case "stable-action", "frame-click", "shadow-click", "pointer", "drag", "rich-editor", "dialog":
	default:
		s.m.mu.Unlock()
		http.Error(w, "unknown fixture action", http.StatusBadRequest)
		return
	}
	s.m.state.ActionCounts[req.Kind]++
	v := s.snapshotLocked()
	s.m.mu.Unlock()
	s.respond(w, http.StatusOK, v)
}

func (s *Server) asset(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path
	if name == "/" {
		name = "/index.html"
	}
	if name == "/frame" {
		name = "/frame.html"
	}
	if !slices.Contains([]string{"/index.html", "/frame.html", "/app.js", "/style.css", "/visual.svg"}, name) {
		http.NotFound(w, r)
		return
	}
	body, err := assets.ReadFile("web" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	kind := map[string]string{".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml"}[path.Ext(name)]
	s.write(w, http.StatusOK, kind, body)
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "fixture.txt"
	}
	if !downloadName.MatchString(name) {
		http.Error(w, "invalid fixture filename", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	s.write(w, http.StatusOK, "text/plain; charset=utf-8", []byte(downloadBody))
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, (1<<20)+(16<<10))
	if err := r.ParseMultipartForm(64 << 10); err != nil {
		http.Error(w, "invalid bounded fixture upload", http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()
	if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 || len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.Value["run_id"]) != 1 {
		http.Error(w, "fixture upload requires one file and run_id", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "fixture upload requires file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (1<<20)+1))
	if err != nil || n > 1<<20 {
		http.Error(w, "fixture upload exceeds 1 MiB", http.StatusBadRequest)
		return
	}
	name := path.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
	if !downloadName.MatchString(name) {
		http.Error(w, "invalid fixture filename", http.StatusBadRequest)
		return
	}
	u := &Upload{Filename: name, Bytes: n, SHA256: hex.EncodeToString(hash.Sum(nil))}
	s.m.mu.Lock()
	if r.FormValue("run_id") != s.m.state.RunID {
		s.m.mu.Unlock()
		http.Error(w, "stale run", http.StatusConflict)
		return
	}
	s.m.state.LastUpload = u
	s.m.state.ActionCounts["upload"]++
	s.m.mu.Unlock()
	s.respond(w, http.StatusOK, u)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	code, err := strconv.Atoi(r.PathValue("code"))
	if err != nil || code < 200 || code > 599 {
		http.Error(w, "invalid fixture status", http.StatusBadRequest)
		return
	}
	s.write(w, code, "text/plain", []byte(fmt.Sprintf("Fixture status %d\n", code)))
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request) {
	to, err := url.Parse(r.URL.Query().Get("to"))
	if err != nil || to.User != nil || to.Opaque != "" {
		http.Error(w, "invalid fixture redirect", http.StatusBadRequest)
		return
	}
	base := &url.URL{Scheme: "http", Host: r.Host}
	to = base.ResolveReference(to)
	if !s.origins[to.Scheme+"://"+to.Host] {
		http.Error(w, "redirect must stay on a fixture origin", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, to.String(), http.StatusFound)
}

func (s *Server) basicAuth(w http.ResponseWriter, r *http.Request) {
	user, password, ok := r.BasicAuth()
	if !ok || user != "fixture" || password != "fixture" {
		w.Header().Set("WWW-Authenticate", `Basic realm="brw local fixture"`)
		http.Error(w, "fixture credentials required", http.StatusUnauthorized)
		return
	}
	s.write(w, http.StatusOK, "text/plain", []byte("Authenticated local fixture\n"))
}

func (s *Server) cookies(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "testbed_public", Value: "synthetic", Path: "/", SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "testbed_private", Value: "synthetic-http-only", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	s.respond(w, http.StatusOK, map[string]any{"fixture_only": true, "names": []string{"testbed_public", "testbed_private"}})
}

func validJSON(data []byte, value any) bool {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(value) == nil && d.Decode(&struct{}{}) == io.EOF
}
