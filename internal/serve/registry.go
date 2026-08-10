package serve

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/fileutil"
)

// spawnChildEntry is one row of the spawned-children registry. The parent
// records every child it launches; each child removes its own row on graceful
// exit; the parent sweeps dead rows at startup and before each spawn so the
// file never becomes a graveyard.
type spawnChildEntry struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	SpawnedAt time.Time `json:"spawned_at"`
}

type spawnChildRegistry struct {
	Children map[string]spawnChildEntry `json:"children"`
}

// spawnRegistryPath is <reasonix home>/serve_children.json; empty when the
// home directory cannot be resolved (registry disabled).
func spawnRegistryPath() string {
	home := strings.TrimSpace(config.ReasonixHomeDir())
	if home == "" {
		return ""
	}
	return filepath.Join(home, "serve_children.json")
}

func loadSpawnRegistry() (spawnChildRegistry, error) {
	path := spawnRegistryPath()
	if path == "" {
		return spawnChildRegistry{}, fmt.Errorf("spawn registry: no reasonix home")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return spawnChildRegistry{Children: map[string]spawnChildEntry{}}, nil
		}
		return spawnChildRegistry{}, err
	}
	var reg spawnChildRegistry
	if err := json.Unmarshal(b, &reg); err != nil {
		return spawnChildRegistry{}, fmt.Errorf("spawn registry %s: %w", path, err)
	}
	if reg.Children == nil {
		reg.Children = map[string]spawnChildEntry{}
	}
	return reg, nil
}

func saveSpawnRegistry(reg spawnChildRegistry) error {
	path := spawnRegistryPath()
	if path == "" {
		return nil
	}
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".serve-children.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return fileutil.ReplaceFile(tmpName, path)
}

// RecordSpawnChild upserts one child in the registry.
func RecordSpawnChild(pid, port int) error {
	reg, err := loadSpawnRegistry()
	if err != nil {
		return err
	}
	reg.Children[strconv.Itoa(pid)] = spawnChildEntry{PID: pid, Port: port, SpawnedAt: time.Now()}
	return saveSpawnRegistry(reg)
}

// RemoveSpawnChild deletes one child row (called by the child itself on
// graceful exit, and on spawn failure rollback). A missing registry is a no-op.
func RemoveSpawnChild(pid int) error {
	reg, err := loadSpawnRegistry()
	if err != nil {
		return err
	}
	if _, ok := reg.Children[strconv.Itoa(pid)]; !ok {
		return nil
	}
	delete(reg.Children, strconv.Itoa(pid))
	return saveSpawnRegistry(reg)
}

// SweepSpawnChildren drops registry rows whose pid is no longer alive and
// returns how many were removed. Best-effort: corrupt/missing registries are
// ignored so a half-written file never blocks spawning.
func SweepSpawnChildren() int {
	reg, err := loadSpawnRegistry()
	if err != nil {
		return 0
	}
	removed := 0
	for key, entry := range reg.Children {
		if !processAlive(entry.PID) {
			delete(reg.Children, key)
			removed++
		}
	}
	if removed > 0 {
		_ = saveSpawnRegistry(reg)
	}
	return removed
}

// processAlive reports whether pid names a live process. Unix probes with
// signal 0 (ESRCH → dead); Windows relies on FindProcess failing for dead
// pids, treating an unsupported Signal as alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || !errors.Is(err, os.ErrProcessDone)
}
