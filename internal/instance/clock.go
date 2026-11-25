// Package instance is a participant's identity and clock (RealityEngine_CI#296,
// #600; SURFACE_SPEC "The instance clock"): {instance, lamport, commit, step}.
//
// A UUID belongs to a participant -- an instance's RE (go-1) or its PE --
// never to an engine type or an image, and no two participants may share one.
// CI allocates it (INSTANCE_UUID for an RE, PE_INSTANCE_UUID for a PE); a
// participant launched without one mints a version 7 UUID at boot.
//
//   - lamport is the full Lamport clock: it ticks on every committed step,
//     every send and every receive, and a receive is max(local, received) + 1.
//   - commit counts committed steps: what lamport counted before #600.
//
// Neither ever resets, and lamport >= commit always holds. For an allocated
// participant both hold across restarts: INSTANCE_CLOCK_DIR (default
// ~/.reality-engine/clock/) keeps two high-water marks, <uuid>.lamport and
// <uuid>.commit, each reserved Reservation ticks ahead. A boot resumes from
// the marks and reserves the next blocks before issuing any tick; a tick past
// a persisted mark -- a receive that jumps past it included -- is always
// preceded by the write of the next mark (temp + rename), never followed by
// it. A directory written before #600 holds only <uuid>.lamport, which
// counted committed steps; the first boot under this rule seeds both marks.
//
// The participant holds an exclusive fcntl record lock on <uuid>.lock for its
// whole life, so a second live process of any engine type presenting the same
// UUID refuses to boot. It is a POSIX record lock, as C++, LSP and Scala take,
// because that is what `instance_uuids.py live` asks about with F_GETLK; flock
// would not be seen. The kernel releases it when the process exits.
//
// The error text and file names follow C++ instance_clock.cpp.
package instance

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Reservation is how far ahead of the issued value each mark is kept.
const Reservation = 1024

// MaxLamport is the largest lamport an RE-Clock header may carry: 2^53 - 1,
// the largest integer every runtime's JSON number represents exactly.
const MaxLamport = 9007199254740991

// HeaderKind is an inbound request's RE-Clock header, as the clock routes count it.
type HeaderKind int

const (
	HeaderReceived HeaderKind = iota
	HeaderMissing
	HeaderRejected
)

// Clock is a participant's identity and counters. It is safe for concurrent use.
type Clock struct {
	mu              sync.Mutex
	uuid            string
	lamport         uint64
	commit          uint64
	lamportReserved uint64
	commitReserved  uint64
	lamportFile     string // <uuid>.lamport; empty for a minted, unpersisted clock
	commitFile      string // <uuid>.commit
	received        uint64
	missing         uint64
	rejected        uint64
	lock            *os.File // held for the clock's life
}

// Boot is an RE's clock for this process, from INSTANCE_UUID and
// INSTANCE_CLOCK_DIR. It fails when an allocated participant cannot keep its
// clock or another live process holds its UUID: either way ticks could be
// issued twice.
func Boot() (*Clock, error) { return BootVar("INSTANCE_UUID") }

// BootVar is the clock allocated under variable (PE_INSTANCE_UUID for a PE).
func BootVar(variable string) (*Clock, error) {
	return bootFrom(os.Getenv(variable), Dir(), variable)
}

// BootFrom is Boot with its inputs given.
func BootFrom(allocated, dir string) (*Clock, error) {
	return bootFrom(allocated, dir, "INSTANCE_UUID")
}

func bootFrom(allocated, dir, variable string) (*Clock, error) {
	if !Canonical(allocated) {
		if allocated != "" {
			fmt.Fprintf(os.Stderr, "%s %q is not a canonical UUID; minting one\n", variable, allocated)
		}
		return Minted(), nil
	}
	uuid := strings.ToLower(allocated)
	return Persisted(uuid, filepath.Join(dir, uuid+".lamport"))
}

// Minted is an unpersisted clock under a fresh version 7 UUID.
func Minted() *Clock { return &Clock{uuid: NewV7()} }

// Persisted is an allocated clock whose lamport mark is file
// (<dir>/<uuid>.lamport) and commit mark the same path with .commit. The lock
// is taken before anything is read or written.
func Persisted(uuid, file string) (*Clock, error) {
	lock, err := lockInstance(file, uuid)
	if err != nil {
		return nil, err
	}
	commitFile := strings.TrimSuffix(file, ".lamport") + ".commit"
	lamportMark, err := ReadReservation(file)
	if err != nil {
		lock.Close()
		return nil, err
	}
	// Before #600 the one mark counted committed steps: it seeds both.
	commitMark := lamportMark
	if _, statErr := os.Stat(commitFile); statErr == nil {
		if commitMark, err = ReadReservation(commitFile); err != nil {
			lock.Close()
			return nil, err
		}
	}
	lamport := max(lamportMark, commitMark)
	c := &Clock{uuid: uuid, lamport: lamport, commit: commitMark, lamportFile: file, commitFile: commitFile, lock: lock}
	if err := c.reserveCommit(commitMark); err != nil {
		lock.Close()
		return nil, err
	}
	if err := c.reserveLamport(lamport); err != nil {
		lock.Close()
		return nil, err
	}
	return c, nil
}

// CommitStep is a committed step: commit and lamport tick together; it
// returns both.
func (c *Clock) CommitStep() (lamport, commit uint64, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	nextCommit, nextLamport := c.commit+1, c.lamport+1
	// Persist first, then issue: a mark is never behind what was handed out.
	if c.commitFile != "" && nextCommit > c.commitReserved {
		if err := c.reserveCommit(nextCommit); err != nil {
			return 0, 0, err
		}
	}
	if c.lamportFile != "" && nextLamport > c.lamportReserved {
		if err := c.reserveLamport(nextLamport); err != nil {
			return 0, 0, err
		}
	}
	c.commit, c.lamport = nextCommit, nextLamport
	return nextLamport, nextCommit, nil
}

// Send is a send -- a response or an outbound call: lamport ticks; it is returned.
func (c *Clock) Send() (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.advance(c.lamport + 1)
}

// Receive is a receive, applied before the request is handled:
// max(local, received) + 1, or local + 1 when no header was presented.
func (c *Clock) Receive(received uint64, presented bool) (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.lamport
	if presented && received > next {
		next = received
	}
	return c.advance(next + 1)
}

// advance moves lamport to next. A jump past the reserved mark is made durable
// before it is used: one jump can exceed the reservation, and adopting it
// first could reissue a tick after a crash. The caller holds mu.
func (c *Clock) advance(next uint64) (uint64, error) {
	if c.lamportFile != "" && next > c.lamportReserved {
		if err := c.reserveLamport(next); err != nil {
			return 0, err
		}
	}
	c.lamport = next
	return next, nil
}

// Event is the clock on a message this participant stores or forwards: a
// tick, as {instance, lamport} (SURFACE_SPEC "The instance clock", body fields).
func (c *Clock) Event() map[string]any {
	lamport, err := c.Send()
	if err != nil {
		return nil
	}
	return map[string]any{"instance": c.uuid, "lamport": lamport}
}

// Header is the RE-Clock header value for lamport.
func (c *Clock) Header(lamport uint64) string {
	return fmt.Sprintf("instance=%s; lamport=%d", c.uuid, lamport)
}

// CountHeader counts an inbound request's RE-Clock header.
func (c *Clock) CountHeader(kind HeaderKind) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch kind {
	case HeaderReceived:
		c.received++
	case HeaderMissing:
		c.missing++
	case HeaderRejected:
		c.rejected++
	}
}

// Headers is `headers` on a clock route: the inbound RE-Clock headers since boot.
func (c *Clock) Headers() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"received": c.received, "missing": c.missing, "rejected": c.rejected}
}

// Instance is the participant UUID.
func (c *Clock) Instance() string { return c.uuid }

// Lamport is the newest issued lamport.
func (c *Clock) Lamport() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lamport
}

// Commit is the number of committed steps.
func (c *Clock) Commit() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.commit
}

// Close releases the participant lock (tests; a process releases it on exit).
func (c *Clock) Close() error {
	if c.lock == nil {
		return nil
	}
	return c.lock.Close()
}

func (c *Clock) reserveLamport(through uint64) error {
	next := through + Reservation
	if c.lamportFile != "" {
		if err := writeReservation(c.lamportFile, next); err != nil {
			return err
		}
	}
	c.lamportReserved = next
	return nil
}

func (c *Clock) reserveCommit(through uint64) error {
	next := through + Reservation
	if c.commitFile != "" {
		if err := writeReservation(c.commitFile, next); err != nil {
			return err
		}
	}
	c.commitReserved = next
	return nil
}

// ParseHeader is an RE-Clock header value, parsed: the sender's lamport, or
// ok false when the value is malformed (SURFACE_SPEC "The header": a canonical
// lower-case UUID and an integer in [0, 2^53 - 1] with no sign or leading
// zeros, each once, in that order, separated by ";" with optional spaces).
func ParseHeader(value string) (uint64, bool) {
	parts := strings.Split(value, ";")
	if len(parts) != 2 {
		return 0, false
	}
	uuid, ok1 := strings.CutPrefix(strings.Trim(parts[0], " "), "instance=")
	digits, ok2 := strings.CutPrefix(strings.Trim(parts[1], " "), "lamport=")
	if !ok1 || !ok2 || !Canonical(uuid) || strings.ToLower(uuid) != uuid {
		return 0, false
	}
	if digits == "" || len(digits) > 16 || strings.TrimLeft(digits, "0123456789") != "" ||
		(len(digits) > 1 && digits[0] == '0') {
		return 0, false
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || n > MaxLamport {
		return 0, false
	}
	return n, true
}

// Canonical reports whether s is a UUID in 8-4-4-4-12 hex form.
func Canonical(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
				return false
			}
		}
	}
	return true
}

// ReadReservation is the mark in file; 0 when the instance has never run.
func ReadReservation(file string) (uint64, error) {
	data, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("Lamport clock file %s cannot be read", file)
	}
	text := strings.Join(strings.Fields(string(data)), "")
	bad := fmt.Errorf("Lamport clock file %s does not hold a whole number: %q", file, text)
	if text == "" || strings.TrimLeft(text, "0123456789") != "" {
		return 0, bad
	}
	v, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, bad
	}
	return v, nil
}

func writeReservation(file string, value uint64) error {
	tmp := file + "-tmp" // <uuid>.lamport-tmp, <uuid>.commit-tmp
	unwritable := fmt.Errorf("Lamport clock file %s cannot be written", tmp)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return unwritable
	}
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(value, 10)+"\n"), 0o644); err != nil {
		return unwritable
	}
	// Atomic: the old mark or the new one, never a torn one.
	if err := os.Rename(tmp, file); err != nil {
		return unwritable
	}
	return nil
}

func lockInstance(file, uuid string) (*os.File, error) {
	path := strings.TrimSuffix(file, ".lamport") + ".lock"
	unopenable := fmt.Errorf("Instance lock %s cannot be opened", path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, unopenable
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, unopenable
	}
	lock := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0} // Start 0, Len 0: the whole file
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &lock); err != nil {
		f.Close()
		return nil, fmt.Errorf("Instance %s is already live: another process holds %s. Two instances may not share a UUID (RealityEngine_CI#296).", uuid, path)
	}
	return f, nil
}

// Dir is INSTANCE_CLOCK_DIR, else ~/.reality-engine/clock.
func Dir() string {
	if dir := os.Getenv("INSTANCE_CLOCK_DIR"); dir != "" {
		return dir
	}
	home := os.Getenv("HOME")
	if home == "" {
		home = "."
	}
	return filepath.Join(home, ".reality-engine", "clock")
}

// NewV7 is a version 7 UUID (RFC 9562): 48 bits of Unix milliseconds, then
// random bits.
func NewV7() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("crypto/rand: %v", err))
	}
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
