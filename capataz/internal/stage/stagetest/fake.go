// Package stagetest ofrece un Syncthing falso para probar stage y lo que
// se construye encima: indexa una carpeta real del disco como si Syncthing
// la tuviera completamente sincronizada.
package stagetest

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/resuelveonline/capataz/capataz/internal/safepath"
	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// Fake implementa stage.Syncthing.
type Fake struct {
	mu     sync.Mutex
	folder syncthing.Folder
	index  map[string]syncthing.FileStatus
}

// New indexa todos los archivos regulares de dir (sin ocultos ni enlaces)
// en una carpeta con el id y label dados.
func New(t testing.TB, dir, id, label string) *Fake {
	t.Helper()
	f := &Fake{folder: syncthing.Folder{ID: id, Label: label, Path: dir}, index: map[string]syncthing.FileStatus{}}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p != dir && safepath.IsHidden(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			f.Index(t, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// Index (re)indexa rel con el estado actual del disco.
func (f *Fake) Index(t testing.TB, rel string) {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(f.folder.Path, rel))
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.index[rel].Local.Sequence + 1
	info := syncthing.FileInfo{
		Name: rel, Size: fi.Size(), Modified: fi.ModTime(), Type: "FILE_INFO_TYPE_FILE",
		Version: []string{fmt.Sprintf("FAKE:%d", seq)}, Sequence: seq,
	}
	f.index[rel] = syncthing.FileStatus{Local: info, Global: info}
}

// Forget quita rel del índice (como un archivo ignorado).
func (f *Fake) Forget(rel string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.index, rel)
}

func (f *Fake) Folders(context.Context) ([]syncthing.Folder, error) {
	return []syncthing.Folder{f.folder}, nil
}

func (f *Fake) File(_ context.Context, _, rel string) (syncthing.FileStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.index[rel]
	if !ok {
		return st, syncthing.ErrNotIndexed
	}
	return st, nil
}

func (f *Fake) Scan(context.Context, string, string) error           { return nil }
func (f *Fake) Need(context.Context, string) (syncthing.Need, error) { return syncthing.Need{}, nil }
