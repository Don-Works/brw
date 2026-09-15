package harness

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Counters is a reading of the metered CDP transport.
//
// Commands and messages are counted as complete WebSocket data messages in each
// direction. Chrome multiplexes every target session over the one browser
// websocket, so this is the whole conversation between brw and the browser, not
// one tab's share of it.
type Counters struct {
	CDPCommands      int64 `json:"cdp_commands"`
	CDPMessages      int64 `json:"cdp_messages"`
	TransportBytesTx int64 `json:"transport_bytes_tx"`
	TransportBytesRx int64 `json:"transport_bytes_rx"`
}

// Sub returns the traffic that happened between two readings.
func (c Counters) Sub(earlier Counters) Counters {
	return Counters{
		CDPCommands:      c.CDPCommands - earlier.CDPCommands,
		CDPMessages:      c.CDPMessages - earlier.CDPMessages,
		TransportBytesTx: c.TransportBytesTx - earlier.TransportBytesTx,
		TransportBytesRx: c.TransportBytesRx - earlier.TransportBytesRx,
	}
}

// Add accumulates a reading into a running total.
func (c Counters) Add(other Counters) Counters {
	return Counters{
		CDPCommands:      c.CDPCommands + other.CDPCommands,
		CDPMessages:      c.CDPMessages + other.CDPMessages,
		TransportBytesTx: c.TransportBytesTx + other.TransportBytesTx,
		TransportBytesRx: c.TransportBytesRx + other.TransportBytesRx,
	}
}

// Meter is a loopback relay between brw's CDP client and Chrome's debugging
// port. It forwards bytes untouched and counts what crosses it.
//
// Counting at the socket is the only place the numbers are real: chromedp
// exposes no hook for "how many commands did that call issue", and an estimate
// made from the Go call graph would be an assertion about the code rather than
// a measurement of the browser conversation.
type Meter struct {
	ln net.Listener
	// target and path are Chrome's own address and browser-websocket path;
	// listen is the relay's address. The handshake check compares against the
	// relay's address and the rewrite that follows it uses the browser's, so
	// both have to be kept.
	target   string
	path     string
	listen   string
	browser  string
	tx       counterPair
	rx       counterPair
	mu       sync.Mutex
	conns    []net.Conn
	closed   atomic.Bool
	wg       sync.WaitGroup
	dialWait time.Duration
}

type counterPair struct {
	messages atomic.Int64
	bytes    atomic.Int64
}

// StartMeter resolves Chrome's browser websocket from its HTTP debugging
// endpoint and returns a relay in front of it.
func StartMeter(chromeEndpoint string) (*Meter, error) {
	wsURL, err := browserWebSocketURL(chromeEndpoint)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(wsURL)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	m := &Meter{
		ln:       ln,
		target:   parsed.Host,
		path:     parsed.Path,
		listen:   ln.Addr().String(),
		dialWait: 10 * time.Second,
	}
	m.browser = "ws://" + m.listen + parsed.Path
	m.wg.Add(1)
	go m.accept()
	return m, nil
}

// BrowserWSURL is the endpoint to hand a CDP client so its traffic is metered.
// It keeps the "/devtools/browser/<id>" path, which is what tells chromedp this
// is already a browser websocket and stops it re-resolving one straight from
// Chrome and bypassing the meter.
func (m *Meter) BrowserWSURL() string { return m.browser }

// Read samples the counters.
func (m *Meter) Read() Counters {
	return Counters{
		CDPCommands:      m.tx.messages.Load(),
		CDPMessages:      m.rx.messages.Load(),
		TransportBytesTx: m.tx.bytes.Load(),
		TransportBytesRx: m.rx.bytes.Load(),
	}
}

// Close stops the relay and drops any live connection.
func (m *Meter) Close() error {
	if m == nil || !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	err := m.ln.Close()
	m.mu.Lock()
	for _, conn := range m.conns {
		_ = conn.Close()
	}
	m.conns = nil
	m.mu.Unlock()
	m.wg.Wait()
	return err
}

func (m *Meter) accept() {
	defer m.wg.Done()
	for {
		conn, err := m.ln.Accept()
		if err != nil {
			return
		}
		m.track(conn)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.relay(conn)
		}()
	}
}

// track registers a connection so Close can drop it.
//
// A connection registered AFTER Close has swept the list is closed immediately
// instead: a relay that was mid-dial when Close ran would otherwise leave a
// socket nothing ever closes, and Close's wait for its goroutines would never
// return.
func (m *Meter) track(conn net.Conn) {
	m.mu.Lock()
	if m.closed.Load() {
		m.mu.Unlock()
		_ = conn.Close()
		return
	}
	m.conns = append(m.conns, conn)
	m.mu.Unlock()
}

func (m *Meter) relay(client net.Conn) {
	defer client.Close()

	clientReader := bufio.NewReader(client)
	head, err := readHTTPHead(clientReader)
	if err != nil {
		return
	}
	// Refused before the dial, so a request the relay will not carry never opens
	// a connection to the browser at all.
	if reason := m.refuseHandshake(head); reason != "" {
		writeRefusal(client, reason)
		return
	}

	server, err := net.DialTimeout("tcp", m.target, m.dialWait)
	if err != nil {
		return
	}
	defer server.Close()
	m.track(server)
	serverReader := bufio.NewReader(server)

	// Chrome refuses a DevTools websocket whose Host header names something
	// other than the port it is listening on, so the relay's own address has to
	// be swapped out before the handshake is forwarded. refuseHandshake has
	// already applied the same check against the relay's own address, so this
	// rewrite no longer stands in for Chrome's.
	if _, err := server.Write(rewriteHost(head, m.target)); err != nil {
		return
	}
	response, err := readHTTPHead(serverReader)
	if err != nil {
		return
	}
	if _, err := client.Write(response); err != nil {
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		pipeFrames(server, clientReader, &m.tx)
		_ = server.Close()
		done <- struct{}{}
	}()
	go func() {
		pipeFrames(client, serverReader, &m.rx)
		_ = client.Close()
		done <- struct{}{}
	}()
	<-done
	<-done
}

// refuseHandshake reports why a request must not be carried, or "" for the one
// websocket upgrade the meter exists to relay.
//
// Chrome's DevTools endpoint refuses a request whose Host header is not its own
// listening address. That check is what stops web content from reaching the
// debugging port after rebinding a name to 127.0.0.1, and rewriting the header
// on the way through replaced it with nothing: anything arriving on the relay's
// port was re-addressed to the browser and answered, /json/version included,
// which hands out the browser UUID and with it a full CDP session. The relay
// therefore applies the same check against its OWN address, and carries nothing
// but the exact upgrade StartMeter resolved: a rebound name cannot present the
// relay's literal address as its Host, and an ordinary cross-origin fetch is
// not an upgrade.
func (m *Meter) refuseHandshake(head []byte) string {
	lines := strings.Split(strings.TrimRight(string(head), "\r\n"), "\r\n")
	fields := strings.Fields(lines[0])
	if len(fields) < 2 {
		return "malformed request line"
	}
	if fields[0] != http.MethodGet || fields[1] != m.path {
		return "only the browser websocket upgrade is relayed"
	}

	var hosts, upgrade, connection []string
	for _, line := range lines[1:] {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "host":
			hosts = append(hosts, value)
		case "upgrade":
			upgrade = append(upgrade, value)
		case "connection":
			connection = append(connection, value)
		}
	}
	// Exactly one Host: two of them leave the relay and the browser disagreeing
	// about which is authoritative, which is the shape of request smuggling.
	if len(hosts) != 1 {
		return "a relayed request carries exactly one host header"
	}
	if !strings.EqualFold(hosts[0], m.listen) {
		return "host header is not the relay's own address"
	}
	if !headerHasToken(upgrade, "websocket") || !headerHasToken(connection, "upgrade") {
		return "only a websocket upgrade is relayed"
	}
	return ""
}

// headerHasToken reports whether any of a header's values carries the token,
// which is how "Connection: keep-alive, Upgrade" has to be read.
func headerHasToken(values []string, token string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// writeRefusal answers a request the relay will not carry, so a mis-plumbed
// client sees why rather than a closed socket.
func writeRefusal(client net.Conn, reason string) {
	body := reason + "\n"
	_, _ = fmt.Fprintf(client,
		"HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body)
}

// readHTTPHead reads up to and including the blank line that ends an HTTP
// message head. Everything after it on a 101 connection is websocket frames.
func readHTTPHead(r *bufio.Reader) ([]byte, error) {
	const limit = 64 * 1024
	head := make([]byte, 0, 512)
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		head = append(head, b)
		if len(head) >= 4 && string(head[len(head)-4:]) == "\r\n\r\n" {
			return head, nil
		}
		if len(head) > limit {
			return nil, errors.New("http head too large")
		}
	}
}

func rewriteHost(head []byte, host string) []byte {
	lines := strings.Split(string(head), "\r\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.ToLower(line), "host:") {
			lines[i] = "Host: " + host
		}
	}
	return []byte(strings.Join(lines, "\r\n"))
}

func pipeFrames(dst io.Writer, src io.Reader, dir *counterPair) {
	counter := &frameCounter{dir: dir}
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			counter.consume(buf[:n])
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// frameCounter walks a websocket byte stream and counts complete data messages
// without buffering a payload: it accumulates a frame header until it parses,
// then skips the payload length it declares. A 40 MB screenshot therefore costs
// the meter nothing but the byte count.
type frameCounter struct {
	dir     *counterPair
	header  []byte
	payload int64
}

func (f *frameCounter) consume(p []byte) {
	f.dir.bytes.Add(int64(len(p)))
	for {
		f.commitReady()
		if len(p) == 0 {
			return
		}
		if f.payload > 0 {
			skip := int64(len(p))
			if skip > f.payload {
				skip = f.payload
			}
			f.payload -= skip
			p = p[skip:]
			continue
		}
		need := frameHeaderSize(f.header)
		take := need - len(f.header)
		if take > len(p) {
			take = len(p)
		}
		f.header = append(f.header, p[:take]...)
		p = p[take:]
	}
}

func (f *frameCounter) commitReady() {
	for f.payload == 0 && len(f.header) >= 2 && len(f.header) >= frameHeaderSize(f.header) {
		f.commit()
	}
}

func (f *frameCounter) commit() {
	head := f.header
	length := int64(head[1] & 0x7f)
	switch length {
	case 126:
		length = int64(binary.BigEndian.Uint16(head[2:4]))
	case 127:
		length = int64(binary.BigEndian.Uint64(head[2:10]) & 0x7fffffffffffffff)
	}
	f.payload = length
	// A message is counted once, on the frame that finishes it. Opcodes 0, 1
	// and 2 are continuation, text and binary; 8 and above are control frames
	// (close, ping, pong), which are transport chatter and not CDP messages.
	if head[0]&0x80 != 0 && head[0]&0x0f < 8 {
		f.dir.messages.Add(1)
	}
	f.header = f.header[:0]
}

// frameHeaderSize reports how many bytes this frame's header occupies. With
// fewer than two bytes in hand the answer is "two more", because the second
// byte carries both the mask bit and the length class that decide the rest.
func frameHeaderSize(head []byte) int {
	if len(head) < 2 {
		return 2
	}
	size := 2
	switch head[1] & 0x7f {
	case 126:
		size += 2
	case 127:
		size += 8
	}
	if head[1]&0x80 != 0 {
		size += 4
	}
	return size
}

func browserWebSocketURL(endpoint string) (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(strings.TrimRight(endpoint, "/") + "/json/version")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("chrome /json/version returned %s", resp.Status)
	}
	var payload struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.WebSocketDebuggerURL == "" {
		return "", errors.New("chrome reported no browser websocket")
	}
	return payload.WebSocketDebuggerURL, nil
}
