package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "estado")

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.InstanceID) != 26 {
		t.Errorf("instanceId %q: se esperaban 26 caracteres", first.InstanceID)
	}

	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Errorf("el segundo arranque cambió la identidad: %+v != %+v", second, first)
	}

	for p, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, fileName): 0o600} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s: permisos %o, quiero %o", p, got, want)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".instance-*")); len(left) > 0 {
		t.Errorf("quedaron temporales: %v", left)
	}
}

func TestLoadOrCreateConcurrent(t *testing.T) {
	dir := t.TempDir()
	ids := make([]string, 8)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Go(func() {
			inst, err := LoadOrCreate(dir)
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = inst.InstanceID
		})
	}
	wg.Wait()
	for _, id := range ids[1:] {
		if id != ids[0] {
			t.Fatalf("arranques simultáneos con identidades distintas: %v", ids)
		}
	}
}

func TestLoadOrCreateCorrupt(t *testing.T) {
	for name, content := range map[string]string{
		"json inválido": "{no es json",
		"sin id":        `{"createdAt":"2026-10-19T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreate(dir); err == nil {
				t.Error("se esperaba error; nunca debe regenerar una identidad dañada en silencio")
			}
		})
	}
}
