package stage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

const folderID = "fgyww-edjwa"

// fakeST imita Syncthing: el índice es un mapa que las pruebas llenan a
// partir del disco y pueden alterar.
type fakeST struct {
	mu        sync.Mutex
	folder    syncthing.Folder
	index     map[string]syncthing.FileStatus
	need      syncthing.Need
	scans     []string
	foldersEr error
	fileHook  func(rel string, call int) (syncthing.FileStatus, error, bool)
	calls     map[string]int
}

func (f *fakeST) Folders(context.Context) ([]syncthing.Folder, error) {
	return []syncthing.Folder{f.folder}, f.foldersEr
}

func (f *fakeST) File(_ context.Context, folder, rel string) (syncthing.FileStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[rel]++
	if f.fileHook != nil {
		if st, err, ok := f.fileHook(rel, f.calls[rel]); ok {
			return st, err
		}
	}
	st, ok := f.index[rel]
	if !ok {
		return st, syncthing.ErrNotIndexed
	}
	return st, nil
}

func (f *fakeST) Scan(_ context.Context, folder, sub string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans = append(f.scans, sub)
	return nil
}

func (f *fakeST) Need(context.Context, string) (syncthing.Need, error) { return f.need, nil }

// indexFile pone en el índice el estado actual del disco (versión v).
func (f *fakeST) indexFile(t *testing.T, rel string, v int) {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(f.folder.Path, rel))
	if err != nil {
		t.Fatal(err)
	}
	info := syncthing.FileInfo{
		Name: rel, Size: fi.Size(), Modified: fi.ModTime(), Type: "FILE_INFO_TYPE_FILE",
		Version: []string{fmt.Sprintf("ABC:%d", v)}, Sequence: int64(v),
	}
	f.mu.Lock()
	f.index[rel] = syncthing.FileStatus{Local: info, Global: info}
	f.mu.Unlock()
}

type env struct {
	t       *testing.T
	st      *fakeST
	dir     string // carpeta sincronizada
	staging string
	logs    *bytes.Buffer
	opts    Options
}

// newEnv crea:
//
//	base/pruebas/
//	  informe.pdf  expediente-confidencial.txt  .stversions/v.txt  fifo-raiz
//	  enlace-informe -> informe.pdf     dir-enlace -> fotos
//	  fotos/a.jpg  fotos/sub/b.txt  fotos/vacia/  fotos/.stversions/x
//	  fotos/enlace -> a.jpg  fotos/fifo  fotos/ignorado.tmp (fuera del índice)
//	base/staging/ (0700)
func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "pruebas")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"fotos/sub", "fotos/vacia", "fotos/.stversions", ".stversions"} {
		must(os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	for name, content := range map[string]string{
		"informe.pdf":                 "%PDF-1.7 informe",
		"expediente-confidencial.txt": "contenido-secreto-del-expediente",
		".stversions/v.txt":           "v",
		"fotos/a.jpg":                 "JPEG",
		"fotos/sub/b.txt":             "bbb",
		"fotos/.stversions/x":         "x",
		"fotos/ignorado.tmp":          "ignorado",
	} {
		must(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	}
	must(os.Symlink("informe.pdf", filepath.Join(dir, "enlace-informe")))
	must(os.Symlink("fotos", filepath.Join(dir, "dir-enlace")))
	must(os.Symlink("a.jpg", filepath.Join(dir, "fotos/enlace")))
	must(syscall.Mkfifo(filepath.Join(dir, "fotos/fifo"), 0o644))
	must(syscall.Mkfifo(filepath.Join(dir, "fifo-raiz"), 0o644))

	st := &fakeST{
		folder: syncthing.Folder{ID: folderID, Label: "pruebas", Path: dir},
		index:  map[string]syncthing.FileStatus{},
		calls:  map[string]int{},
	}
	for _, rel := range []string{"informe.pdf", "expediente-confidencial.txt", "fotos/a.jpg", "fotos/sub/b.txt"} {
		st.indexFile(t, rel, 1)
	}
	logs := &bytes.Buffer{}
	e := &env{t: t, st: st, dir: dir, staging: filepath.Join(base, "staging"), logs: logs}
	e.opts = Options{
		Syncthing: st, StagingDir: e.staging, MaxPayload: 500 << 20,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		retryDelay: -1, // sin esperas en pruebas
		freeSpace:  func(string) (uint64, error) { return 1 << 40, nil },
	}
	return e
}

func (e *env) stager() *Stager {
	e.t.Helper()
	s, err := New(e.opts)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) begin(paths ...string) (*Snapshot, error) {
	var items []Item
	for _, p := range paths {
		items = append(items, Item{Folder: folderID, Path: p})
	}
	return e.stager().Begin(context.Background(), items)
}

// prepDirs devuelve las preparaciones que quedaron en el staging.
func (e *env) prepDirs() []string {
	des, _ := os.ReadDir(e.staging)
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var se *Error
	if !errors.As(err, &se) || se.Code != code {
		t.Fatalf("error = %v; quiero código %s", err, code)
	}
}

func TestFreezeSingleFile(t *testing.T) {
	e := newEnv(t)
	sn, err := e.begin("informe.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if len(sn.Items) != 1 || sn.Items[0] != (ItemInfo{Name: "informe.pdf"}) || len(sn.Entries) != 1 {
		t.Fatalf("plan: %+v %+v", sn.Items, sn.Entries)
	}
	if !slices.Equal(e.st.scans, []string{"informe.pdf"}) {
		t.Errorf("escaneos %v; quiero [informe.pdf]", e.st.scans)
	}

	sf, err := sn.Freeze(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(e.dir, "informe.pdf")
	want, _ := os.ReadFile(orig)
	got, _ := os.ReadFile(sf.Path)
	if !bytes.Equal(got, want) || sf.Size != int64(len(want)) || sf.SHA256 != sha256.Sum256(want) {
		t.Error("la copia no coincide con el original")
	}
	if !strings.HasPrefix(sf.Path, sn.Dir()) || strings.Contains(sf.Path, "informe") {
		t.Errorf("la copia debe ir en la preparación y sin el nombre original: %s", sf.Path)
	}
	cfi, _ := os.Stat(sf.Path)
	ofi, _ := os.Stat(orig)
	if os.SameFile(cfi, ofi) {
		t.Error("la copia es un enlace duro del original")
	}
	if cfi.Mode().Perm() != 0o600 || !cfi.ModTime().Equal(ofi.ModTime()) {
		t.Errorf("permisos %o, fecha %v (original %v)", cfi.Mode().Perm(), cfi.ModTime(), ofi.ModTime())
	}
	if pfi, _ := os.Stat(sn.Dir()); pfi.Mode().Perm() != 0o700 {
		t.Errorf("la preparación tiene permisos %o", pfi.Mode().Perm())
	}

	if err := sn.Close(); err != nil {
		t.Fatal(err)
	}
	if left := e.prepDirs(); len(left) != 0 {
		t.Errorf("Close dejó %v", left)
	}
}

func TestPlanFolder(t *testing.T) {
	e := newEnv(t)
	sn, err := e.begin("fotos")
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()

	var got []string
	for _, en := range sn.Entries {
		k := "F"
		if en.IsDir {
			k = "D"
		}
		got = append(got, k+" "+en.Path)
	}
	want := []string{"D fotos", "F fotos/a.jpg", "D fotos/sub", "F fotos/sub/b.txt", "D fotos/vacia"}
	if !slices.Equal(got, want) {
		t.Errorf("entradas %v\n quiero %v", got, want)
	}
	if sn.Skipped != (Skipped{Ignored: 1, Hidden: 1, Symlink: 1, Special: 1}) {
		t.Errorf("omitidos %+v", sn.Skipped)
	}
	if e.st.scans[0] != "fotos" {
		t.Errorf("el primer escaneo debe ser sub=fotos: %v", e.st.scans)
	}

	// Todas las copias de la carpeta.
	for i, en := range sn.Entries {
		if en.IsDir {
			continue
		}
		sf, err := sn.Freeze(context.Background(), i)
		if err != nil {
			t.Fatalf("%s: %v", en.Path, err)
		}
		os.Remove(sf.Path)
	}
}

func TestPlanWholeFolder(t *testing.T) {
	e := newEnv(t)
	sn, err := e.begin("")
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	if sn.Items[0] != (ItemInfo{Name: "pruebas", IsDir: true}) {
		t.Errorf("elemento %+v; la raíz usa el label", sn.Items[0])
	}
	if sn.Entries[0].Path != "pruebas" || !slices.ContainsFunc(sn.Entries, func(en Entry) bool { return en.Path == "pruebas/fotos/sub/b.txt" }) {
		t.Errorf("entradas %+v", sn.Entries)
	}
	// .stversions (2), enlaces (3), FIFOs (2), ignorado (1).
	if sn.Skipped != (Skipped{Ignored: 1, Hidden: 2, Symlink: 3, Special: 2}) {
		t.Errorf("omitidos %+v", sn.Skipped)
	}
	if e.st.scans[0] != "" {
		t.Errorf("la carpeta entera se escanea sin sub: %v", e.st.scans)
	}
}

func TestBeginErrors(t *testing.T) {
	setIndex := func(rel string, f func(*syncthing.FileStatus)) func(*env) {
		return func(e *env) {
			st := e.st.index[rel]
			f(&st)
			e.st.index[rel] = st
		}
	}
	tests := []struct {
		name  string
		path  string
		setup func(*env)
		code  string
	}{
		{"enlace seleccionado", "enlace-informe", nil, CodePathNotAllowed},
		{"a través de un enlace a carpeta", "dir-enlace/a.jpg", nil, CodePathNotAllowed},
		{"..", "../staging", nil, CodePathNotAllowed},
		{"absoluta", "/etc/hosts", nil, CodePathNotAllowed},
		{"byte nulo", "informe.pdf\x00", nil, CodePathNotAllowed},
		{"oculto", ".stversions/v.txt", nil, CodeNotFound},
		{"oculto en mayúsculas", ".STVERSIONS", nil, CodeNotFound},
		{"no existe", "no-existe.txt", nil, CodeNotFound},
		{"fifo seleccionado", "fifo-raiz", nil, CodeNotFound},
		{"no indexado (seleccionado directamente)", "fotos/ignorado.tmp", nil, CodeFileNotSynced},
		{"ignorado en el índice", "informe.pdf", setIndex("informe.pdf", func(s *syncthing.FileStatus) { s.Local.Ignored = true }), CodeFileNotSynced},
		{"borrado", "informe.pdf", setIndex("informe.pdf", func(s *syncthing.FileStatus) { s.Global.Deleted = true }), CodeFileNotSynced},
		{"inválido", "informe.pdf", setIndex("informe.pdf", func(s *syncthing.FileStatus) { s.Local.Invalid = true }), CodeFileNotSynced},
		{"mustRescan", "informe.pdf", setIndex("informe.pdf", func(s *syncthing.FileStatus) { s.Local.MustRescan = true }), CodeFileNotSynced},
		{"localFlags", "informe.pdf", setIndex("informe.pdf", func(s *syncthing.FileStatus) { s.Local.LocalFlags = 8 }), CodeFileNotSynced},
		{"tipo carpeta en el índice", "informe.pdf", setIndex("informe.pdf", func(s *syncthing.FileStatus) { s.Local.Type = "FILE_INFO_TYPE_DIRECTORY" }), CodeFileNotSynced},
		{"versión local distinta de la global", "informe.pdf", setIndex("informe.pdf", func(s *syncthing.FileStatus) { s.Global.Version = []string{"XYZ:9"} }), CodeFileNotSynced},
		{"descargándose (db/need progress)", "informe.pdf", func(e *env) { e.st.need.Progress = []syncthing.FileInfo{{Name: "informe.pdf"}} }, CodeFileInUse},
		{"en cola (db/need queued)", "informe.pdf", func(e *env) { e.st.need.Queued = []syncthing.FileInfo{{Name: "informe.pdf"}} }, CodeFileNotSynced},
		{"pendiente (db/need rest)", "fotos", func(e *env) { e.st.need.Rest = []syncthing.FileInfo{{Name: "fotos/sub/b.txt"}} }, CodeFileNotSynced},
		{"un archivo de la carpeta desincronizado", "fotos", setIndex("fotos/a.jpg", func(s *syncthing.FileStatus) { s.Local.Version = []string{"ABC:0"} }), CodeFileNotSynced},
		{"syncthing no disponible", "informe.pdf", func(e *env) { e.st.foldersEr = syncthing.ErrUnavailable }, CodeSyncthingUnavailable},
		{"sin espacio", "informe.pdf", func(e *env) { e.opts.freeSpace = func(string) (uint64, error) { return 1 << 20, nil } }, CodeInsufficientStorage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.setup != nil {
				tt.setup(e)
			}
			_, err := e.begin(tt.path)
			wantCode(t, err, tt.code)
			if left := e.prepDirs(); len(left) != 0 {
				t.Errorf("un error no debe dejar nada en el staging: %v", left)
			}
		})
	}

	t.Run("carpeta desconocida", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.stager().Begin(context.Background(), []Item{{Folder: "no-existe", Path: "x"}})
		wantCode(t, err, CodeNotFound)
	})
	t.Run("sin elementos", func(t *testing.T) {
		_, err := newEnv(t).stager().Begin(context.Background(), nil)
		wantCode(t, err, CodeInvalidRequest)
	})
	t.Run("número de elemento", func(t *testing.T) {
		_, err := newEnv(t).begin("informe.pdf", "fotos", "no-existe")
		var se *Error
		if !errors.As(err, &se) || se.Item != 3 || !strings.HasPrefix(err.Error(), "elemento 3: not_found") {
			t.Errorf("error = %v", err)
		}
	})
}

// El índice va por detrás del disco: los dos primeros db/file dan un
// tamaño viejo; Capataz reescanea el archivo y espera.
func TestIndexCatchesUp(t *testing.T) {
	e := newEnv(t)
	e.st.fileHook = func(rel string, call int) (syncthing.FileStatus, error, bool) {
		if rel == "informe.pdf" && call <= 2 {
			st := e.st.index[rel]
			st.Local.Size = 3
			return st, nil, true
		}
		return syncthing.FileStatus{}, nil, false
	}
	sn, err := e.begin("informe.pdf")
	if err != nil {
		t.Fatal(err)
	}
	sn.Close()
	if !slices.Equal(e.st.scans, []string{"informe.pdf", "informe.pdf", "informe.pdf"}) {
		t.Errorf("escaneos %v; quiero el inicial y dos reescaneos", e.st.scans)
	}
}

func TestIndexNeverCatchesUp(t *testing.T) {
	e := newEnv(t)
	st := e.st.index["informe.pdf"]
	st.Local.Modified = st.Local.Modified.Add(-time.Hour)
	e.st.index["informe.pdf"] = st
	_, err := e.begin("informe.pdf")
	wantCode(t, err, CodeFileNotSynced)
	if len(e.st.scans) != 1+indexRetries {
		t.Errorf("%d escaneos; quiero %d", len(e.st.scans), 1+indexRetries)
	}
}

// Tolerancia de modTimeWindowS (FAT/exFAT).
func TestModTimeWindow(t *testing.T) {
	e := newEnv(t)
	e.st.folder.ModTimeWindowS = 2
	st := e.st.index["informe.pdf"]
	st.Local.Modified = st.Local.Modified.Add(-1500 * time.Millisecond)
	e.st.index["informe.pdf"] = st
	sn, err := e.begin("informe.pdf")
	if err != nil {
		t.Fatalf("dentro de la ventana de 2 s: %v", err)
	}
	sn.Close()
}

// Cambia una vez durante la copia: el segundo intento copia la versión nueva.
func TestFreezeChangedOnce(t *testing.T) {
	e := newEnv(t)
	changes := 0
	e.opts.afterCopy = func(rel string) {
		if changes == 0 {
			changes++
			p := filepath.Join(e.dir, rel)
			os.WriteFile(p, []byte("versión nueva, más larga"), 0o644)
			future := time.Now().Add(time.Minute)
			os.Chtimes(p, future, future)
			e.st.indexFile(t, rel, 2)
		}
	}
	sn, err := e.begin("informe.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	sf, err := sn.Freeze(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(sf.Path); string(got) != "versión nueva, más larga" {
		t.Errorf("copia %q; quiero la versión nueva", got)
	}
	if files, _ := os.ReadDir(sn.Dir()); len(files) != 1 {
		t.Errorf("quedaron %d copias; la del intento fallido debe borrarse", len(files))
	}
}

func TestFreezeAlwaysChanging(t *testing.T) {
	e := newEnv(t)
	v := 1
	e.opts.afterCopy = func(rel string) {
		v++
		p := filepath.Join(e.dir, rel)
		os.WriteFile(p, []byte(strings.Repeat("x", v)), 0o644)
		e.st.indexFile(t, rel, v)
	}
	sn, err := e.begin("informe.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	_, err = sn.Freeze(context.Background(), 0)
	wantCode(t, err, CodeFileChanged)
	if v != 1+maxAttempts {
		t.Errorf("%d intentos; quiero %d", v-1, maxAttempts)
	}
	if files, _ := os.ReadDir(sn.Dir()); len(files) != 0 {
		t.Errorf("quedaron %d copias tras el fallo", len(files))
	}
}

// Solo cambia el índice (otra versión), no el disco: también es un cambio.
func TestFreezeIndexVersionChanged(t *testing.T) {
	e := newEnv(t)
	e.opts.afterCopy = func(rel string) {
		st := e.st.index[rel]
		st.Local.Sequence++
		e.st.index[rel] = st
	}
	sn, err := e.begin("informe.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Close()
	_, err = sn.Freeze(context.Background(), 0)
	wantCode(t, err, CodeFileChanged)
}

func TestStagingInsideSyncedFolder(t *testing.T) {
	e := newEnv(t)
	e.opts.StagingDir = filepath.Join(e.dir, "fotos", "staging")
	_, err := e.begin("informe.pdf")
	wantCode(t, err, CodeInternal)
	if !strings.Contains(err.Error(), "staging está dentro de una carpeta sincronizada") {
		t.Errorf("error = %v", err)
	}
}

func TestStagingPermissions(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(e.staging, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(e.staging, 0o755)
	if _, err := New(e.opts); err == nil {
		t.Error("un staging 0755 debe rechazarse")
	}
}

func TestRemoveOrphans(t *testing.T) {
	e := newEnv(t)
	os.MkdirAll(e.staging, 0o700)
	old := time.Now().Add(-48 * time.Hour)
	for _, d := range []string{"prep-vieja", "prep-reciente", "otra-cosa"} {
		os.Mkdir(filepath.Join(e.staging, d), 0o700)
	}
	os.WriteFile(filepath.Join(e.staging, "prep-vieja", "000001"), []byte("x"), 0o600)
	os.Chtimes(filepath.Join(e.staging, "prep-vieja"), old, old)
	os.Chtimes(filepath.Join(e.staging, "otra-cosa"), old, old)
	e.stager()
	if got := e.prepDirs(); !slices.Equal(got, []string{"otra-cosa", "prep-reciente"}) {
		t.Errorf("quedaron %v", got)
	}
}

// Ni los errores ni la bitácora contienen nombres, rutas ni contenido.
func TestNoNamesInErrorsOrLogs(t *testing.T) {
	e := newEnv(t)
	var msgs []string
	collect := func(err error) {
		if err != nil {
			msgs = append(msgs, err.Error(), fmt.Sprintf("%+v", err))
			if u := errors.Unwrap(err); u != nil {
				msgs = append(msgs, u.Error())
			}
		}
	}

	sn, err := e.begin("expediente-confidencial.txt")
	collect(err)
	if err == nil {
		_, err = sn.Freeze(context.Background(), 0)
		collect(err)
		sn.Close()
	}
	for _, p := range []string{"expediente-confidencial.txt/x", "../expediente-confidencial.txt", "no-existe/expediente-confidencial.txt"} {
		_, err := e.begin(p)
		collect(err)
	}
	e.st.index["expediente-confidencial.txt"] = syncthing.FileStatus{Local: syncthing.FileInfo{Name: "expediente-confidencial.txt", Deleted: true}}
	_, err = e.begin("expediente-confidencial.txt")
	collect(err)
	e.st.foldersEr = fmt.Errorf("syncthing config/folders: %w (connection refused)", syncthing.ErrUnavailable)
	_, err = e.begin("expediente-confidencial.txt")
	collect(err)

	if len(msgs) < 5 {
		t.Fatalf("se esperaban varios errores, hubo %d", len(msgs))
	}
	all := strings.Join(msgs, "\n") + "\n" + e.logs.String()
	for _, leak := range []string{"expediente", "confidencial", "contenido-secreto", e.dir, "pruebas/"} {
		if strings.Contains(all, leak) {
			t.Errorf("se filtra %q:\n%s", leak, all)
		}
	}
}

func TestCheck(t *testing.T) {
	tests := []struct {
		path string
		code string // "" = aceptado
	}{
		{"informe.pdf", ""},
		{"fotos", ""},
		{"", ""},                   // la carpeta entera
		{"fotos/ignorado.tmp", ""}, // fuera del índice: lo detecta Begin, no Check
		{"enlace-informe", CodePathNotAllowed},
		{"dir-enlace/a.jpg", CodePathNotAllowed},
		{"../staging", CodePathNotAllowed},
		{"/etc/hosts", CodePathNotAllowed},
		{".stversions/v.txt", CodeNotFound},
		{"no-existe.txt", CodeNotFound},
		{"fifo-raiz", CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			e := newEnv(t)
			err := e.stager().Check(context.Background(), []Item{{Folder: folderID, Path: tt.path}})
			if tt.code == "" {
				if err != nil {
					t.Fatalf("rechazado: %v", err)
				}
			} else {
				wantCode(t, err, tt.code)
			}
			if len(e.st.scans) != 0 {
				t.Error("Check no debe escanear")
			}
			if left := e.prepDirs(); len(left) != 0 {
				t.Errorf("Check no debe tocar el staging: %v", left)
			}
		})
	}
	e := newEnv(t)
	wantCode(t, e.stager().Check(context.Background(), []Item{{Folder: "no-existe", Path: "x"}}), CodeNotFound)
	wantCode(t, e.stager().Check(context.Background(), nil), CodeInvalidRequest)
	e.st.foldersEr = syncthing.ErrUnavailable
	wantCode(t, e.stager().Check(context.Background(), []Item{{Folder: folderID, Path: "informe.pdf"}}), CodeSyncthingUnavailable)
}
