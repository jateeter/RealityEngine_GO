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
		if got, _ := c.Tick(); got != want {
			t.Fatalf("tick %d = %d", want, got)
		}
	}
	if mark() != Reservation {
		t.Fatal("the mark moved before it had to")
	}
	if got, _ := c.Tick(); got != Reservation+1 || mark() != 2*Reservation+1 {
		t.Fatalf("%d %d", got, mark())
	}
}

func TestARestartResumesFromTheMark(t *testing.T) {
	file := filepath.Join(t.TempDir(), uuid+".lamport")
	c, err := Persisted(uuid, file)
	if err != nil {
		t.Fatal(err)
	}
	c.Tick()
	issued, _ := c.Tick()
	c.Close()
	c, err = Persisted(uuid, file)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Lamport() != Reservation {
		t.Fatal(c.Lamport())
	}
	if next, _ := c.Tick(); next <= issued {
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
