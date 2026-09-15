package harness

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestMeterCountsRealWebSocketTraffic drives a real websocket client through
// the relay to a real websocket server and checks what it counted.
//
// Nothing here is stubbed on the path under test: coder/websocket masks the
// client frames and leaves the server frames unmasked, picks the 7-bit, 16-bit
// and 64-bit length forms by payload size, and does its own close handshake, so
// the frame walker meets every shape it has to handle.
func TestMeterCountsRealWebSocketTraffic(t *testing.T) {
	var observedHost atomic.Value
	server, wsPath := startEchoServer(t, &observedHost, nil)

	meter, err := StartMeter(server.URL)
	if err != nil {
		t.Fatalf("start meter: %v", err)
	}
	defer meter.Close()

	if !strings.HasSuffix(meter.BrowserWSURL(), wsPath) {
		t.Fatalf("meter url %q does not keep the browser websocket path %q", meter.BrowserWSURL(), wsPath)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, meter.BrowserWSURL(), nil)
	if err != nil {
		t.Fatalf("dial through meter: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(4 << 20)

	// Short, 16-bit-length and 64-bit-length payloads, so every header form the
	// walker parses is exercised.
	payloads := []string{
		`{"id":1,"method":"Page.enable"}`,
		strings.Repeat("a", 400),
		strings.Repeat("b", 70000),
	}
	var sent int
	for _, payload := range payloads {
		if err := conn.Write(ctx, websocket.MessageText, []byte(payload)); err != nil {
			t.Fatalf("write: %v", err)
		}
		sent += len(payload)
		kind, echoed, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if kind != websocket.MessageText || string(echoed) != payload {
			t.Fatalf("echo mismatch: got %d bytes of kind %v", len(echoed), kind)
		}
	}

	counters := meter.Read()
	if counters.CDPCommands != int64(len(payloads)) {
		t.Errorf("commands counted = %d, want %d", counters.CDPCommands, len(payloads))
	}
	if counters.CDPMessages != int64(len(payloads)) {
		t.Errorf("messages counted = %d, want %d", counters.CDPMessages, len(payloads))
	}
	if counters.TransportBytesTx <= int64(sent) {
		t.Errorf("tx bytes = %d, want more than the %d payload bytes plus framing", counters.TransportBytesTx, sent)
	}
	if counters.TransportBytesRx < int64(sent) {
		t.Errorf("rx bytes = %d, want at least the %d echoed payload bytes", counters.TransportBytesRx, sent)
	}

	host, _ := observedHost.Load().(string)
	if host != strings.TrimPrefix(server.URL, "http://") {
		t.Errorf("upstream saw Host %q, want the browser's own address %q", host, strings.TrimPrefix(server.URL, "http://"))
	}
}

// TestMeterForwardsPayloadsUnchanged is the other half: a meter that counted
// correctly but corrupted a large frame would make every benchmark run fail in
// a way that looks like a browser bug.
func TestMeterForwardsPayloadsUnchanged(t *testing.T) {
	var observedHost atomic.Value
	server, _ := startEchoServer(t, &observedHost, nil)
	meter, err := StartMeter(server.URL)
	if err != nil {
		t.Fatalf("start meter: %v", err)
	}
	defer meter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, meter.BrowserWSURL(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	conn.SetReadLimit(8 << 20)

	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, echoed, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(echoed) != len(payload) {
		t.Fatalf("echoed %d bytes, want %d", len(echoed), len(payload))
	}
	for i := range payload {
		if echoed[i] != payload[i] {
			t.Fatalf("byte %d changed in transit: got %d want %d", i, echoed[i], payload[i])
		}
	}
}

// TestMeterCarriesNothingButItsOwnUpgrade is the DNS-rebinding property.
//
// Chrome refuses a DevTools request whose Host header is not its own address,
// which is what keeps web content off the debugging port. The meter sits in
// front of that check and used to rewrite the header for anything that arrived,
// so a page that rebound a name to 127.0.0.1 and found the relay's port could
// read /json/version, take the browser UUID out of it and open a full CDP
// session. Every row here is a request that must never reach the browser.
func TestMeterCarriesNothingButItsOwnUpgrade(t *testing.T) {
	var observedHost atomic.Value
	var upstream atomic.Int64
	server, wsPath := startEchoServer(t, &observedHost, &upstream)

	meter, err := StartMeter(server.URL)
	if err != nil {
		t.Fatalf("start meter: %v", err)
	}
	defer meter.Close()
	relay := meter.listen
	// StartMeter resolves the websocket itself; only what arrives through the
	// relay from here on counts as upstream contact.
	upstream.Store(0)

	upgrade := "Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: AAAAAAAAAAAAAAAAAAAAAA==\r\n"
	cases := []struct {
		name string
		head string
	}{
		{
			name: "rebound name asks for the browser list",
			head: "GET /json/version HTTP/1.1\r\nHost: evil.test:1234\r\n\r\n",
		},
		{
			name: "the relay's own address asks for the browser list",
			head: "GET /json/version HTTP/1.1\r\nHost: " + relay + "\r\n\r\n",
		},
		{
			name: "rebound name upgrades on the right path",
			head: "GET " + wsPath + " HTTP/1.1\r\nHost: evil.test:1234\r\n" + upgrade + "\r\n",
		},
		{
			name: "right host, another target's websocket",
			head: "GET /devtools/page/some-tab HTTP/1.1\r\nHost: " + relay + "\r\n" + upgrade + "\r\n",
		},
		{
			name: "right host and path, no upgrade",
			head: "GET " + wsPath + " HTTP/1.1\r\nHost: " + relay + "\r\n\r\n",
		},
		{
			name: "right host and path, not a GET",
			head: "POST " + wsPath + " HTTP/1.1\r\nHost: " + relay + "\r\n" + upgrade + "\r\n",
		},
		{
			name: "two host headers, one of them the relay's",
			head: "GET " + wsPath + " HTTP/1.1\r\nHost: " + relay + "\r\nHost: evil.test:1234\r\n" + upgrade + "\r\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := speakToMeter(t, relay, testCase.head)
			if !strings.HasPrefix(status, "HTTP/1.1 403") {
				t.Fatalf("meter answered %q with %q and body %q, want a refusal", testCase.head, status, body)
			}
		})
	}
	if reached := upstream.Load(); reached != 0 {
		t.Fatalf("%d refused requests still reached the browser", reached)
	}
	if host, _ := observedHost.Load().(string); host != "" {
		t.Fatalf("the browser saw a handshake with Host %q from a refused request", host)
	}
}

// speakToMeter writes one raw HTTP head at the relay and returns its status
// line and whatever body followed.
func speakToMeter(t *testing.T, address, head string) (string, string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		t.Fatalf("dial the relay: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, head); err != nil {
		t.Fatalf("write the head: %v", err)
	}
	answer, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	status, body, _ := strings.Cut(string(answer), "\r\n")
	return status, body
}

func startEchoServer(t *testing.T, observedHost *atomic.Value, upstream *atomic.Int64) (*httptest.Server, string) {
	t.Helper()
	const wsPath = "/devtools/browser/fixture-id"
	mux := http.NewServeMux()
	var base string
	if upstream == nil {
		upstream = &atomic.Int64{}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		http.NotFound(w, r)
	})
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"Browser":"Chrome/fixture","Protocol-Version":"1.3","webSocketDebuggerUrl":"ws://%s%s"}`,
			strings.TrimPrefix(base, "http://"), wsPath)
	})
	mux.HandleFunc(wsPath, func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		observedHost.Store(r.Host)
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusInternalError, "")
		conn.SetReadLimit(8 << 20)
		for {
			kind, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err := conn.Write(r.Context(), kind, data); err != nil {
				return
			}
		}
	})
	server := httptest.NewServer(mux)
	base = server.URL
	t.Cleanup(server.Close)
	return server, wsPath
}

// TestFrameCounterHandlesEveryFrameShape drives the walker directly with
// hand-built frames, because a live client never produces some of them: a
// fragmented message, an interleaved control frame, and a zero-length frame all
// have to be classified, and every one of them is a way to miscount.
func TestFrameCounterHandlesEveryFrameShape(t *testing.T) {
	cases := []struct {
		name   string
		frames [][]byte
		want   int64
	}{
		{
			name:   "single unmasked text frame",
			frames: [][]byte{frame(true, 1, false, []byte("hello"))},
			want:   1,
		},
		{
			name:   "single masked text frame",
			frames: [][]byte{frame(true, 1, true, []byte("hello"))},
			want:   1,
		},
		{
			name: "fragmented message counts once",
			frames: [][]byte{
				frame(false, 1, false, []byte("part one ")),
				frame(false, 0, false, []byte("part two ")),
				frame(true, 0, false, []byte("part three")),
			},
			want: 1,
		},
		{
			name: "control frames are not messages",
			frames: [][]byte{
				frame(true, 9, false, []byte("ping")),
				frame(true, 10, false, []byte("pong")),
				frame(true, 8, false, []byte{0x03, 0xe8}),
			},
			want: 0,
		},
		{
			name: "control frame between fragments",
			frames: [][]byte{
				frame(false, 2, false, []byte("bin")),
				frame(true, 9, false, []byte("ping")),
				frame(true, 0, false, []byte("ary")),
			},
			want: 1,
		},
		{
			name:   "zero length frame is a message",
			frames: [][]byte{frame(true, 1, false, nil)},
			want:   1,
		},
		{
			name: "extended length forms",
			frames: [][]byte{
				frame(true, 1, false, make([]byte, 200)),
				frame(true, 1, true, make([]byte, 70000)),
			},
			want: 2,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var stream []byte
			for _, f := range testCase.frames {
				stream = append(stream, f...)
			}
			// Feed the same stream in several chunk sizes: a header split across
			// two reads is the normal case on a real socket and the one a naive
			// parser gets wrong.
			for _, chunk := range []int{1, 3, 7, 4096, len(stream)} {
				if chunk <= 0 {
					continue
				}
				var pair counterPair
				counter := &frameCounter{dir: &pair}
				for offset := 0; offset < len(stream); offset += chunk {
					end := offset + chunk
					if end > len(stream) {
						end = len(stream)
					}
					counter.consume(stream[offset:end])
				}
				if got := pair.messages.Load(); got != testCase.want {
					t.Errorf("chunk %d: counted %d messages, want %d", chunk, got, testCase.want)
				}
				if got := pair.bytes.Load(); got != int64(len(stream)) {
					t.Errorf("chunk %d: counted %d bytes, want %d", chunk, got, len(stream))
				}
			}
		})
	}
}

// frame builds one websocket frame the way a peer would put it on the wire.
func frame(fin bool, opcode byte, masked bool, payload []byte) []byte {
	var out []byte
	first := opcode & 0x0f
	if fin {
		first |= 0x80
	}
	out = append(out, first)

	length := len(payload)
	var second byte
	if masked {
		second = 0x80
	}
	switch {
	case length < 126:
		out = append(out, second|byte(length))
	case length < 1<<16:
		out = append(out, second|126)
		var size [2]byte
		binary.BigEndian.PutUint16(size[:], uint16(length))
		out = append(out, size[:]...)
	default:
		out = append(out, second|127)
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(length))
		out = append(out, size[:]...)
	}

	body := append([]byte(nil), payload...)
	if masked {
		key := [4]byte{0x12, 0x34, 0x56, 0x78}
		out = append(out, key[:]...)
		for i := range body {
			body[i] ^= key[i%4]
		}
	}
	return append(out, body...)
}
