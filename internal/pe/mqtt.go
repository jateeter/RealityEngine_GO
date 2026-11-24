package pe

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The MQTT bridge (RealityEngine_GO#3, slice E): GET /api/mqtt/status,
// GET/PUT /api/mqtt/mappings, POST /api/mqtt/enable and /disable. C++
// MqttBridge, mqtt_client.cpp and mqtt_mapping.cpp are the concrete form;
// Rust's pe/mqtt prior art. The bridge subscribes to every filter its mapping
// registry declares and fans each PUBLISH out to every matching rule. A
// decoded message becomes a value for its sensor through the same path as
// POST /api/sensors/:id (a new sensor registered mqtt:<topic>), announced as
// mqtt-ingest. Push policy across a fan-out: any immediate rule pushes once,
// at once; otherwise the smallest debounced window schedules one push after
// the last message; manual never pushes. A push is the async push of POST
// /api/push, so it coalesces with one in flight.
//
// The client is a minimal MQTT v3.1.1 ingest client: CONNECT, SUBSCRIBE,
// receive PUBLISH (PUBACK for QoS 1), PINGREQ at half the keepalive, and
// reconnect after a delay whenever the connection is lost. No TLS: the broker
// is an accepted plaintext deficiency (owner, 2026-11-07).

// --- the mapping registry ------------------------------------------------

type mqttRule struct {
	id, topicFilter, sensorIDTemplate string
	offset, length                    int64
	extract                           string // raw, csv-float, json, single-float
	pointer                           string
	csvIndex                          *int64
	mode                              string // passthrough, minmax, linear, band
	min, max, scale, shift            float64
	clamp                             bool
	ttlMS, qos                        int64
	acceptRetained                    bool
	pushMode                          string // debounced, manual, immediate
	debounceMS                        int64
}

type mqttMetrics struct {
	received, mapped, rejected, stale int64
	lastMessageAtMS, lastErrorAtMS    int64
	lastError                         string
}

type mqttRegistry struct {
	rules   []*mqttRule
	metrics []*mqttMetrics
}

// leadingInt is atoi's reading: optional sign, digits, within int32.
func leadingInt(text string) (int64, bool) {
	text = strings.TrimLeft(text, " \t\n\v\f\r")
	start := 0
	if strings.HasPrefix(text, "+") || strings.HasPrefix(text, "-") {
		start = 1
	}
	end := start
	for end < len(text) && text[end] >= '0' && text[end] <= '9' {
		end++
	}
	if end == start {
		return 0, false
	}
	n, err := strconv.ParseInt(text[:end], 10, 64)
	if err != nil || n < math.MinInt32 || n > math.MaxInt32 {
		return 0, false
	}
	return n, true
}

// leadingNumber is strtod's reading: the longest prefix that is a number.
func leadingNumber(token string) (float64, bool) {
	token = strings.TrimLeft(token, " \t\n\v\f\r")
	for end := len(token); end > 0; end-- {
		prefix := token[:end]
		if strings.ContainsAny(prefix, "_xXpP") {
			continue
		}
		if v, err := strconv.ParseFloat(prefix, 64); err == nil {
			return v, true
		}
	}
	return 0, false
}

func stod(text string) float64 {
	if v, ok := leadingNumber(text); ok {
		return v
	}
	return math.NaN()
}

func oneOf(name string, allowed []string, def, what string) (string, error) {
	if name == "" && def != "" {
		return def, nil
	}
	for _, a := range allowed {
		if a == name {
			return a, nil
		}
	}
	return "", fmt.Errorf("unknown %s: %s", what, name)
}

func registryFrom(root map[string]any) (*mqttRegistry, error) {
	defaults, _ := root["defaults"].(map[string]any)
	ttl, qos, debounce := int64(30000), int64(0), int64(250)
	if f, ok := defaults["ttlMs"].(float64); ok {
		ttl = int64(f)
	}
	if f, ok := defaults["qos"].(float64); ok {
		qos = int64(int32(f))
	}
	if f, ok := defaults["debounceMs"].(float64); ok {
		debounce = int64(f)
	}
	retained := flag(defaults, "acceptRetained", true)
	pushes := "debounced"
	if name, ok := defaults["pushMode"].(string); ok {
		var err error
		if pushes, err = oneOf(name, []string{"debounced", "manual", "immediate"}, "debounced", "pushMode"); err != nil {
			return nil, err
		}
	}
	mappings, ok := root["mappings"].([]any)
	if !ok {
		return nil, fmt.Errorf("mqtt-mappings: missing top-level \"mappings\" array")
	}
	reg := &mqttRegistry{}
	for _, mv := range mappings {
		m, ok := mv.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mqtt-mappings: each mapping must be a JSON object")
		}
		r := &mqttRule{id: str(m["id"], ""), topicFilter: str(m["topicFilter"], ""), sensorIDTemplate: str(m["sensorIdTemplate"], "")}
		if r.id == "" || r.topicFilter == "" {
			return nil, fmt.Errorf("mqtt-mappings: id and topicFilter are required")
		}
		r.offset, r.length = 0, 1
		if region, ok := m["region"].(map[string]any); ok {
			r.offset = int64(int32(num(region["offset"], 0)))
			r.length = int64(max(int32(num(region["length"], 1)), 1))
		}
		r.extract = "csv-float"
		if extract, ok := m["extract"].(map[string]any); ok {
			var err error
			if r.extract, err = oneOf(str(extract["type"], "csv-float"), []string{"json", "csv-float", "raw", "single-float"}, "", "extract.type"); err != nil {
				return nil, err
			}
			r.pointer = str(extract["pointer"], "")
			if f, ok := extract["index"].(float64); ok {
				i := int64(int32(f))
				r.csvIndex = &i
			}
		}
		r.mode, r.min, r.max, r.scale, r.shift = "passthrough", 0, 1, 1, 0
		norm, hasNorm := m["normalize"].(map[string]any)
		if hasNorm {
			mode := str(norm["mode"], "passthrough")
			if mode == "" {
				mode = "passthrough"
			}
			var err error
			if r.mode, err = oneOf(mode, []string{"minmax", "linear", "passthrough", "band"}, "", "normalize.mode"); err != nil {
				return nil, err
			}
			r.min, r.max = num(norm["min"], 0), num(norm["max"], 1)
			r.scale, r.shift = num(norm["scale"], 1), num(norm["offset"], 0)
		}
		r.clamp = flag(norm, "clamp", true)
		r.ttlMS, r.qos, r.debounceMS = ttl, qos, debounce
		if f, ok := m["ttlMs"].(float64); ok {
			r.ttlMS = int64(f)
		}
		if f, ok := m["qos"].(float64); ok {
			r.qos = int64(int32(f))
		}
		if f, ok := m["debounceMs"].(float64); ok {
			r.debounceMS = int64(f)
		}
		r.acceptRetained = flag(m, "acceptRetained", retained)
		r.pushMode = pushes
		if name, ok := m["pushMode"].(string); ok {
			var err error
			if r.pushMode, err = oneOf(name, []string{"debounced", "manual", "immediate"}, "debounced", "pushMode"); err != nil {
				return nil, err
			}
		}
		reg.rules = append(reg.rules, r)
		reg.metrics = append(reg.metrics, &mqttMetrics{})
	}
	return reg, nil
}

// registryFromLegacy reads MQTT_TOPIC_MAP: [{topic, sensorId, offset, length, codec}].
func registryFromLegacy(legacy any) (*mqttRegistry, error) {
	items, _ := legacy.([]any)
	mappings := []any{}
	counter := 0
	for _, iv := range items {
		item, ok := iv.(map[string]any)
		if !ok {
			continue
		}
		id := str(item["sensorId"], fmt.Sprintf("mqtt-legacy-%d", counter))
		counter++
		mappings = append(mappings, map[string]any{
			"id": id, "topicFilter": str(item["topic"], ""), "sensorIdTemplate": str(item["sensorId"], ""),
			"region":  map[string]any{"offset": num(item["offset"], 0), "length": num(item["length"], 1)},
			"extract": map[string]any{"type": str(item["codec"], "csv-float")}, "normalize": map[string]any{"mode": "passthrough"},
		})
	}
	return registryFrom(map[string]any{"mappings": mappings})
}

func matchFilter(topic, filter []string) ([]string, bool) {
	var captures []string
	at := 0
	for i, level := range filter {
		if level == "#" {
			rest := ""
			if at <= len(topic) {
				rest = strings.Join(topic[at:], "/")
			}
			return append(captures, rest), i+1 == len(filter)
		}
		if at >= len(topic) {
			return nil, false
		}
		if level == "+" {
			captures = append(captures, topic[at])
		} else if level != topic[at] {
			return nil, false
		}
		at++
	}
	return captures, at == len(topic)
}

type ruleMatch struct {
	index    int
	captures []string
}

func (r *mqttRegistry) matchAll(topic string) []ruleMatch {
	levels := strings.Split(topic, "/")
	var out []ruleMatch
	for i, rule := range r.rules {
		if captures, ok := matchFilter(levels, strings.Split(rule.topicFilter, "/")); ok {
			out = append(out, ruleMatch{i, captures})
		}
	}
	return out
}

func resolveSensorID(rule *mqttRule, topic string, captures []string) string {
	if rule.sensorIDTemplate == "" {
		return topic
	}
	var b strings.Builder
	rest := rule.sensorIDTemplate
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			break
		}
		close := strings.IndexByte(rest[open+1:], '}')
		if close < 0 {
			break
		}
		close += open + 1
		b.WriteString(rest[:open])
		if i, ok := leadingInt(rest[open+1 : close]); ok && i >= 1 && int(i) <= len(captures) {
			b.WriteString(captures[i-1])
		}
		rest = rest[close+1:]
	}
	b.WriteString(rest)
	return b.String()
}

func navigate(root any, p string) (any, bool) {
	if p == "" || p == "/" {
		return root, true
	}
	if !strings.HasPrefix(p, "/") {
		return nil, false
	}
	cursor := root
	for _, token := range strings.Split(p[1:], "/") {
		switch c := cursor.(type) {
		case map[string]any:
			v, ok := c[token]
			if !ok {
				return nil, false
			}
			cursor = v
		case []any:
			i, ok := leadingInt(token)
			if !ok || i < 0 || int(i) >= len(c) {
				return nil, false
			}
			cursor = c[i]
		default:
			return nil, false
		}
	}
	return cursor, true
}

func (rule *mqttRule) decode(payload []byte) ([]float64, error) {
	text := string(payload)
	var raw []float64
	switch rule.extract {
	case "raw", "single-float":
		raw = []float64{stod(text)}
	case "csv-float":
		var all []float64
		for _, tok := range strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune(", \n\r\t", r) }) {
			all = append(all, stod(tok))
		}
		raw = all
		if rule.csvIndex != nil {
			i := *rule.csvIndex
			if i < 0 || int(i) >= len(all) {
				return nil, fmt.Errorf("csv index %d out of range (have %d)", i, len(all))
			}
			raw = []float64{all[i]}
		}
	case "json":
		var parsed any
		if err := json.Unmarshal(payload, &parsed); err != nil {
			return nil, fmt.Errorf("json parse: %v", err)
		}
		node, ok := navigate(parsed, rule.pointer)
		if !ok {
			return nil, fmt.Errorf("json pointer %q not found", rule.pointer)
		}
		switch n := node.(type) {
		case float64:
			raw = []float64{n}
		case []any:
			for _, x := range n {
				if f, ok := x.(float64); ok {
					raw = append(raw, f)
				} else {
					raw = append(raw, math.NaN())
				}
			}
		case string:
			raw = []float64{stod(n)}
		case bool:
			raw = []float64{boolf(n)}
		default:
			return nil, fmt.Errorf("json pointer target is not a number / array / string / bool")
		}
	}
	values := make([]float64, 0, len(raw))
	for _, v := range raw {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("value is not finite")
		}
		n := v
		switch rule.mode {
		case "minmax":
			span := rule.max - rule.min
			if span == 0 {
				return nil, fmt.Errorf("normalize.min == normalize.max")
			}
			n = (v - rule.min) / span
		case "linear":
			n = v*rule.scale + rule.shift
		case "band":
			n = 0
			if v >= rule.min && v <= rule.max {
				n = 1
			}
		}
		if rule.clamp {
			n = clamp01(n)
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, fmt.Errorf("value not finite after normalize")
		}
		values = append(values, n)
	}
	if int64(len(values)) != rule.length {
		return nil, fmt.Errorf("transformed value count %d != region.length %d", len(values), rule.length)
	}
	return values, nil
}

func boolf(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (r *mqttRegistry) validateOverlaps(allow bool) []string {
	warnings := []string{}
	if allow {
		return warnings
	}
	for i, a := range r.rules {
		for _, b := range r.rules[i+1:] {
			if a.offset < b.offset+b.length && b.offset < a.offset+a.length {
				warnings = append(warnings, fmt.Sprintf("mappings %q [%d,%d) and %q [%d,%d) overlap",
					a.id, a.offset, a.offset+a.length, b.id, b.offset, b.offset+b.length))
			}
		}
	}
	return warnings
}

func (r *mqttRegistry) json() map[string]any {
	out := []any{}
	for i, rule := range r.rules {
		m := r.metrics[i]
		extract := map[string]any{"type": rule.extract}
		if rule.pointer != "" {
			extract["pointer"] = rule.pointer
		}
		if rule.csvIndex != nil {
			extract["index"] = *rule.csvIndex
		}
		out = append(out, map[string]any{
			"id": rule.id, "topicFilter": rule.topicFilter, "sensorIdTemplate": rule.sensorIDTemplate,
			"region": map[string]any{"offset": rule.offset, "length": rule.length}, "extract": extract,
			"normalize": map[string]any{"mode": rule.mode, "min": rule.min, "max": rule.max, "scale": rule.scale, "offset": rule.shift, "clamp": rule.clamp},
			"ttlMs":     rule.ttlMS, "qos": rule.qos, "acceptRetained": rule.acceptRetained,
			"pushMode": rule.pushMode, "debounceMs": rule.debounceMS,
			"counters": map[string]any{
				"received": m.received, "mapped": m.mapped, "rejected": m.rejected, "stale": m.stale,
				"lastMessageAtMs": m.lastMessageAtMS, "lastError": m.lastError, "lastErrorAtMs": m.lastErrorAtMS,
			},
		})
	}
	return map[string]any{"mappings": out}
}

// --- the client ----------------------------------------------------------

type mqttConfig struct {
	host               string
	port               int64
	clientID           string
	username, password *string
	keepaliveSec       int64
	reconnectDelay     time.Duration
}

func defaultClientID() string {
	if id := os.Getenv("MQTT_CLIENT_ID"); id != "" {
		return id
	}
	return "reality-engine-pe-go"
}

func defaultMQTTConfig() mqttConfig {
	return mqttConfig{host: "localhost", port: 1883, clientID: defaultClientID(), keepaliveSec: 60, reconnectDelay: 2 * time.Second}
}

type clientStats struct {
	connectAttempts, connectSuccesses, messagesReceived, bytesReceived, pingsSent, reconnects int64
	lastMessageAtMS                                                                           int64
}

func putString(b []byte, s string) []byte {
	if len(s) > math.MaxUint16 {
		s = s[:math.MaxUint16]
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

func mqttPacket(first byte, body []byte) []byte {
	out := []byte{first}
	n := len(body)
	for {
		b := byte(n & 0x7F)
		n >>= 7
		if n > 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			break
		}
	}
	return append(out, body...)
}

func remainingLength(r *bufio.Reader) (int, error) {
	length, mult := 0, 1
	for i := 0; i < 4; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		length += int(b&0x7F) * mult
		if b&0x80 == 0 {
			return length, nil
		}
		mult *= 128
	}
	return 0, fmt.Errorf("malformed remaining length")
}

// --- the bridge ----------------------------------------------------------

type bridgeStats struct {
	mapped, rejected, unmatched, received, pushes int64
}

type mqttBridge struct {
	pe         *PE
	config     mqttConfig
	filters    [][2]any // (filter, qos)
	mu         sync.Mutex
	registry   *mqttRegistry
	client     clientStats
	stats      bridgeStats
	connected  bool
	nextPushAt int64
	stop       chan struct{}
	stopOnce   sync.Once
	wake       chan struct{}
}

func newBridge(p *PE, config mqttConfig, reg *mqttRegistry) *mqttBridge {
	qos := map[string]int64{}
	for _, r := range reg.rules {
		if q, ok := qos[r.topicFilter]; !ok || r.qos > q {
			qos[r.topicFilter] = r.qos
		}
	}
	filters := make([]string, 0, len(qos))
	for f := range qos {
		filters = append(filters, f)
	}
	sort.Strings(filters)
	b := &mqttBridge{pe: p, config: config, registry: reg, stop: make(chan struct{}), wake: make(chan struct{}, 1)}
	for _, f := range filters {
		b.filters = append(b.filters, [2]any{f, min(max(qos[f], 0), 1)})
	}
	return b
}

func (b *mqttBridge) start() {
	go b.run()
	go b.debounceLoop()
}

func (b *mqttBridge) halt() { b.stopOnce.Do(func() { close(b.stop) }) }

func (b *mqttBridge) stopped() bool {
	select {
	case <-b.stop:
		return true
	default:
		return false
	}
}

func (b *mqttBridge) count(f func()) { b.mu.Lock(); f(); b.mu.Unlock() }

func (b *mqttBridge) connect() (net.Conn, *bufio.Reader, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(b.config.host, strconv.FormatInt(b.config.port, 10)), 10*time.Second)
	if err != nil {
		return nil, nil, err
	}
	body := putString(nil, "MQTT")
	body = append(body, 0x04)
	flags := byte(0x02)
	if b.config.username != nil {
		flags |= 0x80
	}
	if b.config.password != nil {
		flags |= 0x40
	}
	body = append(body, flags)
	body = binary.BigEndian.AppendUint16(body, uint16(b.config.keepaliveSec))
	body = putString(body, b.config.clientID)
	if b.config.username != nil {
		body = putString(body, *b.config.username)
	}
	if b.config.password != nil {
		body = putString(body, *b.config.password)
	}
	if _, err := conn.Write(mqttPacket(0x10, body)); err != nil {
		conn.Close()
		return nil, nil, err
	}
	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	first, err := r.ReadByte()
	if err == nil && first&0xF0 != 0x20 {
		err = fmt.Errorf("no CONNACK")
	}
	var n int
	if err == nil {
		if n, err = remainingLength(r); err == nil && n != 2 {
			err = fmt.Errorf("no CONNACK")
		}
	}
	var ack [2]byte
	if err == nil {
		_, err = io.ReadFull(r, ack[:])
	}
	if err == nil && ack[1] != 0 {
		err = fmt.Errorf("connection refused")
	}
	if err != nil {
		conn.Close()
		return nil, nil, err
	}
	return conn, r, nil
}

func (b *mqttBridge) subscribe(conn net.Conn) error {
	if len(b.filters) == 0 {
		return nil
	}
	body := binary.BigEndian.AppendUint16(nil, 1)
	for _, f := range b.filters {
		body = putString(body, f[0].(string))
		body = append(body, byte(f[1].(int64)))
	}
	_, err := conn.Write(mqttPacket(0x82, body))
	return err
}

func (b *mqttBridge) run() {
	for !b.stopped() {
		b.count(func() { b.client.connectAttempts++ })
		conn, r, err := b.connect()
		if err != nil {
			select {
			case <-b.stop:
				return
			case <-time.After(b.config.reconnectDelay):
			}
			continue
		}
		b.count(func() { b.client.connectSuccesses++; b.connected = true })
		if b.subscribe(conn) == nil {
			b.readLoop(conn, r)
		}
		b.count(func() { b.connected = false })
		if b.stopped() {
			_, _ = conn.Write([]byte{0xE0, 0})
			conn.Close()
			return
		}
		conn.Close()
		b.count(func() { b.client.reconnects++ })
	}
}

func (b *mqttBridge) readLoop(conn net.Conn, r *bufio.Reader) {
	lastTraffic := time.Now()
	half := time.Duration(max(b.config.keepaliveSec, 0)*1000/2) * time.Millisecond
	for !b.stopped() {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		first, err := r.ReadByte()
		if b.config.keepaliveSec > 0 && time.Since(lastTraffic) >= half {
			if _, werr := conn.Write([]byte{0xC0, 0}); werr == nil {
				b.count(func() { b.client.pingsSent++ })
			}
			lastTraffic = time.Now()
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := remainingLength(r)
		if err != nil {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		lastTraffic = time.Now()
		if first&0xF0 != 0x30 || len(body) < 2 {
			continue
		}
		topicLen := int(binary.BigEndian.Uint16(body))
		if len(body) < 2+topicLen {
			continue
		}
		topic := string(body[2 : 2+topicLen])
		start := 2 + topicLen
		if (first>>1)&0x03 > 0 && len(body) >= start+2 {
			_, _ = conn.Write(mqttPacket(0x40, body[start:start+2]))
			start += 2
		}
		payload := body[start:]
		b.count(func() {
			b.client.messagesReceived++
			b.client.bytesReceived += int64(len(payload))
			b.client.lastMessageAtMS = nowMS()
		})
		b.onMessage(topic, payload)
	}
}

type delivery struct {
	sensorID       string
	offset, length int64
	values         []float64
	ttlMS          int64
	mappingID      string
}

func (b *mqttBridge) onMessage(topic string, payload []byte) {
	immediate := false
	debounce := int64(-1)
	var delivered []delivery
	b.mu.Lock()
	b.stats.received++
	matches := b.registry.matchAll(topic)
	if len(matches) == 0 {
		b.stats.unmatched++
		b.mu.Unlock()
		return
	}
	for _, m := range matches {
		metrics, rule := b.registry.metrics[m.index], b.registry.rules[m.index]
		metrics.received++
		metrics.lastMessageAtMS = nowMS()
		values, err := rule.decode(payload)
		if err != nil {
			metrics.lastError, metrics.lastErrorAtMS = "["+topic+"] "+err.Error(), nowMS()
			metrics.rejected++
			b.stats.rejected++
			continue
		}
		delivered = append(delivered, delivery{resolveSensorID(rule, topic, m.captures), rule.offset, rule.length, values, rule.ttlMS, rule.id})
		switch rule.pushMode {
		case "immediate":
			immediate = true
		case "debounced":
			if debounce < 0 || rule.debounceMS < debounce {
				debounce = rule.debounceMS
			}
		}
		metrics.mapped++
		b.stats.mapped++
	}
	b.mu.Unlock()
	for _, d := range delivered {
		b.pe.feedMQTT(d, topic)
	}
	if immediate {
		go b.pe.Push(false, true, nil)
		b.count(func() { b.stats.pushes++ })
	} else if debounce >= 0 {
		b.schedulePush(debounce)
	}
}

func (b *mqttBridge) schedulePush(window int64) {
	b.mu.Lock()
	if target := nowMS() + window; target > b.nextPushAt {
		b.nextPushAt = target
	}
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *mqttBridge) debounceLoop() {
	for !b.stopped() {
		b.mu.Lock()
		target := b.nextPushAt
		b.mu.Unlock()
		wait := int64(200)
		if target != 0 {
			wait = target - nowMS()
		}
		if target == 0 || wait > 0 {
			select {
			case <-b.stop:
				return
			case <-b.wake:
			case <-time.After(time.Duration(max(wait, 0)) * time.Millisecond):
			}
			continue
		}
		b.mu.Lock()
		if b.nextPushAt > target {
			b.mu.Unlock()
			continue
		}
		b.nextPushAt = 0
		b.stats.pushes++
		b.mu.Unlock()
		go b.pe.Push(false, true, nil)
	}
}

func (b *mqttBridge) status() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.client
	return map[string]any{
		"enabled": true, "connected": b.connected, "brokerHost": b.config.host, "brokerPort": b.config.port,
		"clientId": b.config.clientID, "connectAttempts": c.connectAttempts, "connectSuccesses": c.connectSuccesses,
		"messagesReceived": c.messagesReceived, "bytesReceived": c.bytesReceived, "pingsSent": c.pingsSent,
		"reconnects": c.reconnects, "lastMessageAtMs": c.lastMessageAtMS,
		"bridge": map[string]any{
			"messagesMapped": b.stats.mapped, "messagesRejected": b.stats.rejected,
			"messagesUnmatched": b.stats.unmatched, "pushesTriggered": b.stats.pushes,
		},
	}
}

// feedMQTT delivers a decoded message to its sensor, registering mqtt:<topic>
// when no source has the sensorId, and announces it.
func (p *PE) feedMQTT(d delivery, topic string) {
	if len(d.values) == 0 || int64(len(d.values)) != d.length {
		return
	}
	p.mu.Lock()
	var source *Source
	if p.updateSensor(d.sensorID, d.values) {
		source = p.sensorByID(d.sensorID)
	} else {
		ttl := d.ttlMS
		if ttl <= 0 {
			ttl = 30000
		}
		at := nowMS()
		source = p.addSource(&Source{Name: "mqtt:" + topic, Region: Region{d.offset, d.length}, sensor: &sensorDetail{
			sensorID: d.sensorID, lastValue: d.values, lastUpdated: &at, ttlMS: ttl, origin: "mqtt",
		}})
	}
	var sj any
	if source != nil {
		sj = source.json(nowMS())
	}
	p.mu.Unlock()
	p.broadcast(map[string]any{
		"type": "mqtt-ingest", "topic": topic, "mappingId": d.mappingID, "sensorId": d.sensorID,
		"source": sj, "timestamp": nowMS(),
	})
}

// --- configuration and routes -------------------------------------------

const defaultBrokerHost = "yuma.lateraledge.cloud"

func envNonEmpty(name string) (string, bool) {
	v := os.Getenv(name)
	return v, v != ""
}

func mqttTruthy(v string, set bool) bool {
	if !set {
		return false
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

func hostAndPort(url string, def int64) (string, int64) {
	authority := url
	if i := strings.Index(url, "://"); i >= 0 {
		authority = url[i+3:]
	}
	if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
		port, ok := leadingInt(authority[colon+1:])
		if !ok {
			port = def
		}
		return authority[:colon], port
	}
	return authority, def
}

// mqttFromEnvironment is the bridge MQTT_* configure at boot, or nothing.
func mqttFromEnvironment() (mqttConfig, *mqttRegistry, bool) {
	cfg := defaultMQTTConfig()
	if mqttTruthy(envNonEmpty("MQTT_DISABLED")) {
		return cfg, nil, false
	}
	host, urlPort := defaultBrokerHost, int64(1883)
	if v, ok := os.LookupEnv("MQTT_BROKER_HOST"); ok {
		if v == "" {
			return cfg, nil, false
		}
		host = v
	} else if url, ok := envNonEmpty("MQTT_BROKER_URL"); ok {
		host, urlPort = hostAndPort(url, 1883)
	}
	cfg.host, cfg.port = host, urlPort
	if v, ok := envNonEmpty("MQTT_BROKER_PORT"); ok {
		n, _ := leadingInt(v)
		cfg.port = n
	}
	if v, ok := envNonEmpty("MQTT_USERNAME"); ok {
		cfg.username = &v
	}
	if v, ok := envNonEmpty("MQTT_PASSWORD"); ok {
		cfg.password = &v
	}
	if v, ok := envNonEmpty("MQTT_KEEPALIVE"); ok {
		n, _ := leadingInt(v)
		cfg.keepaliveSec = n
	}
	var reg *mqttRegistry
	if path, ok := envNonEmpty("MQTT_MAPPINGS_FILE"); ok {
		data, err := os.ReadFile(path)
		var root map[string]any
		if err == nil {
			err = json.Unmarshal(data, &root)
		}
		if err == nil {
			reg, err = registryFrom(root)
		}
		if err != nil {
			log.Printf("MQTT mappings file error (%s): %v — trying JSON fallback", path, err)
			reg = nil
		}
	}
	if reg == nil {
		if raw, ok := envNonEmpty("MQTT_MAPPINGS_JSON"); ok {
			var root map[string]any
			err := json.Unmarshal([]byte(raw), &root)
			if err == nil {
				reg, err = registryFrom(root)
			}
			if err != nil {
				log.Printf("MQTT mappings JSON error: %v — bridge disabled", err)
				return cfg, nil, false
			}
		} else if raw, ok := envNonEmpty("MQTT_TOPIC_MAP"); ok {
			var root any
			err := json.Unmarshal([]byte(raw), &root)
			if err == nil {
				reg, err = registryFromLegacy(root)
			}
			if err != nil {
				log.Printf("MQTT legacy topic map error: %v — bridge disabled", err)
				return cfg, nil, false
			}
		}
	}
	if reg == nil || len(reg.rules) == 0 {
		log.Printf("MQTT broker %s configured but no mappings resolved — bridge disabled", host)
		return cfg, nil, false
	}
	for _, w := range reg.validateOverlaps(mqttTruthy(envNonEmpty("MQTT_ALLOW_REGION_OVERLAP"))) {
		log.Printf("MQTT mapping warning: %s", w)
	}
	return cfg, reg, true
}

func (p *PE) startMQTTFromEnvironment() {
	cfg, reg, ok := mqttFromEnvironment()
	if !ok {
		return
	}
	log.Printf("MQTT bridge enabled — broker=%s:%d, mappings=%d", cfg.host, cfg.port, len(reg.rules))
	b := newBridge(p, cfg, reg)
	p.mqttMu.Lock()
	p.mqtt = b
	p.mqttMu.Unlock()
	b.start()
}

func overlapAllowed() bool {
	v := os.Getenv("MQTT_ALLOW_REGION_OVERLAP")
	return v == "1" || v == "true"
}

func mqttRegistryFromBody(root any) (*mqttRegistry, error) {
	m, _ := root.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	reg, err := registryFrom(m)
	if err != nil {
		return nil, &Error{400, "schema: " + err.Error()}
	}
	if len(reg.rules) == 0 {
		return nil, &Error{400, "mappings array is empty — at least one rule is required"}
	}
	return reg, nil
}

func (p *PE) installBridge(cfg mqttConfig, reg *mqttRegistry) map[string]any {
	warnings := reg.validateOverlaps(overlapAllowed())
	b := newBridge(p, cfg, reg)
	p.mqttMu.Lock()
	if p.mqtt != nil {
		p.mqtt.halt()
	}
	p.mqtt = b
	p.mqttMu.Unlock()
	b.start()
	return map[string]any{"success": true, "enabled": true, "mappings": len(reg.rules), "warnings": warnings}
}

func (p *PE) bridge() *mqttBridge {
	p.mqttMu.Lock()
	defer p.mqttMu.Unlock()
	return p.mqtt
}

// MQTTStatus is GET /api/mqtt/status.
func (p *PE) MQTTStatus() map[string]any {
	if b := p.bridge(); b != nil {
		return b.status()
	}
	return map[string]any{"enabled": false}
}

// MQTTMappings is GET /api/mqtt/mappings.
func (p *PE) MQTTMappings() map[string]any {
	b := p.bridge()
	if b == nil {
		return map[string]any{"enabled": false, "mappings": []any{}}
	}
	b.mu.Lock()
	out := b.registry.json()
	b.mu.Unlock()
	out["enabled"] = true
	return out
}

// ReplaceMQTTMappings is PUT /api/mqtt/mappings: a new registry on the same
// broker settings; 409 without a bridge.
func (p *PE) ReplaceMQTTMappings(body map[string]any) (map[string]any, error) {
	current := p.bridge()
	if current == nil {
		return nil, &Error{409, "no broker config — set MQTT_BROKER_HOST at PE startup before reloading mappings"}
	}
	reg, err := mqttRegistryFromBody(body)
	if err != nil {
		return nil, err
	}
	out := p.installBridge(current.config, reg)
	p.broadcast(map[string]any{"type": "mqtt-mappings-reloaded", "mappings": out["mappings"], "timestamp": nowMS()})
	return out, nil
}

// EnableMQTT is POST /api/mqtt/enable: a bridge from the request.
func (p *PE) EnableMQTT(body map[string]any) (map[string]any, error) {
	url := str(body["brokerUrl"], "")
	if url == "" {
		return nil, &Error{400, "brokerUrl is required"}
	}
	host, port := hostAndPort(url, 1883)
	reg, err := mqttRegistryFromBody(body["mappings"])
	if err != nil {
		return nil, err
	}
	cfg := defaultMQTTConfig()
	if b := p.bridge(); b != nil {
		cfg.clientID = b.config.clientID
	}
	cfg.host, cfg.port = host, port
	if v := str(body["username"], ""); v != "" {
		cfg.username = &v
	}
	if v := str(body["password"], ""); v != "" {
		cfg.password = &v
	}
	out := p.installBridge(cfg, reg)
	log.Printf("MQTT bridge enabled via API — broker=%s:%d", host, port)
	return out, nil
}

// DisableMQTT is POST /api/mqtt/disable.
func (p *PE) DisableMQTT() map[string]any {
	p.mqttMu.Lock()
	if p.mqtt != nil {
		p.mqtt.halt()
		p.mqtt = nil
		log.Printf("MQTT bridge disabled via API")
	}
	p.mqttMu.Unlock()
	return map[string]any{"success": true, "enabled": false}
}
