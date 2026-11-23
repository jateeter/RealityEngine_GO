// Package instance is the instance's identity and Lamport clock
// (RealityEngine_CI#296; SURFACE_SPEC "The instance clock"):
// {instance, lamport, step}.
//
// A UUID belongs to an instance (go-1), never to an engine type or an image,
// and no two instances of any engine type may share one. The instance
// registry allocates it and passes INSTANCE_UUID; an instance launched without
// one mints a version 7 UUID at boot.
//
// lamport ticks once per committed step and is never reset. For an allocated
// instance that holds across restarts too: INSTANCE_CLOCK_DIR (default
// ~/.reality-engine/clock/) keeps <uuid>.lamport, a high-water mark reserved
// Reservation ticks ahead. A boot resumes from the mark and reserves the next
// block before issuing any tick, and a tick past the mark is always preceded
// by the write of the next block (temp + rename), never followed by it.
//
// The instance holds an exclusive fcntl record lock on <uuid>.lock for its
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

// Reservation is how far ahead of the issued value the mark is kept.
const Reservation = 1024

// Clock is the instance's identity and Lamport counter. It is safe for
// concurrent use.
type Clock struct {
	mu       sync.Mutex
	uuid     string
	lamport  uint64
	reserved uint64
	file     string   // <uuid>.lamport; empty for a minted, unpersisted clock
	lock     *os.File // held for the clock's life
}

// Boot is the clock for this process, from INSTANCE_UUID and
// INSTANCE_CLOCK_DIR. It fails when an allocated instance cannot keep its
// clock or another live process holds its UUID: either way ticks could be
// issued twice.
func Boot() (*Clock, error) { return BootFrom(os.Getenv("INSTANCE_UUID"), Dir()) }

// BootFrom is Boot with its inputs given.
func BootFrom(allocated, dir string) (*Clock, error) {
	if !Canonical(allocated) {
		if allocated != "" {
			fmt.Fprintf(os.Stderr, "INSTANCE_UUID %q is not a canonical UUID; minting one\n", allocated)
		}
		return Minted(), nil
	}
	uuid := strings.ToLower(allocated)
	return Persisted(uuid, filepath.Join(dir, uuid+".lamport"))
}

// Minted is an unpersisted clock under a fresh version 7 UUID.
func Minted() *Clock { return &Clock{uuid: NewV7()} }

// Persisted is an allocated clock kept in file. The lock is taken before
// anything is read or written.
func Persisted(uuid, file string) (*Clock, error) {
	lock, err := lockInstance(file, uuid)
	if err != nil {
		return nil, err
	}
	lamport, err := ReadReservation(file)
	if err != nil {
		lock.Close()
		return nil, err
	}
	c := &Clock{uuid: uuid, lamport: lamport, file: file, lock: lock}
	if err := c.reserve(lamport); err != nil {
		lock.Close()
		return nil, err
	}
	return c, nil
}

// Tick advances the clock for a committed step and returns the new value.
func (c *Clock) Tick() (uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := c.lamport + 1
	if c.file != "" && next > c.reserved {
		if err := c.reserve(next); err != nil { // persist first, then issue
			return 0, err
		}
	}
	c.lamport = next
	return next, nil
}

// Instance is the instance UUID.
func (c *Clock) Instance() string { return c.uuid }

// Lamport is the newest issued value.
func (c *Clock) Lamport() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lamport
}

// Close releases the instance lock (tests; a process releases it on exit).
func (c *Clock) Close() error {
	if c.lock == nil {
		return nil
	}
	return c.lock.Close()
}

func (c *Clock) reserve(through uint64) error {
	next := through + Reservation
	if c.file != "" {
		if err := writeReservation(c.file, next); err != nil {
			return err
		}
	}
	c.reserved = next
	return nil
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
	tmp := strings.TrimSuffix(file, ".lamport") + ".lamport-tmp"
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
