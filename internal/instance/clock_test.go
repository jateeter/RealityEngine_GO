package instance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const uuid = "0192f3a4-5b6c-7d8e-9f01-23456789abcd"

func TestMintedIsACanonicalVersion7UUID(t *testing.T) {
	a, b := Minted(), Minted()
	if !Canonical(a.Instance()) || a.Instance()[14] != '7' || !strings.ContainsRune("89ab", rune(a.Instance()[19])) {
		t.Fatalf("not a v7 UUID: %s", a.Instance())
	}
	if a.Instance() == b.Instance() || a.Lamport() != 0 {
		t.Fatalf("%s %s %d", a.Instance(), b.Instance(), a.Lamport())
	}
}

func TestWithoutAnAllocationTheClockIsMintedAndUnpersisted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clock")
	for _, allocated := range []string{"", "not-a-uuid"} {
		c, err := BootFrom(allocated, dir)
		if err != nil || !Canonical(c.Instance()) {
			t.Fatalf("%q: %v %v", allocated, c, err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("a minted clock wrote something")
	}
}

func TestAnAllocatedUUIDIsLowercasedAndReservesBeforeIssuing(t *testing.T) {
	dir := t.TempDir()
	c, err := BootFrom(strings.ToUpper(uuid), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	file := filepath.Join(dir, uuid+".lamport")
	if c.Instance() != uuid {
		t.Fatal(c.Instance())
	}
	if _, err := os.Stat(filepath.Join(dir, uuid+".lock")); err != nil {
		t.Fatal(err)
	}
	mark := func() uint64 {
		v, err := ReadReservation(file)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if mark() != Reservation {
		t.Fatal(mark())
	}
	for want := uint64(1); want <= Reservation; want++ {
		if got, _ := tick(c); got != want {
			t.Fatalf("tick %d = %d", want, got)
		}
	}
	if mark() != Reservation {
		t.Fatal("the mark moved before it had to")
	}
	if got, _ := tick(c); got != Reservation+1 || mark() != 2*Reservation+1 {
		t.Fatalf("%d %d", got, mark())
	}
}

func TestARestartResumesFromTheMark(t *testing.T) {
	file := filepath.Join(t.TempDir(), uuid+".lamport")
	c, err := Persisted(uuid, file)
	if err != nil {
		t.Fatal(err)
	}
	tick(c)
	issued, _ := tick(c)
	c.Close()
	c, err = Persisted(uuid, file)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Lamport() != Reservation {
		t.Fatal(c.Lamport())
	}
	if next, _ := tick(c); next <= issued {
		t.Fatalf("%d repeats %d", next, issued)
	}
}

func TestAClockFileThatIsNotAWholeNumberRefusesTheBoot(t *testing.T) {
	file := filepath.Join(t.TempDir(), uuid+".lamport")
	if err := os.WriteFile(file, []byte("12x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Persisted(uuid, file); err == nil || !strings.Contains(err.Error(), `does not hold a whole number: "12x"`) {
		t.Fatal(err)
	}
}

// A second process presenting a live UUID refuses to boot. fcntl locks are
// per process, so the second boot runs in a child.
func TestASecondLiveProcessRefusesTheUUID(t *testing.T) {
	if dir := os.Getenv("INSTANCE_TEST_CHILD_DIR"); dir != "" {
		if _, err := BootFrom(uuid, dir); err != nil {
			os.Stdout.WriteString(err.Error())
			os.Exit(3)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	c, err := BootFrom(uuid, dir)
	if err != nil {
		t.Fatal(err)
	}
	child := func() (int, string) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestASecondLiveProcessRefusesTheUUID$")
		cmd.Env = append(os.Environ(), "INSTANCE_TEST_CHILD_DIR="+dir)
		out, _ := cmd.Output()
		return cmd.ProcessState.ExitCode(), string(out)
	}
	if code, out := child(); code != 3 || !strings.Contains(out, "is already live") {
		t.Fatalf("held: exit %d: %s", code, out)
	}
	c.Close()
	if code, out := child(); code != 0 {
		t.Fatalf("released: exit %d: %s", code, out)
	}
}

// tick is a committed step's lamport.
func tick(c *Clock) (uint64, error) {
	l, _, err := c.CommitStep()
	return l, err
}

func TestSendsAndReceivesTickLamportOnly(t *testing.T) {
	c := Minted()
	steps := []struct {
		got  func() (uint64, error)
		want uint64
	}{
		{c.Send, 1},
		{func() (uint64, error) { return c.Receive(0, false) }, 2},
		{func() (uint64, error) { return c.Receive(10, true) }, 11},
		{func() (uint64, error) { return c.Receive(3, true) }, 12},
	}
	for i, s := range steps {
		if got, _ := s.got(); got != s.want {
			t.Fatalf("step %d: lamport %d, want %d", i, got, s.want)
		}
	}
	if l, cm, _ := c.CommitStep(); l != 13 || cm != 1 {
		t.Fatalf("commit: (%d, %d), want (13, 1)", l, cm)
	}
}

func TestAReceivePastTheMarkIsPersistedFirst(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "0192f3a4-5b6c-7d8e-9f01-23456789abcd.lamport")
	c, err := Persisted("0192f3a4-5b6c-7d8e-9f01-23456789abcd", file)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got, _ := c.Receive(5_000_000, true); got != 5_000_001 {
		t.Fatalf("receive: %d", got)
	}
	if mark, _ := ReadReservation(file); mark != 5_000_001+Reservation {
		t.Fatalf("mark %d", mark)
	}
}

func TestAPre600DirectorySeedsBothMarks(t *testing.T) {
	dir := t.TempDir()
	uuid := "0192f3a4-5b6c-7d8e-9f01-23456789abcd"
	file := filepath.Join(dir, uuid+".lamport")
	if err := os.WriteFile(file, []byte("4096\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Persisted(uuid, file)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Lamport() != 4096 || c.Commit() != 4096 {
		t.Fatalf("(%d, %d), want (4096, 4096)", c.Lamport(), c.Commit())
	}
	if mark, _ := ReadReservation(filepath.Join(dir, uuid+".commit")); mark != 4096+Reservation {
		t.Fatalf("commit mark %d", mark)
	}
}

func TestTheHeaderParsesAsTheSpecSays(t *testing.T) {
	good := map[string]uint64{
		"instance=0192f3a4-5b6c-7d8e-9f01-23456789abcd; lamport=42":               42,
		"instance=0192f3a4-5b6c-7d8e-9f01-23456789abcd;lamport=0":                 0,
		"instance=0192f3a4-5b6c-7d8e-9f01-23456789abcd; lamport=9007199254740991": MaxLamport,
	}
	for v, want := range good {
		if got, ok := ParseHeader(v); !ok || got != want {
			t.Errorf("%q: (%d, %v)", v, got, ok)
		}
	}
	for _, v := range []string{
		"", "bad",
		"lamport=1; instance=0192f3a4-5b6c-7d8e-9f01-23456789abcd",
		"instance=0192F3A4-5B6C-7D8E-9F01-23456789ABCD; lamport=1",
		"instance=0192f3a4-5b6c-7d8e-9f01-23456789abcd; lamport=01",
		"instance=0192f3a4-5b6c-7d8e-9f01-23456789abcd; lamport=-1",
		"instance=0192f3a4-5b6c-7d8e-9f01-23456789abcd; lamport=9007199254740992",
		"instance=0192f3a4-5b6c-7d8e-9f01-23456789abcd; lamport=1; x=2",
	} {
		if _, ok := ParseHeader(v); ok {
			t.Errorf("%q parsed", v)
		}
	}
}

func TestCausedByPrefersTheRecordThenAWellFormedEcho(t *testing.T) {
	own := map[string]any{"instance": "0192f3a0-0000-7000-8000-0000000000c2", "lamport": uint64(7)}
	echo := map[string]any{"instance": "0192f3a0-0000-7000-8000-000000000009", "lamport": float64(42)}
	if got := CausedBy(own, echo).(map[string]any); got["lamport"] != uint64(7) {
		t.Fatalf("own record: %v", got)
	}
	if got := CausedBy(nil, echo).(map[string]any); got["lamport"] != uint64(42) {
		t.Fatalf("echo: %v", got)
	}
	for _, bad := range []any{
		map[string]any{"instance": "NOPE", "lamport": float64(42)},
		map[string]any{"instance": "0192F3A0-0000-7000-8000-000000000009", "lamport": float64(42)},
		map[string]any{"instance": "0192f3a0-0000-7000-8000-000000000009", "lamport": float64(-1)},
		nil,
	} {
		if got := CausedBy(nil, bad); got != nil {
			t.Errorf("%v: %v", bad, got)
		}
	}
}

func TestAnExternalPartyIsNeverStamped(t *testing.T) {
	for _, h := range []string{"127.0.0.1", "localhost", "192.168.1.4", "10.0.0.2", "engine-cpp-re", "go-1.localhost", "host.docker.internal", "::1"} {
		if !InternalHost(h) {
			t.Errorf("%s should be internal", h)
		}
	}
	for _, h := range []string{"api.openai.com", "8.8.8.8", "yuma.lateraledge.cloud"} {
		if InternalHost(h) {
			t.Errorf("%s should be external", h)
		}
	}
}
