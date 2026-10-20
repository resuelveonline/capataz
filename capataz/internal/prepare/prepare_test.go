package prepare

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/cpk"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
	"github.com/resuelveonline/capataz/capataz/internal/stage/stagetest"
)

const folderID = "fgyww-edjwa"

type env struct {
	t       *testing.T
	dir     string
	staging string
	out     string
	logs    *bytes.Buffer
	stager  *stage.Stager
}

func newEnv(t *testing.T, files map[string]string) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{t: t, dir: filepath.Join(base, "pruebas"), staging: filepath.Join(base, "staging"),
		out: filepath.Join(base, "salida"), logs: &bytes.Buffer{}}
	for rel, c := range files {
		p := filepath.Join(e.dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := stage.New(stage.Options{
		Syncthing: stagetest.New(t, e.dir, folderID, "pruebas"), StagingDir: e.staging,
		MaxPayload: cpk.MaxPayload, Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	e.stager = s
	return e
}

func (e *env) run(label string, paths ...string) (*Result, error) {
	var items []stage.Item
	for _, p := range paths {
		items = append(items, stage.Item{Folder: folderID, Path: p})
	}
	return Run(context.Background(), Options{
		Stager: e.stager, Items: items, OutDir: e.out, Label: label,
		Now: time.Date(2026, 10, 19, 1, 0, 0, 0, time.Local), Logger: slog.New(slog.NewTextHandler(e.logs, nil)),
	})
}

// open descifra la salida con la clave y devuelve el claro y los metadatos.
func (e *env) open(r *Result) ([]byte, map[string]any) {
	e.t.Helper()
	pkg, err := os.ReadFile(filepath.Join(e.out, PackageFile))
	if err != nil {
		e.t.Fatal(err)
	}
	m := r.Manifest
	if int64(len(pkg)) != m.PackageSize || hex.EncodeToString(sum(pkg)) != m.PackageSha256 {
		e.t.Error("el manifiesto no describe el paquete escrito")
	}
	var plain bytes.Buffer
	if _, err := cpk.Decrypt(&plain, bytes.NewReader(pkg), r.Key); err != nil {
		e.t.Fatal(err)
	}
	if int64(plain.Len()) != m.PayloadSize || hex.EncodeToString(sum(plain.Bytes())) != m.PayloadSha256 {
		e.t.Error("el manifiesto no describe la carga útil")
	}
	b64, _ := os.ReadFile(filepath.Join(e.out, MetaFile))
	blob, err := base64.RawURLEncoding.DecodeString(string(b64))
	if err != nil || len(blob) != m.MetaBlobSize {
		e.t.Fatalf("meta.b64: %v", err)
	}
	mj, err := cpk.DecryptMeta(r.Key, blob)
	if err != nil {
		e.t.Fatal(err)
	}
	var meta map[string]any
	json.Unmarshal(mj, &meta)
	if meta["size"] != float64(m.PayloadSize) {
		e.t.Errorf("meta.size %v ≠ payloadSize %d", meta["size"], m.PayloadSize)
	}
	return plain.Bytes(), meta
}

func sum(b []byte) []byte { s := sha256.Sum256(b); return s[:] }

func (e *env) assertCleanStaging() {
	e.t.Helper()
	if left, _ := os.ReadDir(e.staging); len(left) != 0 {
		e.t.Errorf("el staging quedó con %d entradas", len(left))
	}
}

func TestRunSingleFile(t *testing.T) {
	e := newEnv(t, map[string]string{"informe.pdf": "%PDF contenido del informe"})
	r, err := e.run("", "informe.pdf")
	if err != nil {
		t.Fatal(err)
	}
	plain, meta := e.open(r)
	if string(plain) != "%PDF contenido del informe" || meta["kind"] != "file" || meta["name"] != "informe.pdf" {
		t.Errorf("claro %q, meta %v", plain, meta)
	}
	m := r.Manifest
	if m.Kind != "file" || m.CipherVersion != "cpk1" || m.ChunkSizeLog2 != 20 || len(m.LinkID) != 22 {
		t.Errorf("manifiesto %+v", m)
	}
	for _, f := range []string{PackageFile, MetaFile, ManifestFile} {
		if fi, err := os.Stat(filepath.Join(e.out, f)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v (%v)", f, err, fi.Mode())
		}
	}
	e.assertCleanStaging()
}

func TestRunFolder(t *testing.T) {
	e := newEnv(t, map[string]string{"fotos/a.jpg": "jpg", "fotos/sub/b.txt": "bbbb", "fotos/.stversions/x": "x"})
	r, err := e.run("", "fotos")
	if err != nil {
		t.Fatal(err)
	}
	plain, meta := e.open(r)
	zr, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		t.Fatalf("la carga útil no es un ZIP: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "fotos/,fotos/a.jpg,fotos/sub/,fotos/sub/b.txt" {
		t.Errorf("ZIP %v", names)
	}
	if meta["name"] != "fotos.zip" || meta["entryCount"] != float64(2) {
		t.Errorf("meta %v", meta)
	}
	if r.Manifest.Skipped.Hidden != 1 {
		t.Errorf("omitidos %+v", r.Manifest.Skipped)
	}
	e.assertCleanStaging()
}

func TestRunManifestHasNoNamesNorKey(t *testing.T) {
	e := newEnv(t, map[string]string{"expediente-confidencial.pdf": "contenido-secreto"})
	r, err := e.run("Etiqueta-secreta", "expediente-confidencial.pdf")
	if err != nil {
		t.Fatal(err)
	}
	mf, _ := os.ReadFile(filepath.Join(e.out, ManifestFile))
	b64, _ := os.ReadFile(filepath.Join(e.out, MetaFile))
	pkg, _ := os.ReadFile(filepath.Join(e.out, PackageFile))
	all := string(mf) + string(b64) + string(pkg) + e.logs.String()
	for _, leak := range []string{"expediente", "confidencial", "contenido-secreto", "Etiqueta-secreta", r.Key.Base64URL(), e.dir} {
		if strings.Contains(all, leak) {
			t.Errorf("la salida o la bitácora contienen %q", leak)
		}
	}
}

func TestRunRefusesOutputInsideSyncedFolder(t *testing.T) {
	e := newEnv(t, map[string]string{"a.txt": "a"})
	e.out = filepath.Join(e.dir, "salida")
	_, err := e.run("", "a.txt")
	var se *stage.Error
	if !errors.As(err, &se) || !strings.Contains(err.Error(), "la salida está dentro de una carpeta sincronizada") {
		t.Fatalf("error = %v", err)
	}
	e.assertCleanStaging()
}

func TestRunRefusesExistingPackage(t *testing.T) {
	e := newEnv(t, map[string]string{"a.txt": "a"})
	if _, err := e.run("", "a.txt"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(e.out, PackageFile))
	if _, err := e.run("", "a.txt"); err == nil {
		t.Fatal("no debe sobrescribir un paquete existente")
	}
	after, _ := os.ReadFile(filepath.Join(e.out, PackageFile))
	if !bytes.Equal(before, after) {
		t.Error("el paquete anterior cambió")
	}
}

func TestRunFailureLeavesNothing(t *testing.T) {
	e := newEnv(t, map[string]string{"a.txt": "a"})
	_, err := e.run("", "no-existe.txt")
	var se *stage.Error
	if !errors.As(err, &se) || se.Code != stage.CodeNotFound {
		t.Fatalf("error = %v", err)
	}
	if left, _ := os.ReadDir(e.out); len(left) != 0 {
		t.Errorf("quedaron %d archivos en la salida", len(left))
	}
	e.assertCleanStaging()
}
