package machine

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Loaded is one corpus file's machine.
type Loaded struct {
	Machine *Machine
	// RelFile is the file's path relative to the machines directory, with
	// forward slashes (GET /api/machines/json/list reports it).
	RelFile string
}

// LoadError names a corpus file that did not load, and why.
type LoadError struct {
	RelFile string
	Err     error
}

func (e LoadError) Error() string { return e.RelFile + ": " + e.Err.Error() }

// LoadDir loads every .json file under dir, recursively, including
// machines/domains/<name>/ (corpus addressing is path-aware on every engine).
// Files are read in path order. A machine loaded at boot takes the id
// machine-<stem> whatever it declares (owner, 2026-10-18), and SourceFile is
// its file name. A file that fails is reported and skipped; two files whose
// stems give the same id are an error for the second (corpus file names are
// globally unique, RealityEngine_Machines enforces it).
func LoadDir(dir string, nowMS int64) ([]Loaded, []LoadError, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".json") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("read machines directory %s: %w", dir, err)
	}
	sort.Strings(paths)

	var loaded []Loaded
	var failed []LoadError
	byID := map[string]string{}
	for _, path := range paths {
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		data, err := os.ReadFile(path)
		if err != nil {
			failed = append(failed, LoadError{rel, err})
			continue
		}
		m, err := Parse(data)
		if err != nil {
			failed = append(failed, LoadError{rel, err})
			continue
		}
		base := filepath.Base(path)
		m.ID = CanonicalID(strings.TrimSuffix(base, ".json"))
		m.SourceFile = base
		m.LoadedAtMS = nowMS
		if other, dup := byID[m.ID]; dup {
			failed = append(failed, LoadError{rel, fmt.Errorf("id %s is already held by %s", m.ID, other)})
			continue
		}
		byID[m.ID] = rel
		loaded = append(loaded, Loaded{Machine: m, RelFile: rel})
	}
	return loaded, failed, nil
}
