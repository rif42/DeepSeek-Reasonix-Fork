package serve

import (
	"os"
	"strconv"
	"testing"
)

func TestSpawnRegistryRoundTrip(t *testing.T) {
	t.Setenv("REASONIX_HOME", t.TempDir())
	if path := spawnRegistryPath(); path == "" {
		t.Fatal("spawnRegistryPath empty with REASONIX_HOME set")
	}
	if err := RecordSpawnChild(12345, 8788); err != nil {
		t.Fatal(err)
	}
	reg, err := loadSpawnRegistry()
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Children["12345"]
	if !ok || entry.Port != 8788 || entry.PID != 12345 {
		t.Fatalf("recorded entry = %+v, ok=%v", entry, ok)
	}
	if err := RemoveSpawnChild(12345); err != nil {
		t.Fatal(err)
	}
	reg, err = loadSpawnRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Children["12345"]; ok {
		t.Fatal("entry survived RemoveSpawnChild")
	}
	// Removing a missing pid is a no-op, not an error.
	if err := RemoveSpawnChild(99999); err != nil {
		t.Fatalf("RemoveSpawnChild(missing) = %v, want nil", err)
	}
}

func TestSweepSpawnChildrenDropsDeadPids(t *testing.T) {
	t.Setenv("REASONIX_HOME", t.TempDir())
	dead := 1 << 30 // far beyond any real pid on this machine
	if err := RecordSpawnChild(dead, 8789); err != nil {
		t.Fatal(err)
	}
	alive := os.Getpid()
	if err := RecordSpawnChild(alive, 8790); err != nil {
		t.Fatal(err)
	}
	if n := SweepSpawnChildren(); n != 1 {
		t.Fatalf("SweepSpawnChildren removed %d rows, want 1", n)
	}
	reg, err := loadSpawnRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Children[strconv.Itoa(dead)]; ok {
		t.Fatal("dead-pid row survived the sweep")
	}
	if _, ok := reg.Children[strconv.Itoa(alive)]; !ok {
		t.Fatal("alive-pid row was dropped by the sweep")
	}
}
