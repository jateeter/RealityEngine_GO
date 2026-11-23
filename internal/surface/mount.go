package surface

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Surface is how a route is named on the wire: the method and the path as the
// specification writes it, with :param placeholders (SURFACE_SPEC "An
// unimplemented route answers unsupported").
func (r Route) Surface() string { return r.Method + " " + r.Path }

// HTTPMethod is the request method that reaches a route. Stream routes are
// opened with GET (SSE, and the WebSocket upgrade).
func (r Route) HTTPMethod() string {
	switch r.Method {
	case "SSE", "WS":
		return http.MethodGet
	}
	return r.Method
}

var param = regexp.MustCompile(`^:([A-Za-z_][A-Za-z0-9_]*)$`)

// Unsupported is the specified 501 envelope.
type Unsupported struct {
	Error         string `json:"error"`
	Participation string `json:"participation"`
	Surface       string `json:"surface"`
}

// NewUnsupported builds the envelope for a route.
func NewUnsupported(r Route) Unsupported {
	return Unsupported{
		Error:         r.Surface() + " is not implemented by this runtime",
		Participation: "unsupported",
		Surface:       r.Surface(),
	}
}

func unsupportedHandler(r Route) http.Handler {
	body, err := json.Marshal(NewUnsupported(r))
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		if _, err := w.Write(body); err != nil {
			log.Printf("write unsupported response for %s: %v", r.Surface(), err)
		}
	})
}

// Layers selects the routes a server answers for: the RE serves its own and the
// phase-2 K-line routes, the PE its own.
func Layers(component string) []string {
	switch strings.ToUpper(component) {
	case RE:
		return []string{RE, REPhase2}
	case PE:
		return []string{PE}
	}
	return nil
}

type segment struct {
	literal string
	param   string // set when the segment is a :param placeholder
}

type compiled struct {
	route    Route
	method   string
	segments []segment
	handler  http.Handler
}

// Router serves a component's specified routes. A route with a handler is
// served by it; every other specified route answers the 501 unsupported
// envelope; any other request goes to NotFound.
//
// Matching is by path segment. Where two routes match a path, the one with a
// literal segment at the first position where they differ wins
// (/api/machines/json/:name over /api/machines/:id/checkpoints for
// /api/machines/json/checkpoints), so the specification's overlapping routes
// all resolve, which net/http's ServeMux refuses. Path parameters are set on the
// request (Request.PathValue) under their specification names.
type Router struct {
	routes   []compiled
	NotFound http.Handler
}

// NewRouter builds the router for the given layers. handlers maps a route id to
// its handler, and must name exactly the routes in implemented that belong to
// those layers.
func NewRouter(inv Inventory, layers []string, implemented map[string]bool, handlers map[string]http.Handler) (*Router, error) {
	want := map[string]bool{}
	for _, l := range layers {
		want[l] = true
	}
	inLayers := map[string]bool{}
	router := &Router{NotFound: http.NotFoundHandler()}
	seen := map[string]string{}
	for _, r := range inv.Routes {
		if !want[r.Layer] {
			continue
		}
		inLayers[r.ID] = true
		c := compiled{route: r, method: r.HTTPMethod(), segments: split(r.Path)}
		key := c.method + " " + shape(c.segments)
		if other, dup := seen[key]; dup {
			// Two stream protocols on one GET path: the first keeps it.
			log.Printf("surface: %s shares %q with %s", r.ID, key, other)
			continue
		}
		seen[key] = r.ID
		if implemented[r.ID] {
			h, ok := handlers[r.ID]
			if !ok {
				return nil, fmt.Errorf("%s is listed as implemented but has no handler", r.ID)
			}
			c.handler = h
		} else {
			if _, ok := handlers[r.ID]; ok {
				return nil, fmt.Errorf("%s has a handler but is not listed as implemented", r.ID)
			}
			c.handler = unsupportedHandler(r)
		}
		router.routes = append(router.routes, c)
	}
	for id := range handlers {
		if !inLayers[id] {
			return nil, fmt.Errorf("handler for %s, which the specification does not define for %v", id, layers)
		}
	}
	sort.SliceStable(router.routes, func(i, j int) bool { return router.routes[i].route.ID < router.routes[j].route.ID })
	return router, nil
}

func split(path string) []segment {
	path = strings.Trim(path, "/")
	if path == "" {
		return nil
	}
	parts := strings.Split(path, "/")
	segments := make([]segment, len(parts))
	for i, p := range parts {
		if m := param.FindStringSubmatch(p); m != nil {
			segments[i] = segment{param: m[1]}
		} else {
			segments[i] = segment{literal: p}
		}
	}
	return segments
}

// shape is a route's path with parameter names erased, so that two routes
// that match exactly the same requests compare equal.
func shape(segments []segment) string {
	var b strings.Builder
	for _, s := range segments {
		b.WriteByte('/')
		if s.param != "" {
			b.WriteString(":")
		} else {
			b.WriteString(s.literal)
		}
	}
	return b.String()
}

// moreSpecific reports whether a beats b: a literal at the first position
// where one has a literal and the other a parameter.
func moreSpecific(a, b []segment) bool {
	for i := range a {
		if (a[i].param == "") != (b[i].param == "") {
			return a[i].param == ""
		}
	}
	return false
}

func (c compiled) match(parts []string) bool {
	if len(parts) != len(c.segments) {
		return false
	}
	for i, s := range c.segments {
		if s.param == "" && s.literal != parts[i] {
			return false
		}
		if s.param != "" && parts[i] == "" {
			return false
		}
	}
	return true
}

func pathParts(path string) []string {
	trimmed := strings.TrimPrefix(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// Route reports which specified route a request reaches, if any.
func (rt *Router) Route(method, path string) (Route, bool) {
	c := rt.find(method, pathParts(path))
	if c == nil {
		return Route{}, false
	}
	return c.route, true
}

func (rt *Router) find(method string, parts []string) *compiled {
	var best *compiled
	for i := range rt.routes {
		c := &rt.routes[i]
		if c.method != method || !c.match(parts) {
			continue
		}
		if best == nil || moreSpecific(c.segments, best.segments) {
			best = c
		}
	}
	return best
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := pathParts(r.URL.Path)
	c := rt.find(r.Method, parts)
	if c == nil {
		rt.NotFound.ServeHTTP(w, r)
		return
	}
	for i, part := range parts {
		if name := c.segments[i].param; name != "" {
			r.SetPathValue(name, part)
		}
	}
	c.handler.ServeHTTP(w, r)
}
