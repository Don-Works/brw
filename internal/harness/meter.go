package harness

import (
	"bufio"
	"encoding/binary"
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

// Meter is a loopback relay between brw's CDP client and Chrome's debugging port.
type Meter struct {
	ln net.Listener

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

// StartMeter resolves Chrome's browser websocket from its HTTP debugging endpoint and returns a relay in front of it.
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

func writeRefusal(client net.Conn, reason string) {
	body := reason + "\n"
	_, _ = fmt.Fprintf(client,
		"HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		len(body), body)
}

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
	_, _ = io.CopyBuffer(dst, io.TeeReader(src, &frameCounter{dir: dir}), make([]byte, 32*1024))
}

type frameCounter struct {
	dir     *counterPair
	header  []byte
	payload int64
}

func (f *frameCounter) Write(p []byte) (int, error) {
	f.consume(p)
	return len(p), nil
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

	if head[0]&0x80 != 0 && head[0]&0x0f < 8 {
		f.dir.messages.Add(1)
	}
	f.header = f.header[:0]
}

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
	metadata, err := readChromeMetadata(endpoint)
	if err != nil {
		return "", err
	}
	if metadata.WebSocketDebuggerURL == "" {
		return "", errors.New("chrome reported no browser websocket")
	}
	return metadata.WebSocketDebuggerURL, nil
}
