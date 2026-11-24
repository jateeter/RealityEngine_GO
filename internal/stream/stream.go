// Package stream is the engines' event streams (RealityEngine_GO#11):
// a broadcast hub, Server-Sent Events and RFC 6455 WebSocket, with no
// third-party code. SURFACE_SPEC "Streaming events" is the contract: SSE
// frames each message as `data: <json>`, opens with a `: connected` comment
// and keeps the line open with `: keepalive` every SSE_HEARTBEAT_MS (15 s); a
// WebSocket client gets text frames and a ping on the same interval.
package stream

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// capacity is how many messages a slow subscriber may fall behind before
// the oldest are dropped, as a lagged broadcast receiver skips ahead.
const capacity = 256

// Hub broadcasts messages to every subscriber.
type Hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

// NewHub is an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan string]struct{}{}} }

// Listened reports whether anyone is subscribed: a message is built only
// when someone will receive it.
func (h *Hub) Listened() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs) > 0
}

// Subscribe returns a channel of messages and the function that ends the
// subscription.
func (h *Hub) Subscribe() (<-chan string, func()) {
	ch := make(chan string, capacity)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Publish sends msg to every subscriber; a full subscriber loses its oldest
// message rather than holding the publisher back.
func (h *Hub) Publish(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		for {
			select {
			case ch <- msg:
			default:
				select {
				case <-ch:
				default:
				}
				continue
			}
			break
		}
	}
}

// Heartbeat is SSE_HEARTBEAT_MS (at least 10), default 15 s.
func Heartbeat() time.Duration {
	if ms, err := strconv.Atoi(os.Getenv("SSE_HEARTBEAT_MS")); err == nil && ms >= 10 {
		return time.Duration(ms) * time.Millisecond
	}
	return 15 * time.Second
}

// ServeSSE streams the hub's messages until the client goes away.
func ServeSSE(w http.ResponseWriter, r *http.Request, h *Hub) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	messages, cancel := h.Subscribe()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	write := func(s string) bool {
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write(": connected\n\n") {
		return
	}
	beat := time.NewTicker(Heartbeat())
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-messages:
			var b strings.Builder
			for _, line := range strings.Split(msg, "\n") {
				b.WriteString("data: " + line + "\n")
			}
			b.WriteString("\n")
			if !write(b.String()) {
				return
			}
		case <-beat.C:
			if !write(": keepalive\n\n") {
				return
			}
		}
	}
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// ServeWS upgrades to a WebSocket, sends first (when not empty), then the
// hub's messages as text frames and a ping every heartbeat, until the client
// closes or goes away. Messages the client sends are read and discarded.
func ServeWS(w http.ResponseWriter, r *http.Request, h *Hub, first string) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Key") == "" {
		http.Error(w, "expected a WebSocket upgrade", http.StatusBadRequest)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "upgrade unsupported", http.StatusInternalServerError)
		return
	}
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
	conn, rw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	accept := base64.StdEncoding.EncodeToString(sum[:])
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"); err != nil || rw.Flush() != nil {
		return
	}
	messages, cancel := h.Subscribe()
	defer cancel()
	closed := make(chan struct{})
	go func() { // the reader: discard data frames, stop on close or error
		defer close(closed)
		readFrames(rw.Reader)
	}()
	var wmu sync.Mutex
	send := func(op byte, payload []byte) bool {
		wmu.Lock()
		defer wmu.Unlock()
		if _, err := conn.Write(frame(op, payload)); err != nil {
			return false
		}
		return true
	}
	if first != "" && !send(0x1, []byte(first)) {
		return
	}
	beat := time.NewTicker(Heartbeat())
	defer beat.Stop()
	for {
		select {
		case <-closed:
			send(0x8, nil)
			return
		case msg := <-messages:
			if !send(0x1, []byte(msg)) {
				return
			}
		case <-beat.C:
			if !send(0x9, nil) {
				return
			}
		}
	}
}

// frame is one unmasked, unfragmented server frame.
func frame(op byte, payload []byte) []byte {
	head := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		head = append(head, byte(n))
	case n <= 0xFFFF:
		head = append(head, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		head = append(append(head, 127), ext[:]...)
	}
	return append(head, payload...)
}

// readFrames reads client frames until a close frame or an error.
func readFrames(r *bufio.Reader) {
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return
		}
		op, n := h[0]&0x0F, uint64(h[1]&0x7F)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return
			}
			n = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return
			}
			n = binary.BigEndian.Uint64(ext[:])
		}
		if h[1]&0x80 != 0 {
			var mask [4]byte
			if _, err := io.ReadFull(r, mask[:]); err != nil {
				return
			}
		}
		if _, err := io.CopyN(io.Discard, r, int64(n)); err != nil {
			return
		}
		if op == 0x8 {
			return
		}
	}
}
