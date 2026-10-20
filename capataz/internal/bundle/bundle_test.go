package bundle

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/resuelveonline/capataz/capataz/internal/cpk"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
	"github.com/resuelveonline/capataz/capataz/internal/stage/stagetest"
)

const folderID = "fgyww-edjwa"

var (
	created = time.Date(2026, 10, 19, 7, 0, 0, 0, time.UTC)
	now     = time.Date(2026, 10, 19, 1, 30, 0, 0, time.Local)
)

type env struct {
	t      *testing.T
	dir    string
	fake   *stagetest.Fake
	stager *stage.Stager
}

// newEnv escribe files (ruta relativa -> contenido) y crea los directorios
// vacíos dirs dentro de una carpeta "pruebas" indexada por el falso.
func newEnv(t *testing.T, files map[string]string, dirs ...string) *env {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "pruebas")
	for rel, content := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range dirs {
		os.MkdirAll(filepath.Join(dir, d), 0o755)
	}
	fake := stagetest.New(t, dir, folderID, "pruebas")
	s, err := stage.New(stage.Options{
		Syncthing: fake, StagingDir: filepath.Join(base, "staging"), MaxPayload: cpk.MaxPayload,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, dir: dir, fake: fake, stager: s}
}

func (e *env) snapshot(paths ...string) *stage.Snapshot {
	e.t.Helper()
	var items []stage.Item
	for _, p := range paths {
		items = append(items, stage.Item{Folder: folderID, Path: p})
	}
	sn, err := e.stager.Begin(context.Background(), items)
	if err != nil {
		e.t.Fatal(err)
	}
	sn.CreatedAt = created
	e.t.Cleanup(func() { sn.Close() })
	return sn
}

func (e *env) build(o Options, paths ...string) (*Payload, *stage.Snapshot, error) {
	sn := e.snapshot(paths...)
	if o.Now.IsZero() {
		o.Now = now
	}
	p, err := Build(context.Background(), sn, o)
	return p, sn, err
}

func metaJSON(t *testing.T, p *Payload) string {
	t.Helper()
	b, err := p.MetaJSON()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSingleFile(t *testing.T) {
	e := newEnv(t, map[string]string{"docs/contrato <final>.pdf": "%PDF-1.7 contenido"})
	p, sn, err := e.build(Options{Label: "Para Ana & Luis"}, "docs/contrato <final>.pdf")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("%PDF-1.7 contenido")
	got, _ := os.ReadFile(p.Path)
	if p.Kind != KindFile || string(got) != string(want) || p.Size != int64(len(want)) || p.SHA256 != sha256.Sum256(want) {
		t.Errorf("payload %+v", p)
	}
	if !strings.HasPrefix(p.Path, sn.Dir()) {
		t.Errorf("la carga útil debe estar en el staging: %s", p.Path)
	}
	// Sin escapes HTML ni espacios; sin campos de zip.
	wantMeta := `{"v":1,"kind":"file","name":"contrato <final>.pdf","size":18,"mime":"application/pdf","createdAt":"2026-10-19T07:00:00Z","label":"Para Ana & Luis"}`
	if m := metaJSON(t, p); m != wantMeta {
		t.Errorf("meta\n got  %s\n want %s", m, wantMeta)
	}
}

func readZip(t *testing.T, p *Payload) (*zip.ReadCloser, map[string]*zip.File) {
	t.Helper()
	zr, err := zip.OpenReader(p.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { zr.Close() })
	byName := map[string]*zip.File{}
	for _, f := range zr.File {
		byName[f.Name] = f
	}
	return zr, byName
}

func content(t *testing.T, f *zip.File) string {
	t.Helper()
	r, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFolderZip(t *testing.T) {
	e := newEnv(t, map[string]string{
		"fotos/a.JPG":       "jpeg-ya-comprimido",
		"fotos/notas.txt":   strings.Repeat("texto comprimible ", 200),
		"fotos/sub/año.txt": "ñ",
	}, "fotos/vacia")
	p, sn, err := e.build(Options{}, "fotos")
	if err != nil {
		t.Fatal(err)
	}
	zr, byName := readZip(t, p)

	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	want := []string{"fotos/", "fotos/a.JPG", "fotos/notas.txt", "fotos/sub/", "fotos/sub/año.txt", "fotos/vacia/"}
	if !slices.Equal(names, want) {
		t.Errorf("entradas %v\n quiero %v", names, want)
	}
	if m := byName["fotos/a.JPG"].Method; m != zip.Store {
		t.Errorf(".JPG con método %d; quiero Store", m)
	}
	if m := byName["fotos/notas.txt"].Method; m != zip.Deflate {
		t.Errorf(".txt con método %d; quiero Deflate", m)
	}
	if f := byName["fotos/sub/año.txt"]; f.NonUTF8 || f.Flags&0x800 == 0 || content(t, f) != "ñ" {
		t.Errorf("nombre UTF-8: flags %#x", f.Flags)
	}
	if content(t, byName["fotos/notas.txt"]) != strings.Repeat("texto comprimible ", 200) {
		t.Error("contenido de notas.txt distinto")
	}
	raw, _ := os.ReadFile(p.Path)
	if p.SHA256 != sha256.Sum256(raw) || p.Size != int64(len(raw)) {
		t.Error("SHA-256 o tamaño de la carga útil no corresponden al ZIP")
	}
	// Solo queda el ZIP en el staging: cada copia se borró al añadirla.
	if left, _ := os.ReadDir(sn.Dir()); len(left) != 1 || left[0].Name() != "payload.zip" {
		t.Errorf("quedaron en el staging: %v", left)
	}

	var m map[string]any
	json.Unmarshal([]byte(metaJSON(t, p)), &m)
	if m["kind"] != "zip" || m["name"] != "fotos.zip" || m["entryCount"] != float64(3) || m["mime"] != nil || m["entriesTruncated"] != nil {
		t.Errorf("meta %v", m)
	}
	entries := m["entries"].([]any)
	if len(entries) != 3 || entries[0].(map[string]any)["path"] != "fotos/a.JPG" {
		t.Errorf("entries %v (solo archivos)", entries)
	}
}

func TestMultipleItemsCollisionsAndName(t *testing.T) {
	e := newEnv(t, map[string]string{
		"a/Informe.pdf": "1",
		"b/informe.pdf": "2",
		"c/informe.pdf": "3",
		"x/fotos/1.jpg": "x",
		"y/fotos/2.jpg": "y",
		"z/.bashrc":     "z",
		"w/.bashrc":     "w",
	})
	p, _, err := e.build(Options{}, "a/Informe.pdf", "b/informe.pdf", "c/informe.pdf", "x/fotos", "y/fotos", "z/.bashrc", "w/.bashrc")
	if err != nil {
		t.Fatal(err)
	}
	_, byName := readZip(t, p)
	for name, want := range map[string]string{
		"Informe.pdf":     "1",
		"informe (2).pdf": "2", // sin distinguir mayúsculas
		"informe (3).pdf": "3",
		"fotos/1.jpg":     "x",
		"fotos (2)/2.jpg": "y",
		".bashrc":         "z",
		".bashrc (2)":     "w", // sin extensión
	} {
		f, ok := byName[name]
		if !ok {
			t.Errorf("falta %q en el ZIP", name)
			continue
		}
		if c := content(t, f); c != want {
			t.Errorf("%s = %q; quiero %q", name, c, want)
		}
	}
	if p.Meta.Name != "archivos-20261019.zip" {
		t.Errorf("nombre %q; quiero archivos-AAAAMMDD.zip con la fecha local", p.Meta.Name)
	}

	p, _, err = e.build(Options{Label: " Entrega: 2026/10\x07 "}, "a/Informe.pdf", "b/informe.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if p.Meta.Name != "Entrega: 2026_10.zip" || p.Meta.Label != "Entrega: 2026_10" {
		t.Errorf("con label: nombre %q, label %q", p.Meta.Name, p.Meta.Label)
	}
}

func TestPayloadTooLarge(t *testing.T) {
	e := newEnv(t, map[string]string{
		"grande.bin":          strings.Repeat("x", 1000),
		"fotos/a.jpg":         strings.Repeat("j", 600),
		"fotos/b.jpg":         strings.Repeat("j", 600),
		"texto/largo.txt":     strings.Repeat("abcdefghij", 300), // 3000 bytes, se comprime mucho
		"texto/aleatorio.txt": randomText(4000),                  // no se comprime
	})
	wantTooLarge := func(t *testing.T, err error, sn *stage.Snapshot) {
		t.Helper()
		var se *stage.Error
		if !errors.As(err, &se) || se.Code != CodePayloadTooLarge {
			t.Fatalf("error = %v; quiero payload_too_large", err)
		}
		if left, _ := os.ReadDir(sn.Dir()); len(left) != 0 {
			t.Errorf("tras el error quedaron en el staging: %v", left)
		}
	}

	t.Run("archivo suelto, antes de copiar", func(t *testing.T) {
		_, sn, err := e.build(Options{MaxPayload: 999}, "grande.bin")
		wantTooLarge(t, err, sn)
	})
	t.Run("archivo suelto justo en el tope", func(t *testing.T) {
		if _, _, err := e.build(Options{MaxPayload: 1000}, "grande.bin"); err != nil {
			t.Errorf("1000 bytes con tope 1000: %v", err)
		}
	})
	t.Run("ZIP: la cota inferior ya supera el tope", func(t *testing.T) {
		_, sn, err := e.build(Options{MaxPayload: 1100}, "fotos") // 1200 bytes de jpg sin comprimir
		wantTooLarge(t, err, sn)
	})
	t.Run("ZIP: se aborta mientras se arma", func(t *testing.T) {
		_, sn, err := e.build(Options{MaxPayload: 2500}, "texto") // la cota pasa; el aleatorio no cabe
		wantTooLarge(t, err, sn)
	})
	t.Run("ZIP comprimible que sí cabe", func(t *testing.T) {
		p, _, err := e.build(Options{MaxPayload: 2500}, "texto/largo.txt", "grande.bin")
		if err != nil || p.Size > 2500 {
			t.Errorf("3000+1000 bytes comprimibles con tope 2500: %v (tamaño %d)", err, p.Size)
		}
	})
}

func randomText(n int) string {
	b := make([]byte, n)
	f, _ := os.Open("/dev/urandom")
	defer f.Close()
	io.ReadFull(f, b)
	return string(b)
}

func TestEntriesLimit(t *testing.T) {
	files := map[string]string{}
	for i := range 510 {
		files[fmt.Sprintf("muchos/%04d.txt", i)] = "x"
	}
	e := newEnv(t, files)
	p, _, err := e.build(Options{}, "muchos")
	if err != nil {
		t.Fatal(err)
	}
	var m Meta
	json.Unmarshal([]byte(metaJSON(t, p)), &m)
	if *m.EntryCount != 510 || len(*m.Entries) != 500 || m.EntriesTruncated == nil || !*m.EntriesTruncated {
		t.Errorf("entryCount %d, entries %d, truncado %v", *m.EntryCount, len(*m.Entries), m.EntriesTruncated)
	}
}

// Rutas largas: 500 entradas no caben en 192 KiB y se recortan.
func TestMetaFitsBlobLimit(t *testing.T) {
	files := map[string]string{}
	dir := strings.Repeat("d", 200)
	for i := range 480 {
		files[fmt.Sprintf("%s/%04d%s.txt", dir, i, strings.Repeat("n", 240))] = "x"
	}
	e := newEnv(t, files)
	p, _, err := e.build(Options{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.MetaJSON()
	if err != nil {
		t.Fatal(err)
	}
	if cpk.MetaBlobSize(len(b)) > cpk.MaxMetaBlob {
		t.Errorf("metaBlob de %d bytes supera el tope", cpk.MetaBlobSize(len(b)))
	}
	var m Meta
	json.Unmarshal(b, &m)
	if len(*m.Entries) >= 480 || !*m.EntriesTruncated || *m.EntryCount != 480 {
		t.Errorf("entries %d, truncado %v, entryCount %d", len(*m.Entries), *m.EntriesTruncated, *m.EntryCount)
	}
}

func TestEmptyFolderZip(t *testing.T) {
	e := newEnv(t, nil, "vacia")
	p, _, err := e.build(Options{}, "vacia")
	if err != nil {
		t.Fatal(err)
	}
	if m := metaJSON(t, p); !strings.Contains(m, `"entryCount":0,"entries":[]`) {
		t.Errorf("un zip sin archivos lleva entryCount 0 y entries vacío: %s", m)
	}
}

func TestNames(t *testing.T) {
	long := strings.Repeat("á", 250)
	if got := fitName(long+".pdf", "x"); utf8.RuneCountInString(got) != 200 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("fitName largo: %d caracteres, %q…", utf8.RuneCountInString(got), got[len(got)-8:])
	}
	for in, want := range map[string]string{
		"a/b\\c":            "a_b_c",
		"  con\tcontrol\n ": "concontrol",
		"normal.txt":        "normal.txt",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q; quiero %q", in, got, want)
		}
	}
	if !compressed("x.MP4") || !compressed("a.tar.GZ") || compressed("a.txt") || compressed("zip") {
		t.Error("detección de extensiones comprimidas")
	}
	if mimeOf("a.pdf") != "application/pdf" || mimeOf("a.desconocida") != "application/octet-stream" {
		t.Error("mimeOf")
	}
}
