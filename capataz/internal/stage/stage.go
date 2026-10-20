// Package stage prepara la copia congelada de los elementos de un enlace,
// con el procedimiento validado en la Fase 0 (docs/decisiones.md):
//
//  1. Resolver cada ruta dentro de su carpeta con safepath (os.Root), sin
//     seguir enlaces simbólicos.
//  2. Forzar db/scan?sub= y esperar a que el índice alcance al disco
//     (tamaño y fecha).
//  3. Validar el estado local/global de cada archivo y que no esté en
//     db/need.
//  4. Copiar a staging (copia real), re-verificar el origen después y
//     reintentar hasta 3 veces si cambió. Calcular SHA-256 al copiar.
//
// Ningún error ni línea de bitácora de este paquete contiene rutas, nombres
// de archivo ni contenido: solo códigos, contadores y el número de elemento.
package stage

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/config"
	"github.com/resuelveonline/capataz/capataz/internal/safepath"
	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// Códigos de Error.code del contrato.
const (
	CodeNotFound             = "not_found"
	CodePathNotAllowed       = "path_not_allowed"
	CodeFileNotSynced        = "file_not_synced"
	CodeFileInUse            = "file_in_use"
	CodeFileChanged          = "file_changed_during_copy"
	CodeInvalidRequest       = "invalid_request"
	CodeSyncthingUnavailable = "syncthing_unavailable"
	CodeInternal             = "internal_error"
	CodeInsufficientStorage  = "insufficient_storage"
)

// Error lleva un código del contrato y un motivo genérico. Nunca incluye
// la ruta: Item dice qué elemento seleccionado falló (1, 2, ...).
type Error struct {
	Code   string
	Reason string
	Item   int   // 0 si no corresponde a un elemento
	Err    error // causa interna, también sin rutas
}

func (e *Error) Error() string {
	msg := e.Code + " (" + e.Reason + ")"
	if e.Item > 0 {
		msg = fmt.Sprintf("elemento %d: %s", e.Item, msg)
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func newErr(item int, code, reason string, cause error) *Error {
	return &Error{Code: code, Reason: reason, Item: item, Err: cause}
}

// Syncthing es lo que stage necesita del cliente de Syncthing.
type Syncthing interface {
	Folders(ctx context.Context) ([]syncthing.Folder, error)
	File(ctx context.Context, folder, file string) (syncthing.FileStatus, error)
	Scan(ctx context.Context, folder, sub string) error
	Need(ctx context.Context, folder string) (syncthing.Need, error)
}

// Item es un elemento seleccionado: una ruta dentro de una carpeta.
type Item struct {
	Folder string // id de Syncthing
	Path   string // relativa a la carpeta; "" es la carpeta entera
}

// Options configura un Stager.
type Options struct {
	Syncthing  Syncthing
	StagingDir string
	MaxPayload int64 // para estimar el espacio necesario
	Logger     *slog.Logger

	// Solo pruebas.
	retryDelay time.Duration // espera base entre reintentos
	freeSpace  func(string) (uint64, error)
	afterCopy  func(folderRel string) // se llama tras copiar, antes de re-verificar
}

const (
	maxAttempts  = 3 // copias por archivo
	indexRetries = 3 // reescaneos esperando a que el índice alcance al disco
	orphanAge    = 24 * time.Hour
	spaceMargin  = 64 << 20
	prepPrefix   = "prep-"
	defaultDelay = 200 * time.Millisecond
)

// Stager prepara copias congeladas en StagingDir.
type Stager struct {
	st         Syncthing
	dir        string
	maxPayload int64
	log        *slog.Logger
	delay      time.Duration
	freeSpace  func(string) (uint64, error)
	afterCopy  func(string)
}

// New crea StagingDir (0700) si no existe, exige que no sea accesible por
// otros usuarios y borra preparaciones huérfanas de más de 24 h.
func New(o Options) (*Stager, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.retryDelay == 0 {
		o.retryDelay = defaultDelay
	}
	if o.retryDelay < 0 {
		o.retryDelay = 0
	}
	if o.freeSpace == nil {
		o.freeSpace = freeSpace
	}
	if err := os.MkdirAll(o.StagingDir, 0o700); err != nil {
		return nil, fmt.Errorf("creando el staging: %w", err)
	}
	fi, err := os.Stat(o.StagingDir)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("el staging no debe ser accesible por otros usuarios (permisos 0700)")
	}
	s := &Stager{
		st: o.Syncthing, dir: o.StagingDir, maxPayload: o.MaxPayload, log: o.Logger,
		delay: o.retryDelay, freeSpace: o.freeSpace, afterCopy: o.afterCopy,
	}
	s.removeOrphans()
	return s, nil
}

func (s *Stager) removeOrphans() {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	n := 0
	for _, de := range des {
		if !de.IsDir() || !strings.HasPrefix(de.Name(), prepPrefix) {
			continue
		}
		if fi, err := de.Info(); err == nil && time.Since(fi.ModTime()) > orphanAge {
			if os.RemoveAll(filepath.Join(s.dir, de.Name())) == nil {
				n++
			}
		}
	}
	if n > 0 {
		s.log.Info("preparaciones huérfanas eliminadas", "cantidad", n)
	}
}

// Skipped cuenta lo omitido al recorrer carpetas, por motivo (sin nombres).
type Skipped struct {
	Ignored int `json:"ignored"` // fuera del índice de Syncthing (.stignore)
	Hidden  int `json:"hidden"`  // internos de Syncthing
	Symlink int `json:"symlink"` // enlaces simbólicos
	Special int `json:"special"` // FIFO, sockets, dispositivos
}

// ItemInfo describe un elemento seleccionado.
type ItemInfo struct {
	Name  string // nombre de primer nivel (último segmento, o label para la carpeta entera)
	IsDir bool
}

// Entry es un archivo o carpeta de la copia, en orden de recorrido.
type Entry struct {
	Item    int    // índice en Snapshot.Items
	Path    string // relativa al padre del elemento: "fotos/a/x.jpg" o "informe.pdf"
	IsDir   bool
	Size    int64
	ModTime time.Time

	folder  string // id de Syncthing
	rel     string // ruta dentro de la carpeta sincronizada
	version []string
	seq     int64
}

// Snapshot es una preparación en curso. Close la borra del disco.
type Snapshot struct {
	Items     []ItemInfo
	Entries   []Entry
	Skipped   Skipped
	CreatedAt time.Time // momento de la copia congelada (UTC)

	s       *Stager
	dir     string
	roots   map[string]*safepath.Folder
	windows map[string]time.Duration
	seq     int
}

// Dir es el directorio de esta preparación dentro del staging (0700).
func (sn *Snapshot) Dir() string { return sn.dir }

// Close borra la preparación entera y libera las carpetas abiertas.
func (sn *Snapshot) Close() error {
	sn.closeRoots()
	return os.RemoveAll(sn.dir)
}

func (sn *Snapshot) closeRoots() {
	for _, r := range sn.roots {
		r.Close()
	}
}

// Begin resuelve y valida los elementos y planifica la copia, sin copiar
// todavía. Si devuelve error no deja nada en el staging.
func (s *Stager) Begin(ctx context.Context, items []Item) (_ *Snapshot, err error) {
	if len(items) == 0 {
		return nil, newErr(0, CodeInvalidRequest, "no hay elementos", nil)
	}
	folders, err := s.st.Folders(ctx)
	if err != nil {
		return nil, syncErr(0, err)
	}
	if err := checkOutsideFolders(s.dir, "el staging", folders); err != nil {
		return nil, err
	}

	id := make([]byte, 8)
	rand.Read(id)
	dir := filepath.Join(s.dir, prepPrefix+hex.EncodeToString(id))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, newErr(0, CodeInternal, "no se pudo crear la preparación", err)
	}
	sn := &Snapshot{
		s: s, dir: dir, CreatedAt: time.Now().UTC().Truncate(time.Second),
		roots: map[string]*safepath.Folder{}, windows: map[string]time.Duration{},
	}
	// sn es local (no el valor de retorno): "return nil, err" no lo anula
	// antes de que este defer borre la preparación.
	defer func() {
		if err != nil {
			sn.Close()
		}
	}()

	byID := map[string]syncthing.Folder{}
	for _, f := range folders {
		byID[f.ID] = f
	}
	needs := map[string]*needSets{}
	for i, it := range items {
		n := i + 1
		f, ok := byID[it.Folder]
		if !ok {
			return nil, newErr(n, CodeNotFound, "carpeta desconocida", nil)
		}
		if err := sn.plan(ctx, n, f, it.Path); err != nil {
			return nil, err
		}
		if _, ok := needs[f.ID]; !ok {
			nd, err := s.st.Need(ctx, f.ID)
			if err != nil {
				return nil, syncErr(n, err)
			}
			needs[f.ID] = newNeedSets(nd)
		}
	}

	// Validación contra el índice de cada archivo planificado.
	kept := sn.Entries[:0]
	for _, e := range sn.Entries {
		if e.IsDir {
			kept = append(kept, e)
			continue
		}
		direct := !sn.Items[e.Item].IsDir
		st, verr := sn.validate(ctx, e.Item+1, e.folder, e.rel, needs[e.folder])
		if verr != nil {
			var se *Error
			if !direct && errors.As(verr, &se) && se.Reason == reasonIgnored {
				sn.Skipped.Ignored++
				continue
			}
			return nil, verr
		}
		e.Size, e.ModTime, e.version, e.seq = st.Size, st.Modified, st.Version, st.Sequence
		kept = append(kept, e)
	}
	sn.Entries = kept

	if err := sn.checkSpace(); err != nil {
		return nil, err
	}
	s.log.Info("copia planificada", "elementos", len(items), "archivos", sn.fileCount(),
		"omitidos", sn.Skipped.Ignored+sn.Skipped.Hidden+sn.Skipped.Symlink+sn.Skipped.Special)
	return sn, nil
}

// Check valida rápido los elementos de un enlace antes de aceptarlo: que
// la carpeta la reporte Syncthing y que cada ruta sea admisible, exista y
// sea un archivo o una carpeta, sin seguir enlaces simbólicos. No escanea
// ni copia: el resto lo valida Begin. Devuelve *Error como Begin.
func (s *Stager) Check(ctx context.Context, items []Item) error {
	if len(items) == 0 {
		return newErr(0, CodeInvalidRequest, "no hay elementos", nil)
	}
	folders, err := s.st.Folders(ctx)
	if err != nil {
		return syncErr(0, err)
	}
	byID := map[string]syncthing.Folder{}
	for _, f := range folders {
		byID[f.ID] = f
	}
	sn := &Snapshot{s: s, roots: map[string]*safepath.Folder{}, windows: map[string]time.Duration{}}
	defer sn.closeRoots()
	for i, it := range items {
		n := i + 1
		f, ok := byID[it.Folder]
		if !ok {
			return newErr(n, CodeNotFound, "carpeta desconocida", nil)
		}
		rel, err := safepath.Clean(it.Path)
		if err != nil {
			return newErr(n, CodePathNotAllowed, "ruta no permitida", nil)
		}
		if safepath.HasHidden(rel) {
			return newErr(n, CodeNotFound, "no existe", nil)
		}
		root, err := sn.root(f)
		if err != nil {
			return newErr(n, CodeInternal, "la carpeta no se puede abrir en disco", err)
		}
		fi, err := root.StatNoFollow(rel)
		if err != nil {
			return pathErr(n, err)
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return newErr(n, CodeNotFound, "no es un archivo ni una carpeta", nil)
		}
	}
	return nil
}

// plan resuelve un elemento y añade sus entradas (aún sin validar).
func (sn *Snapshot) plan(ctx context.Context, n int, f syncthing.Folder, p string) error {
	rel, err := safepath.Clean(p)
	if err != nil {
		return newErr(n, CodePathNotAllowed, "ruta no permitida", nil)
	}
	if safepath.HasHidden(rel) {
		return newErr(n, CodeNotFound, "no existe", nil)
	}
	root, err := sn.root(f)
	if err != nil {
		return newErr(n, CodeInternal, "la carpeta no se puede abrir en disco", err)
	}
	sub := rel
	if sub == "." {
		sub = ""
	}
	// Escanear antes de recorrer: con autoNormalize Syncthing puede
	// renombrar en disco nombres mal normalizados durante el escaneo.
	if _, err := root.StatNoFollow(rel); err != nil {
		return pathErr(n, err)
	}
	if err := sn.s.st.Scan(ctx, f.ID, sub); err != nil {
		return syncErr(n, err)
	}
	fi, err := root.StatNoFollow(rel)
	if err != nil {
		return pathErr(n, err)
	}

	name := filepath.Base(rel)
	if rel == "." {
		name = strings.NewReplacer("/", "_", "\\", "_").Replace(cmp.Or(f.Label, f.ID))
	}
	idx := len(sn.Items)
	switch {
	case fi.Mode().IsRegular():
		sn.Items = append(sn.Items, ItemInfo{Name: name})
		sn.Entries = append(sn.Entries, Entry{Item: idx, Path: name, folder: f.ID, rel: rel})
		return nil
	case !fi.IsDir():
		return newErr(n, CodeNotFound, "no es un archivo ni una carpeta", nil)
	}

	sn.Items = append(sn.Items, ItemInfo{Name: name, IsDir: true})
	sn.Entries = append(sn.Entries, Entry{Item: idx, Path: name, IsDir: true, ModTime: fi.ModTime(), folder: f.ID, rel: rel})
	prefix := rel + "/"
	if rel == "." {
		prefix = ""
	}
	err = root.Walk(rel, func(w safepath.WalkEntry) error {
		zp := name + "/" + strings.TrimPrefix(w.Path, prefix)
		switch w.Kind {
		case safepath.KindDir:
			sn.Entries = append(sn.Entries, Entry{Item: idx, Path: zp, IsDir: true, ModTime: w.Info.ModTime(), folder: f.ID, rel: w.Path})
		case safepath.KindFile:
			sn.Entries = append(sn.Entries, Entry{Item: idx, Path: zp, folder: f.ID, rel: w.Path})
		case safepath.KindHidden:
			sn.Skipped.Hidden++
		case safepath.KindSymlink:
			sn.Skipped.Symlink++
		case safepath.KindSpecial:
			sn.Skipped.Special++
		}
		return nil
	})
	if err != nil {
		return pathErr(n, err)
	}
	return nil
}

func (sn *Snapshot) root(f syncthing.Folder) (*safepath.Folder, error) {
	if r, ok := sn.roots[f.ID]; ok {
		return r, nil
	}
	dir, err := config.ExpandHome(f.Path)
	if err != nil {
		return nil, err
	}
	r, err := safepath.OpenFolder(dir)
	if err != nil {
		return nil, err
	}
	sn.roots[f.ID] = r
	sn.windows[f.ID] = time.Duration(f.ModTimeWindowS) * time.Second
	return r, nil
}

const reasonIgnored = "ignorado por Syncthing"

// validate espera a que el índice alcance al disco y comprueba que el
// archivo esté completamente sincronizado. Devuelve la vista local.
func (sn *Snapshot) validate(ctx context.Context, n int, folder, rel string, need *needSets) (syncthing.FileInfo, error) {
	root := sn.roots[folder]
	window := sn.windows[folder]
	for attempt := 0; ; attempt++ {
		disk, err := root.StatNoFollow(rel)
		if err != nil {
			return syncthing.FileInfo{}, pathErr(n, err)
		}
		if !disk.Mode().IsRegular() {
			return syncthing.FileInfo{}, newErr(n, CodeNotFound, "no es un archivo", nil)
		}
		st, err := sn.s.st.File(ctx, folder, rel)
		switch {
		case errors.Is(err, syncthing.ErrNotIndexed):
			// Tras un escaneo, un archivo que existe y no está en el índice
			// es uno que Syncthing ignora (.stignore).
			if attempt >= indexRetries {
				return syncthing.FileInfo{}, newErr(n, CodeFileNotSynced, reasonIgnored, nil)
			}
		case err != nil:
			return syncthing.FileInfo{}, syncErr(n, err)
		default:
			l, g := st.Local, st.Global
			if l.Ignored {
				return syncthing.FileInfo{}, newErr(n, CodeFileNotSynced, reasonIgnored, nil)
			}
			if l.Size == disk.Size() && sameTime(l.Modified, disk.ModTime(), window) {
				return l, checkState(n, rel, l, g, need)
			}
			if attempt >= indexRetries {
				return syncthing.FileInfo{}, newErr(n, CodeFileNotSynced, "el índice no alcanza al disco", nil)
			}
		}
		// El índice va por detrás: reescanear este archivo y esperar.
		if err := sleep(ctx, sn.s.delay<<attempt); err != nil {
			return syncthing.FileInfo{}, err
		}
		if err := sn.s.st.Scan(ctx, folder, rel); err != nil {
			return syncthing.FileInfo{}, syncErr(n, err)
		}
	}
}

func checkState(n int, rel string, l, g syncthing.FileInfo, need *needSets) error {
	switch {
	case need.progress[rel]:
		return newErr(n, CodeFileInUse, "Syncthing lo está descargando", nil)
	case need.pending[rel]:
		return newErr(n, CodeFileNotSynced, "pendiente de descargar", nil)
	case l.Deleted || g.Deleted:
		return newErr(n, CodeFileNotSynced, "borrado en el índice", nil)
	case l.Invalid, l.MustRescan:
		return newErr(n, CodeFileNotSynced, "inválido o pendiente de reescaneo", nil)
	case l.LocalFlags != 0:
		return newErr(n, CodeFileNotSynced, "con banderas locales", nil)
	case l.Type != "" && l.Type != "FILE_INFO_TYPE_FILE":
		return newErr(n, CodeFileNotSynced, "no es un archivo en el índice", nil)
	case !slices.Equal(l.Version, g.Version):
		return newErr(n, CodeFileNotSynced, "versión local distinta de la global", nil)
	}
	return nil
}

// StagedFile es una copia congelada en el staging.
type StagedFile struct {
	Path    string // dentro de Snapshot.Dir; el nombre es un número, no el original
	Size    int64
	ModTime time.Time
	SHA256  [32]byte
}

// Freeze copia la entrada i al staging y verifica que el origen no cambió
// durante la copia; si cambió, reintenta hasta 3 veces y después devuelve
// file_changed_during_copy. Quien llama borra la copia cuando ya no la
// necesita (o Close lo hace todo).
func (sn *Snapshot) Freeze(ctx context.Context, i int) (StagedFile, error) {
	e := &sn.Entries[i]
	n := e.Item + 1
	if e.IsDir {
		return StagedFile{}, newErr(n, CodeInternal, "Freeze sobre una carpeta", nil)
	}
	for attempt := 1; ; attempt++ {
		sf, changed, err := sn.copyOnce(ctx, e)
		if err != nil {
			return StagedFile{}, err
		}
		if !changed {
			return sf, nil
		}
		sn.s.log.Info("el origen cambió durante la copia; reintento", "elemento", n, "intento", attempt)
		if attempt == maxAttempts {
			return StagedFile{}, newErr(n, CodeFileChanged, "cambió durante la copia", nil)
		}
		// Revalidar: el índice debe alcanzar la nueva versión.
		if err := sleep(ctx, sn.s.delay); err != nil {
			return StagedFile{}, err
		}
		if err := sn.s.st.Scan(ctx, e.folder, e.rel); err != nil {
			return StagedFile{}, syncErr(n, err)
		}
		nd, err := sn.s.st.Need(ctx, e.folder)
		if err != nil {
			return StagedFile{}, syncErr(n, err)
		}
		st, err := sn.validate(ctx, n, e.folder, e.rel, newNeedSets(nd))
		if err != nil {
			return StagedFile{}, err
		}
		e.Size, e.ModTime, e.version, e.seq = st.Size, st.Modified, st.Version, st.Sequence
	}
}

func (sn *Snapshot) fileCount() int {
	n := 0
	for _, e := range sn.Entries {
		if !e.IsDir {
			n++
		}
	}
	return n
}

// CheckOutside exige que dir (que debe existir) esté fuera de toda carpeta
// sincronizada. what nombra el directorio en el mensaje ("la salida").
func (s *Stager) CheckOutside(ctx context.Context, dir, what string) error {
	folders, err := s.st.Folders(ctx)
	if err != nil {
		return syncErr(0, err)
	}
	return checkOutsideFolders(dir, what, folders)
}

// checkOutsideFolders exige que dir no esté dentro de ninguna carpeta
// sincronizada (ni al revés), comparando rutas reales.
func checkOutsideFolders(dir, what string, folders []syncthing.Folder) error {
	staging, err := realPath(dir)
	if err != nil {
		return newErr(0, CodeInternal, what+" no es accesible", err)
	}
	for _, f := range folders {
		p, err := config.ExpandHome(f.Path)
		if err != nil {
			continue
		}
		fp, err := realPath(p)
		if err != nil {
			continue // carpeta sin disco: se detecta al usarla
		}
		if within(staging, fp) || within(fp, staging) {
			return newErr(0, CodeInternal, what+" está dentro de una carpeta sincronizada", nil)
		}
	}
	return nil
}

// checkSpace exige espacio para el archivo más grande, la carga útil y el
// paquete cifrado, con margen: max + 2*min(total, tope) + 64 MiB.
func (sn *Snapshot) checkSpace() error {
	var total, largest int64
	for _, e := range sn.Entries {
		total += e.Size
		largest = max(largest, e.Size)
	}
	need := uint64(largest) + 2*uint64(min(total, sn.s.maxPayload)) + spaceMargin
	free, err := sn.s.freeSpace(sn.s.dir)
	if err != nil {
		return newErr(0, CodeInternal, "no se pudo medir el espacio libre", err)
	}
	if free < need {
		return newErr(0, CodeInsufficientStorage, "espacio insuficiente en el staging", nil)
	}
	return nil
}

func freeSpace(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil // Bsize es uint32 en macOS e int64 en Linux
}

type needSets struct {
	progress map[string]bool // descargándose ahora
	pending  map[string]bool // en cola o pendiente
}

func newNeedSets(nd syncthing.Need) *needSets {
	s := &needSets{progress: map[string]bool{}, pending: map[string]bool{}}
	for _, f := range nd.Progress {
		s.progress[f.Name] = true
	}
	for _, list := range [][]syncthing.FileInfo{nd.Queued, nd.Rest} {
		for _, f := range list {
			s.pending[f.Name] = true
		}
	}
	return s
}

func pathErr(n int, err error) error {
	switch {
	case errors.Is(err, safepath.ErrSymlink), errors.Is(err, safepath.ErrNotAllowed):
		return newErr(n, CodePathNotAllowed, "ruta no permitida o enlace simbólico", nil)
	case errors.Is(err, safepath.ErrNotFound):
		return newErr(n, CodeNotFound, "no existe", nil)
	case errors.Is(err, safepath.ErrNotDir), errors.Is(err, safepath.ErrNotRegular):
		return newErr(n, CodeNotFound, "no es un archivo ni una carpeta", nil)
	}
	return newErr(n, CodeInternal, "error de disco", err)
}

func syncErr(n int, err error) error {
	switch {
	case errors.Is(err, syncthing.ErrNoSuchFolder):
		return newErr(n, CodeNotFound, "carpeta desconocida para Syncthing", nil)
	case errors.Is(err, syncthing.ErrUnavailable):
		return newErr(n, CodeSyncthingUnavailable, "no se pudo consultar Syncthing", err)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	}
	return newErr(n, CodeInternal, "respuesta inesperada de Syncthing", err)
}

func sameTime(a, b time.Time, window time.Duration) bool {
	d := a.Sub(b)
	return d == 0 || (d < 0 && -d <= window) || (d > 0 && d <= window)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// within indica si child está dentro de parent (o es la misma ruta).
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
