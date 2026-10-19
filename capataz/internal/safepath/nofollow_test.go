package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// nofollowFixture crea:
//
//	base/fuera/secreto.txt
//	base/carpeta/
//	  a.txt  docs/b.txt  docs/sub/c.txt  vacia/
//	  .stversions/v.txt  .syncthing.x.tmp
//	  enlace-dentro -> a.txt          enlace-dir-dentro -> docs
//	  enlace-fuera  -> ../fuera/secreto.txt
//	  docs/enlace-hermano -> b.txt    fifo
func nofollowFixture(t *testing.T) *Folder {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "carpeta")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"fuera", "carpeta/docs/sub", "carpeta/vacia", "carpeta/.stversions"} {
		must(os.MkdirAll(filepath.Join(base, d), 0o755))
	}
	for name, content := range map[string]string{
		"fuera/secreto.txt":         "secreto",
		"carpeta/a.txt":             "hola",
		"carpeta/docs/b.txt":        "b",
		"carpeta/docs/sub/c.txt":    "c",
		"carpeta/.stversions/v.txt": "v",
		"carpeta/.syncthing.x.tmp":  "parcial",
	} {
		must(os.WriteFile(filepath.Join(base, name), []byte(content), 0o644))
	}
	for name, target := range map[string]string{
		"enlace-dentro":       "a.txt",
		"enlace-dir-dentro":   "docs",
		"enlace-fuera":        "../fuera/secreto.txt",
		"docs/enlace-hermano": "b.txt",
	} {
		must(os.Symlink(target, filepath.Join(dir, name)))
	}
	must(syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))
	f, err := OpenFolder(dir)
	must(err)
	t.Cleanup(func() { f.Close() })
	return f
}

func TestStatAndOpenNoFollow(t *testing.T) {
	f := nofollowFixture(t)
	tests := []struct {
		rel     string
		statErr error
		openErr error
	}{
		{"a.txt", nil, nil},
		{"docs/sub/c.txt", nil, nil},
		{"docs", nil, ErrIsDir},
		{".", nil, ErrIsDir},
		{"fifo", nil, ErrNotRegular},
		{"enlace-dentro", ErrSymlink, ErrSymlink},           // aunque apunte dentro
		{"enlace-dir-dentro/b.txt", ErrSymlink, ErrSymlink}, // segmento intermedio
		{"enlace-dir-dentro", ErrSymlink, ErrSymlink},
		{"enlace-fuera", ErrSymlink, ErrSymlink},
		{"docs/enlace-hermano", ErrSymlink, ErrSymlink},
		{"no-existe", ErrNotFound, ErrNotFound},
		{"a.txt/x", ErrNotFound, ErrNotFound},
		{"../fuera/secreto.txt", ErrNotAllowed, ErrNotAllowed},
		{"/etc/hosts", ErrNotAllowed, ErrNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			if _, err := f.StatNoFollow(tt.rel); !errors.Is(err, tt.statErr) {
				t.Errorf("StatNoFollow = %v; quiero %v", err, tt.statErr)
			}
			file, _, err := f.OpenNoFollow(tt.rel)
			if file != nil {
				file.Close()
			}
			if !errors.Is(err, tt.openErr) {
				t.Errorf("OpenNoFollow = %v; quiero %v", err, tt.openErr)
			}
		})
	}
}

func TestWalk(t *testing.T) {
	f := nofollowFixture(t)
	var got []string
	err := f.Walk("", func(e WalkEntry) error {
		kind := map[Kind]string{KindFile: "F", KindDir: "D", KindHidden: "H", KindSymlink: "L", KindSpecial: "S"}[e.Kind]
		got = append(got, kind+" "+e.Path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"H .stversions", // no se recorre: v.txt no aparece
		"H .syncthing.x.tmp",
		"F a.txt",
		"D docs",
		"F docs/b.txt",
		"L docs/enlace-hermano",
		"D docs/sub",
		"F docs/sub/c.txt",
		"L enlace-dentro",
		"L enlace-dir-dentro", // no se sigue: docs no aparece dos veces
		"L enlace-fuera",
		"S fifo",
		"D vacia",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Walk:\n%s\nquiero:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	var sub []string
	if err := f.Walk("docs/sub", func(e WalkEntry) error { sub = append(sub, e.Path); return nil }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sub, ",") != "docs/sub/c.txt" {
		t.Errorf("Walk(docs/sub) = %v", sub)
	}

	for rel, want := range map[string]error{
		"a.txt":             ErrNotDir,
		"enlace-dir-dentro": ErrSymlink,
		"no-existe":         ErrNotFound,
		"../fuera":          ErrNotAllowed,
	} {
		if err := f.Walk(rel, func(WalkEntry) error { return nil }); !errors.Is(err, want) {
			t.Errorf("Walk(%q) = %v; quiero %v", rel, err, want)
		}
	}

	stop := errors.New("parar")
	n := 0
	err = f.Walk("", func(WalkEntry) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Errorf("el error de fn debe detener el recorrido: %v tras %d", err, n)
	}
}
