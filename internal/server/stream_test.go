package server

import (
	"bufio"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseLines opens an SSE stream and returns a reader of its lines.
func sseLines(t *testing.T, url string) *bufio.Reader {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	return bufio.NewReader(resp.Body)
}

func readUntil(t *testing.T, r *bufio.Reader, prefix string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended looking for %q: %v", prefix, err)
		}
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no %q", prefix)
	return ""
}

func TestREEngineStreamCarriesEachStepBare(t *testing.T) {
	t.Setenv("SSE_HEARTBEAT_MS", "20")
	srv := httptest.NewServer(machinesHandler(t))
	t.Cleanup(srv.Close) // after the stream's body is closed (cleanups run last first)
	r := sseLines(t, srv.URL+"/api/engine/stream")
	readUntil(t, r, ": connected")
	readUntil(t, r, ": keepalive")
	if _, err := http.Post(srv.URL+"/api/perceive", "application/json", strings.NewReader(`{"vector": [1]}`)); err != nil {
		t.Fatal(err)
	}
	line := readUntil(t, r, "data: ")
	if !strings.HasPrefix(line, `data: {"activeRegions"`) || !strings.Contains(line, `"stepNumber":0`) {
		t.Fatalf("the bare step: %.200s", line)
	}
}

func TestPEEventsAndWebSocket(t *testing.T) {
	t.Setenv("SSE_HEARTBEAT_MS", "20")
	srv := httptest.NewServer(peHandler(t, `{"machines": []}`))
	t.Cleanup(srv.Close)
	r := sseLines(t, srv.URL+"/api/events")
	readUntil(t, r, ": connected")
	if _, err := http.Post(srv.URL+"/api/sources", "application/json", strings.NewReader(`{"type": "simulated", "region": {"offset": 0, "length": 1}}`)); err != nil {
		t.Fatal(err)
	}
	if line := readUntil(t, r, "data: "); !strings.Contains(line, `"type":"state-update"`) {
		t.Fatalf("%.200s", line)
	}
	// A push the RE cannot answer still reports: success false.
	resp, err := http.Post(srv.URL+"/api/push", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if line := readUntil(t, r, "data: "); !strings.Contains(line, `"type":"push-result"`) || !strings.Contains(line, `"success":false`) {
		t.Fatalf("%.200s", line)
	}

	// WebSocket: the handshake, the current state first, then pings.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	if _, err := io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: "+key+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	status, _ := br.ReadString('\n')
	if !strings.Contains(status, "101") {
		t.Fatalf("%q", status)
	}
	for {
		line, _ := br.ReadString('\n')
		if line == "\r\n" {
			break
		}
		if strings.HasPrefix(line, "Sec-WebSocket-Accept:") && !strings.Contains(line, "BACScCJPNqyz+UBoqMH89VmURoA=") {
			t.Fatalf("accept key: %q", line)
		}
	}
	readFrame := func() (byte, []byte) {
		var h [2]byte
		if _, err := io.ReadFull(br, h[:]); err != nil {
			t.Fatal(err)
		}
		n := int(h[1] & 0x7F)
		if n == 126 {
			var ext [2]byte
			io.ReadFull(br, ext[:])
			n = int(ext[0])<<8 | int(ext[1])
		} else if n == 127 {
			var ext [8]byte
			io.ReadFull(br, ext[:])
			n = 0
			for _, b := range ext {
				n = n<<8 | int(b)
			}
		}
		payload := make([]byte, n)
		io.ReadFull(br, payload)
		return h[0] & 0x0F, payload
	}
	if op, payload := readFrame(); op != 0x1 || !strings.Contains(string(payload), `"type":"state-update"`) {
		t.Fatalf("first frame: %x %.100s", op, payload)
	}
	if op, _ := readFrame(); op != 0x9 {
		t.Fatalf("expected a ping, got %x", op)
	}
	// A masked close frame from the client ends the session.
	if _, err := conn.Write([]byte{0x88, 0x80, 1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
}
