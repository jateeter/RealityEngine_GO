package instance

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
)

// HeaderName is the participant clock's header (SURFACE_SPEC "The header").
const HeaderName = "RE-Clock"

// Middleware is the participant clock on a server's boundary
// (RealityEngine_CI#600; SURFACE_SPEC "The instance clock"), the same rule as
// the three voters':
//
//   - every request is a receive, applied before it is handled --
//     max(local, received) + 1 with an RE-Clock header, + 1 without -- and a
//     malformed or repeated header is refused with 400 (still a receive);
//   - every response, a 404 or failure included, is a send: the clock ticks,
//     then stamps RE-Clock.
func Middleware(c *Clock, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(HeaderName)
		var received uint64
		ok := true
		switch len(values) {
		case 0:
		case 1:
			received, ok = ParseHeader(values[0])
		default:
			ok = false
		}
		stamped := &stampingWriter{ResponseWriter: w, clock: c}
		if !ok {
			c.CountHeader(HeaderRejected)
			if _, err := c.Receive(0, false); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			stamped.Header().Set("Content-Type", "application/json")
			stamped.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(stamped).Encode(map[string]string{"error": "malformed RE-Clock header"})
			return
		}
		if len(values) == 1 {
			c.CountHeader(HeaderReceived)
		} else {
			c.CountHeader(HeaderMissing)
		}
		if _, err := c.Receive(received, len(values) == 1); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(stamped, r)
		stamped.stamp() // a handler that wrote nothing still answers, stamped
	})
}

// stampingWriter ticks the clock and stamps RE-Clock once, as the response's
// header is written.
type stampingWriter struct {
	http.ResponseWriter
	clock   *Clock
	stamped bool
}

func (s *stampingWriter) stamp() {
	if s.stamped {
		return
	}
	s.stamped = true
	if lamport, err := s.clock.Send(); err == nil {
		s.ResponseWriter.Header().Set(HeaderName, s.clock.Header(lamport))
	}
}

func (s *stampingWriter) WriteHeader(status int) {
	s.stamp()
	s.ResponseWriter.WriteHeader(status)
}

func (s *stampingWriter) Write(b []byte) (int, error) {
	s.stamp()
	return s.ResponseWriter.Write(b)
}

// Flush keeps streaming routes (server-sent events) streaming.
func (s *stampingWriter) Flush() {
	s.stamp()
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack hands a WebSocket upgrade its connection. The upgrade writes its own
// handshake, which carries no RE-Clock; the request was still a receive.
func (s *stampingWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("the response writer cannot be hijacked")
	}
	s.stamped = true
	return hj.Hijack()
}

// Unwrap lets http.ResponseController reach the connection.
func (s *stampingWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// InternalHost reports whether host is an internal peer: loopback, a private or
// link-local address, a single-label name (a compose service), or a name under
// .localhost, .internal or .local -- the rule C++ is_internal_host applies.
func InternalHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if h == "" {
		return false
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".internal") || strings.HasSuffix(h, ".local") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	return !strings.Contains(h, ".")
}

// Transport is the participant clock on a client: every call to an internal
// peer is a send (stamped RE-Clock) and its response a receive. An external
// party is never stamped: the clock crosses the local integration boundaries
// only, and an instance UUID never leaves the install.
type Transport struct {
	Clock *Clock
	Base  http.RoundTripper
}

func (t *Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	if !InternalHost(r.URL.Hostname()) {
		return base.RoundTrip(r)
	}
	lamport, err := t.Clock.Send()
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.Header.Set(HeaderName, t.Clock.Header(lamport))
	resp, err := base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	// A response's header is the responder's clock; one that is absent or
	// unreadable still marks a receive, as a local event.
	received, ok := ParseHeader(resp.Header.Get(HeaderName))
	_, _ = t.Clock.Receive(received, ok)
	return resp, nil
}

// CausedBy is a completion's causedBy: the PE's own dispatch record's clock when
// it has one, else the sender's well-formed echo (a canonical lower-case UUID
// and a lamport in [0, 2^53 - 1]), else nil.
func CausedBy(own, echo any) any {
	if m, ok := own.(map[string]any); ok && m != nil {
		return m
	}
	e, ok := echo.(map[string]any)
	if !ok {
		return nil
	}
	instance, _ := e["instance"].(string)
	if !Canonical(instance) || strings.ToLower(instance) != instance {
		return nil
	}
	f, ok := e["lamport"].(float64)
	if !ok || f < 0 || f > MaxLamport || f != float64(uint64(f)) {
		return nil
	}
	return map[string]any{"instance": instance, "lamport": uint64(f)}
}
