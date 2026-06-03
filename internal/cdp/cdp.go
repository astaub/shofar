// Package cdp is a tiny, dependency-free Chrome DevTools Protocol client. It
// speaks just enough of the protocol to list page targets and read each tab's
// JS-heap memory, over a hand-rolled WebSocket so shofar stays a single
// zero-dependency, easy-to-audit binary.
//
// It reports JSHeapUsedSize (the DevTools "Performance" metric), which is a
// per-tab memory figure — not the full process RSS Chrome's Task Manager shows,
// but the right signal for ranking which tabs are heavy.
package cdp

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Tab is one page target with its measured JS-heap memory.
type Tab struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	JSHeapBytes uint64 `json:"js_heap_bytes"`
}

type targetEntry struct {
	Type                 string `json:"type"`
	Title                string `json:"title"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Available reports whether a CDP endpoint is answering at host:port.
func Available(host string, port int) bool {
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://%s:%d/json/version", host, port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// Tabs lists page tabs on the CDP endpoint at host:port and measures each tab's
// JS-heap memory.
func Tabs(host string, port int) ([]Tab, error) {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://%s:%d/json", host, port))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var entries []targetEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, err
	}
	var tabs []Tab
	for _, e := range entries {
		if e.Type != "page" || e.WebSocketDebuggerURL == "" {
			continue
		}
		if strings.HasPrefix(e.URL, "devtools://") || strings.HasPrefix(e.URL, "chrome-extension://") {
			continue
		}
		heap, _ := jsHeap(e.WebSocketDebuggerURL) // best-effort; 0 on failure
		tabs = append(tabs, Tab{Title: e.Title, URL: e.URL, JSHeapBytes: heap})
	}
	return tabs, nil
}

// jsHeap opens a WebSocket to one page target and returns its JSHeapUsedSize.
func jsHeap(wsURL string) (uint64, error) {
	conn, err := wsDial(wsURL)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if err := conn.send(`{"id":1,"method":"Performance.enable"}`); err != nil {
		return 0, err
	}
	_, _ = conn.recv() // ack for id 1
	if err := conn.send(`{"id":2,"method":"Performance.getMetrics"}`); err != nil {
		return 0, err
	}
	for {
		msg, err := conn.recv()
		if err != nil {
			return 0, err
		}
		var r struct {
			ID     int `json:"id"`
			Result struct {
				Metrics []struct {
					Name  string  `json:"name"`
					Value float64 `json:"value"`
				} `json:"metrics"`
			} `json:"result"`
		}
		if json.Unmarshal(msg, &r) != nil || r.ID != 2 {
			continue // event or other reply; keep reading
		}
		for _, m := range r.Result.Metrics {
			if m.Name == "JSHeapUsedSize" {
				return uint64(m.Value), nil
			}
		}
		return 0, nil
	}
}

// ── minimal RFC 6455 client (text frames only, client-masked) ───────────────

type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func wsDial(wsURL string) (*wsConn, error) {
	rest := strings.TrimPrefix(wsURL, "ws://")
	host, path := rest, "/"
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		host, path = rest[:i], rest[i:]
	}
	conn, err := net.DialTimeout("tcp", host, 3*time.Second)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	key := make([]byte, 16)
	rand.Read(key)
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(key) + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	r := bufio.NewReader(conn)
	status, err := r.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, "101") {
		conn.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %s", strings.TrimSpace(status))
	}
	for { // drain remaining response headers
		line, err := r.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &wsConn{conn: conn, r: r}, nil
}

func (w *wsConn) Close() error { return w.conn.Close() }

func (w *wsConn) send(text string) error {
	payload := []byte(text)
	n := len(payload)
	var hdr []byte
	const b0 = byte(0x81) // FIN + text opcode
	switch {
	case n < 126:
		hdr = []byte{b0, byte(0x80 | n)}
	case n < 65536:
		hdr = []byte{b0, 0x80 | 126, byte(n >> 8), byte(n)}
	default:
		hdr = make([]byte, 10)
		hdr[0], hdr[1] = b0, 0x80|127
		binary.BigEndian.PutUint64(hdr[2:], uint64(n))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := w.conn.Write(append(append(hdr, mask...), masked...))
	return err
}

func (w *wsConn) recv() ([]byte, error) {
	for {
		h := make([]byte, 2)
		if _, err := io.ReadFull(w.r, h); err != nil {
			return nil, err
		}
		opcode := h[0] & 0x0f
		ln := int(h[1] & 0x7f)
		switch ln {
		case 126:
			ext := make([]byte, 2)
			if _, err := io.ReadFull(w.r, ext); err != nil {
				return nil, err
			}
			ln = int(binary.BigEndian.Uint16(ext))
		case 127:
			ext := make([]byte, 8)
			if _, err := io.ReadFull(w.r, ext); err != nil {
				return nil, err
			}
			ln = int(binary.BigEndian.Uint64(ext))
		}
		if h[1]&0x80 != 0 { // server frames shouldn't be masked, but handle it
			mk := make([]byte, 4)
			io.ReadFull(w.r, mk)
		}
		payload := make([]byte, ln)
		if _, err := io.ReadFull(w.r, payload); err != nil {
			return nil, err
		}
		switch opcode {
		case 0x1, 0x2: // text / binary
			return payload, nil
		case 0x8: // close
			return nil, io.EOF
		default: // ping/pong/continuation — keep reading
			continue
		}
	}
}
