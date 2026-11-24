// Command load-baseline measures a Reality Engine under its loaded corpus and
// writes what it measured (RealityEngine_GO#2: "concurrent requests and
// dimension growth with explicit measured full-corpus load and failure
// behavior; publish baseline methodology/results, not invented performance
// thresholds").
//
//	go run ./cmd/load-baseline -re URL [-steps N] [-concurrency C] [-pid PID] [-out FILE]
//
// It speaks only SURFACE_SPEC routes, so it measures any runtime the same way;
// pointing it at a voter beside go-1 gives figures from the same host and
// corpus. Nothing here is a pass/fail threshold: it records numbers.
//
// Phases, in order:
//
//  1. corpus: GET /api/machines (count, time) and GET /api/config
//     (eventDimension).
//  2. serial steps: N POST /api/perceive calls with a deterministic sparse
//     space of eventDimension cells, one at a time; latency percentiles.
//  3. concurrent: C workers, each issuing N/C steps interleaved with
//     GET /api/machines; throughput, latency, and whether the committed step
//     numbers are distinct and contiguous (steps are serialized, not lost).
//  4. dimension growth: POST /api/machines a machine whose regions sit past
//     the space, read eventDimension again, then DELETE it.
//  5. failure behavior: a malformed body, a body over the 32 MiB bound, and an
//     unknown machine, each with the status it answered.
//
// With -pid, the process's resident set size (ps -o rss) is read before and
// after. The engine is reset (POST /api/engine/reset) before phase 2 and left
// reset at the end: do not point this at a universe whose state you keep.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type latencies []time.Duration

func (l latencies) summary() map[string]any {
	if len(l) == 0 {
		return map[string]any{"n": 0}
	}
	s := append(latencies(nil), l...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) float64 { return ms(s[min(len(s)-1, int(p*float64(len(s))))]) }
	var total time.Duration
	for _, d := range s {
		total += d
	}
	return map[string]any{
		"n": len(s), "meanMs": ms(total / time.Duration(len(s))), "p50Ms": at(0.50), "p90Ms": at(0.90),
		"p99Ms": at(0.99), "maxMs": ms(s[len(s)-1]),
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

type client struct {
	base string
	http *http.Client
}

func (c *client) do(method, path string, body []byte) (int, []byte, time.Duration, error) {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, time.Since(start), err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, time.Since(start), err
}

// space is a deterministic sparse Reality Event of width cells: about one
// cell in sixteen set, varying with the step.
func space(width, step int) []byte {
	var b strings.Builder
	b.WriteString(`{"vector":[`)
	x := uint32(2463534242 + step)
	for i := 0; i < width; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		if x%16 == 0 {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	b.WriteString(`],"includeMachineResults":false,"includePerceptualSpace":false}`)
	return []byte(b.String())
}

func rss(pid int) any {
	if pid <= 0 {
		return nil
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	kb, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return nil
	}
	return kb / 1024
}

func main() {
	base := flag.String("re", "", "the RE's base URL")
	steps := flag.Int("steps", 200, "steps per phase")
	workers := flag.Int("concurrency", 8, "concurrent workers")
	pid := flag.Int("pid", 0, "the RE's process id, for resident set size")
	out := flag.String("out", "", "write the report here as JSON (default stdout)")
	flag.Parse()
	if *base == "" || *steps <= 0 || *workers <= 0 {
		fmt.Fprintln(os.Stderr, "load-baseline: -re URL is required; -steps and -concurrency must be positive")
		os.Exit(2)
	}
	c := &client{base: strings.TrimSuffix(*base, "/"), http: &http.Client{Timeout: 5 * time.Minute}}
	report := map[string]any{"re": c.base, "startedAt": time.Now().UTC().Format(time.RFC3339), "rssMiBBefore": rss(*pid)}
	fail := func(what string, err error) {
		fmt.Fprintf(os.Stderr, "load-baseline: %s: %v\n", what, err)
		os.Exit(1)
	}

	// 1. corpus
	code, data, took, err := c.do("GET", "/api/machines", nil)
	if err != nil || code != 200 {
		fail("GET /api/machines", fmt.Errorf("%d %v", code, err))
	}
	var listing struct {
		Machines []any `json:"machines"`
	}
	_ = json.Unmarshal(data, &listing)
	dimension := func() int {
		_, data, _, _ := c.do("GET", "/api/config", nil)
		var cfg map[string]any
		_ = json.Unmarshal(data, &cfg)
		d, _ := cfg["eventDimension"].(float64)
		return int(d)
	}
	width := dimension()
	if width <= 0 {
		fail("GET /api/config", fmt.Errorf("no eventDimension"))
	}
	report["corpus"] = map[string]any{"machines": len(listing.Machines), "listMs": ms(took), "listBytes": len(data), "eventDimension": width}

	// 2. serial steps
	if code, _, _, err := c.do("POST", "/api/engine/reset", nil); err != nil || code != 200 {
		fail("POST /api/engine/reset", fmt.Errorf("%d %v", code, err))
	}
	var serial latencies
	errors := 0
	started := time.Now()
	for i := 0; i < *steps; i++ {
		code, _, d, err := c.do("POST", "/api/perceive", space(width, i))
		if err != nil || code != 200 {
			errors++
			continue
		}
		serial = append(serial, d)
	}
	elapsed := time.Since(started)
	report["serial"] = map[string]any{"latency": serial.summary(), "errors": errors,
		"stepsPerSecond": float64(len(serial)) / elapsed.Seconds()}

	// 3. concurrent
	var mu sync.Mutex
	var stepLat, listLat latencies
	stepNumbers := map[int]int{}
	cerrors := 0
	var wg sync.WaitGroup
	started = time.Now()
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := w; i < *steps; i += *workers {
				code, data, d, err := c.do("POST", "/api/perceive", space(width, *steps+i))
				var step struct {
					StepNumber *int `json:"stepNumber"`
				}
				_ = json.Unmarshal(data, &step)
				code2, _, d2, err2 := c.do("GET", "/api/machines", nil)
				mu.Lock()
				if err != nil || code != 200 || step.StepNumber == nil {
					cerrors++
				} else {
					stepLat = append(stepLat, d)
					stepNumbers[*step.StepNumber]++
				}
				if err2 != nil || code2 != 200 {
					cerrors++
				} else {
					listLat = append(listLat, d2)
				}
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	elapsed = time.Since(started)
	lo, hi, dup := -1, -1, 0
	for n, count := range stepNumbers {
		if lo < 0 || n < lo {
			lo = n
		}
		if n > hi {
			hi = n
		}
		dup += count - 1
	}
	report["concurrent"] = map[string]any{
		"workers": *workers, "steps": stepLat.summary(), "listMachines": listLat.summary(), "errors": cerrors,
		"stepsPerSecond": float64(len(stepLat)) / elapsed.Seconds(),
		"stepNumbers": map[string]any{"distinct": len(stepNumbers), "duplicates": dup, "first": lo, "last": hi,
			"contiguous": hi-lo+1 == len(stepNumbers) && dup == 0},
	}

	// 4. dimension growth
	past := width + 256
	machine := fmt.Sprintf(`{"name": "load-baseline growth probe %d", "perceptualMapping": {"input": {"offset": %d, "length": 4},
	  "output": {"offset": %d, "length": 4}}, "sequences": [{"id": "s", "events": [{"id": "a", "isInitial": true,
	  "elements": [{"value": 1}, {"value": 1}, {"value": 1}, {"value": 1}], "outputEvents": [{"id": "o", "vector": [1, 0, 0, 1]}]}]}]}`,
		time.Now().UnixNano(), past, past+4)
	code, data, took, err = c.do("POST", "/api/machines", []byte(machine))
	growth := map[string]any{"postStatus": code, "postMs": ms(took), "before": width}
	if err == nil && (code == 200 || code == 201) {
		var added struct {
			Machine struct {
				ID string `json:"id"`
			} `json:"machine"`
		}
		_ = json.Unmarshal(data, &added)
		growth["after"] = dimension()
		growth["expectedAtLeast"] = past + 8
		_, _, d, _ := c.do("POST", "/api/perceive", space(dimension(), 0))
		growth["stepAfterGrowthMs"] = ms(d)
		if added.Machine.ID != "" {
			code, _, _, _ := c.do("DELETE", "/api/machines/"+added.Machine.ID, nil)
			growth["deleteStatus"] = code
		}
	}
	report["dimensionGrowth"] = growth

	// 5. failure behavior
	failures := map[string]any{}
	code, _, took, _ = c.do("POST", "/api/perceive", []byte(`{"vector": [1, 0,`))
	failures["malformedPerceive"] = map[string]any{"status": code, "ms": ms(took)}
	big := append([]byte(`{"name": "x", "pad": "`), bytes.Repeat([]byte("a"), 33<<20)...)
	big = append(big, []byte(`"}`)...)
	code, _, took, _ = c.do("POST", "/api/machines", big)
	failures["oversizeMachine33MiB"] = map[string]any{"status": code, "ms": ms(took)}
	code, _, took, _ = c.do("POST", "/api/machines/no-such-machine/process", []byte(`{"inputEvent": [1]}`))
	failures["unknownMachineProcess"] = map[string]any{"status": code, "ms": ms(took)}
	code, _, _, err = c.do("GET", "/api/health", nil)
	failures["healthAfter"] = map[string]any{"status": code, "error": fmt.Sprint(err)}
	report["failureBehavior"] = failures

	_, _, _, _ = c.do("POST", "/api/engine/reset", nil)
	report["rssMiBAfter"] = rss(*pid)
	report["finishedAt"] = time.Now().UTC().Format(time.RFC3339)
	encoded, _ := json.MarshalIndent(report, "", "  ")
	encoded = append(encoded, '\n')
	if *out == "" {
		os.Stdout.Write(encoded)
		return
	}
	if err := os.WriteFile(*out, encoded, 0o644); err != nil {
		fail("write "+*out, err)
	}
}
