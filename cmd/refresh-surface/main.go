// Command refresh-surface vendors the route tables of RealityEngine_CI's
// SURFACE_SPEC.md into internal/surface/spec-routes.json, and writes
// conformance/SURFACE_STATUS.md, or checks both.
//
// Local maintenance, never a hosted step: it reads a sibling checkout.
//
//	go run ./cmd/refresh-surface [-spec FILE] [-check]
//
// -spec defaults to ../RealityEngine_CI/SURFACE_SPEC.md. Without -check it
// refuses a spec with uncommitted changes. With -check it writes nothing and
// exits 1 when the vendored inventory or the status file is stale.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jateeter/RealityEngine_GO/internal/surface"
)

const (
	inventoryPath = "internal/surface/spec-routes.json"
	statusPath    = "conformance/SURFACE_STATUS.md"
)

func main() {
	spec := flag.String("spec", filepath.Join("..", "RealityEngine_CI", "SURFACE_SPEC.md"), "SURFACE_SPEC.md to read")
	check := flag.Bool("check", false, "compare with the vendored inventory; write nothing")
	flag.Parse()
	if err := run(*spec, *check); err != nil {
		fmt.Fprintln(os.Stderr, "refresh-surface:", err)
		os.Exit(1)
	}
}

func run(spec string, check bool) error {
	text, err := os.ReadFile(spec)
	if err != nil {
		return err
	}
	routes, err := surface.Parse(text)
	if err != nil {
		return err
	}
	digest, err := surface.Digest(routes)
	if err != nil {
		return err
	}
	dir := filepath.Dir(spec)
	if !check {
		if dirty, err := git(dir, "status", "--porcelain", "--", filepath.Base(spec)); err != nil {
			return err
		} else if dirty != "" {
			return fmt.Errorf("%s has uncommitted changes; refusing to vendor them", spec)
		}
	}

	if check {
		vendored, err := surface.Load()
		if err != nil {
			return err
		}
		if vendored.RoutesSha256 != digest {
			reportDrift(vendored.Routes, routes)
			return fmt.Errorf("surface inventory is stale against %s: regenerate with go run ./cmd/refresh-surface", spec)
		}
		statuses, err := surface.Statuses(vendored)
		if err != nil {
			return err
		}
		current, err := os.ReadFile(statusPath)
		if err != nil {
			return err
		}
		if string(current) != surface.StatusMarkdown(vendored, statuses) {
			return fmt.Errorf("%s is stale: regenerate with go run ./cmd/refresh-surface", statusPath)
		}
		fmt.Printf("surface inventory current: %v match %s\n", surface.Counts(routes), spec)
		return nil
	}

	commit, err := git(dir, "log", "-1", "--format=%H", "--", filepath.Base(spec))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(text)
	inv := surface.Inventory{
		Schema:  1,
		Purpose: surface.Purpose,
		Source: surface.Source{
			Repository: "jateeter/RealityEngine_CI",
			Path:       "SURFACE_SPEC.md",
			Commit:     commit,
			Sha256:     hex.EncodeToString(sum[:]),
		},
		Excluded:     surface.Excluded,
		Counts:       surface.Counts(routes),
		RoutesSha256: digest,
		Routes:       routes,
	}
	statuses, err := surface.Statuses(inv)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(inv); err != nil {
		return err
	}
	if err := os.WriteFile(inventoryPath, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(statusPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(statusPath, []byte(surface.StatusMarkdown(inv, statuses)), 0o644); err != nil {
		return err
	}
	fmt.Printf("vendored %d routes %v from %s@%s\n", len(routes), inv.Counts, spec, commit[:12])
	return nil
}

func reportDrift(old, new []surface.Route) {
	before, after := map[string]bool{}, map[string]bool{}
	for _, r := range old {
		before[r.ID] = true
	}
	for _, r := range new {
		after[r.ID] = true
	}
	var lines []string
	for id := range after {
		if !before[id] {
			lines = append(lines, "  added   "+id)
		}
	}
	for id := range before {
		if !after[id] {
			lines = append(lines, "  removed "+id)
		}
	}
	sort.Strings(lines)
	fmt.Fprintln(os.Stderr, strings.Join(lines, "\n"))
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	return strings.TrimSpace(string(out)), nil
}
