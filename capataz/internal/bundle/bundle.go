// Package bundle convierte una copia congelada (stage) en la carga útil que
// se cifra: el archivo tal cual si se eligió uno solo, o un ZIP si se
// eligieron varios elementos o una carpeta (docs/formato-paquete-cifrado.md
// §5). También arma el JSON de metadatos de §6.2.
package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/resuelveonline/capataz/capataz/internal/cpk"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
)

const (
	// CodePayloadTooLarge es el código del contrato para el tope.
	CodePayloadTooLarge = "payload_too_large"

	KindFile = "file"
	KindZip  = "zip"

	maxNameRunes  = 200 // §6.2
	maxLabelRunes = 120 // LinkRequest.label en el contrato de Capataz
	maxEntries    = 500 // §6.2
)

// Options configura Build.
type Options struct {
	MaxPayload int64     // por defecto cpk.MaxPayload
	Label      string    // etiqueta opcional del titular
	Now        time.Time // fecha local para el nombre "archivos-AAAAMMDD.zip"
}

// Payload es la carga útil lista para cifrar.
type Payload struct {
	Kind   string
	Path   string // archivo en el staging (dentro de Snapshot.Dir)
	Size   int64
	SHA256 [32]byte
	Meta   Meta
}

// Meta es el JSON de metadatos de §6.2. Los punteros distinguen "ausente"
// de cero: entryCount y entries existen siempre en un zip, aunque estén
// vacíos, y nunca en un file.
type Meta struct {
	V                int          `json:"v"`
	Kind             string       `json:"kind"`
	Name             string       `json:"name"`
	Size             int64        `json:"size"`
	Mime             string       `json:"mime,omitempty"`
	CreatedAt        string       `json:"createdAt"`
	Label            string       `json:"label,omitempty"`
	EntryCount       *int         `json:"entryCount,omitempty"`
	Entries          *[]MetaEntry `json:"entries,omitempty"`
	EntriesTruncated *bool        `json:"entriesTruncated,omitempty"`
}

// MetaEntry es un archivo del ZIP (solo archivos: las carpetas se deducen
// de las rutas).
type MetaEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func tooLarge(reason string) error {
	return &stage.Error{Code: CodePayloadTooLarge, Reason: reason}
}

// Build arma la carga útil. Para un ZIP, cada copia del staging se borra
// en cuanto se añade, así que en disco solo conviven el ZIP y una copia.
func Build(ctx context.Context, sn *stage.Snapshot, o Options) (*Payload, error) {
	if o.MaxPayload == 0 {
		o.MaxPayload = cpk.MaxPayload
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	label := truncateRunes(cleanName(o.Label), maxLabelRunes)

	if len(sn.Items) == 1 && !sn.Items[0].IsDir {
		return buildFile(ctx, sn, o, label)
	}
	return buildZip(ctx, sn, o, label)
}

func buildFile(ctx context.Context, sn *stage.Snapshot, o Options, label string) (*Payload, error) {
	e := sn.Entries[0]
	if e.Size > o.MaxPayload {
		return nil, tooLarge("el archivo supera el tope por enlace")
	}
	sf, err := sn.Freeze(ctx, 0)
	if err != nil {
		return nil, err
	}
	if sf.Size > o.MaxPayload { // creció entre la planificación y la copia
		os.Remove(sf.Path)
		return nil, tooLarge("el archivo supera el tope por enlace")
	}
	name := fitName(cleanName(sn.Items[0].Name), "archivo")
	return &Payload{
		Kind: KindFile, Path: sf.Path, Size: sf.Size, SHA256: sf.SHA256,
		Meta: Meta{
			V: 1, Kind: KindFile, Name: name, Size: sf.Size, Mime: mimeOf(name),
			CreatedAt: sn.CreatedAt.Format(time.RFC3339), Label: label,
		},
	}, nil
}

func buildZip(ctx context.Context, sn *stage.Snapshot, o Options, label string) (_ *Payload, err error) {
	tops := topLevelNames(sn.Items)
	zipPath := func(e stage.Entry) string {
		_, rest, _ := strings.Cut(e.Path, "/")
		if rest == "" {
			return tops[e.Item]
		}
		return tops[e.Item] + "/" + rest
	}

	// Cota inferior del ZIP antes de copiar nada: los archivos sin
	// comprimir cuentan entero y cada entrada lleva sus cabeceras. Si ya
	// supera el tope, no puede caber.
	var lower int64 = 22 // fin del directorio central
	for _, e := range sn.Entries {
		name := zipPath(e)
		if err := checkZipPath(name); err != nil {
			return nil, err
		}
		lower += 30 + 46 + 2*int64(len(name)+1)
		if !e.IsDir && compressed(name) {
			lower += e.Size // irá sin comprimir: ocupa al menos su tamaño
		}
	}
	if lower > o.MaxPayload {
		return nil, tooLarge("el contenido no cabe en el tope por enlace")
	}

	out := filepath.Join(sn.Dir(), "payload.zip")
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, &stage.Error{Code: stage.CodeInternal, Reason: "no se pudo crear el ZIP", Err: err}
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(out)
		}
	}()

	h := sha256.New()
	cw := &capWriter{w: io.MultiWriter(f, h), limit: o.MaxPayload}
	zw := zip.NewWriter(cw)

	var entries []MetaEntry
	for i, e := range sn.Entries {
		name := zipPath(e)
		if e.IsDir {
			_, err = zw.CreateHeader(&zip.FileHeader{Name: name + "/", Method: zip.Store, Modified: e.ModTime.Local()})
			if err != nil {
				return nil, zipErr(err)
			}
			continue
		}
		if err := addFile(ctx, sn, i, zw, name); err != nil {
			return nil, zipErr(err)
		}
		entries = append(entries, MetaEntry{Path: name, Size: sn.Entries[i].Size})
	}
	if err := zw.Close(); err != nil {
		return nil, zipErr(err)
	}
	if err := f.Sync(); err != nil {
		return nil, zipErr(err)
	}
	if err := f.Close(); err != nil {
		return nil, zipErr(err)
	}

	name := zipName(sn.Items, label, o.Now)
	count := len(entries)
	p := &Payload{
		Kind: KindZip, Path: out, Size: cw.n,
		Meta: Meta{
			V: 1, Kind: KindZip, Name: name, Size: cw.n,
			CreatedAt: sn.CreatedAt.Format(time.RFC3339), Label: label, EntryCount: &count,
		},
	}
	copy(p.SHA256[:], h.Sum(nil))
	p.Meta.setEntries(entries, maxEntries)
	return p, nil
}

// addFile congela la entrada i, la añade al ZIP y borra la copia.
func addFile(ctx context.Context, sn *stage.Snapshot, i int, zw *zip.Writer, name string) error {
	sf, err := sn.Freeze(ctx, i)
	if err != nil {
		return err
	}
	defer os.Remove(sf.Path)
	src, err := os.Open(sf.Path)
	if err != nil {
		return err
	}
	defer src.Close()

	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: sf.ModTime.Local()}
	if compressed(name) {
		hdr.Method = zip.Store
	}
	hdr.SetMode(0o644)
	w, err := zw.CreateHeader(hdr) // UTF-8: archive/zip marca la bandera si el nombre no es ASCII
	if err != nil {
		return err
	}
	_, err = io.Copy(w, src)
	return err
}

var errCap = errors.New("tope superado")

// capWriter cuenta bytes y falla en cuanto se supera el tope, sin esperar
// a terminar el ZIP.
type capWriter struct {
	w     io.Writer
	n     int64
	limit int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if c.n+int64(len(p)) > c.limit {
		return 0, errCap
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func zipErr(err error) error {
	var se *stage.Error
	switch {
	case errors.As(err, &se):
		return se
	case errors.Is(err, errCap):
		return tooLarge("el ZIP supera el tope por enlace")
	}
	return &stage.Error{Code: stage.CodeInternal, Reason: "error armando el ZIP", Err: stripPath(err)}
}

// MetaJSON serializa los metadatos sin espacios ni escapes HTML y, si el
// metaBlob resultante superaría el tope de 192 KiB, recorta entries.
func (p *Payload) MetaJSON() ([]byte, error) {
	m := p.Meta
	for {
		b, err := marshal(m)
		if err != nil {
			return nil, err
		}
		if cpk.MetaBlobSize(len(b)) <= cpk.MaxMetaBlob {
			return b, nil
		}
		if m.Entries == nil || len(*m.Entries) == 0 {
			return nil, cpk.ErrMetaTooLarge
		}
		all := *m.Entries
		m.setEntries(all, len(all)*3/4)
	}
}

func (m *Meta) setEntries(all []MetaEntry, limit int) {
	list := append([]MetaEntry{}, all[:min(len(all), limit)]...) // nunca nil: "entries":[] y no null
	truncated := len(list) < *m.EntryCount
	m.Entries = &list
	m.EntriesTruncated = nil
	if truncated {
		m.EntriesTruncated = &truncated
	}
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// topLevelNames resuelve colisiones de primer nivel sin distinguir
// mayúsculas (APFS): el segundo "informe.pdf" pasa a "informe (2).pdf" y la
// segunda carpeta "fotos" a "fotos (2)".
func topLevelNames(items []stage.ItemInfo) []string {
	out := make([]string, len(items))
	used := map[string]bool{}
	for i, it := range items {
		name := cleanName(it.Name)
		if name == "" {
			name = "elemento"
		}
		base, ext := name, ""
		if !it.IsDir {
			base, ext = splitExt(name)
		}
		candidate := name
		for k := 2; used[strings.ToLower(candidate)]; k++ {
			candidate = fmt.Sprintf("%s (%d)%s", base, k, ext)
		}
		used[strings.ToLower(candidate)] = true
		out[i] = candidate
	}
	return out
}

// zipName: una carpeta, "<carpeta>.zip"; varios elementos, "<label>.zip" o
// "archivos-AAAAMMDD.zip" con la fecha local.
func zipName(items []stage.ItemInfo, label string, now time.Time) string {
	var base string
	switch {
	case len(items) == 1:
		base = cleanName(items[0].Name)
	case label != "":
		base = label
	}
	if base == "" {
		base = "archivos-" + now.Local().Format("20060102")
	}
	return fitName(base+".zip", "archivos.zip")
}

// cleanName quita separadores de ruta y caracteres de control.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '_'
		case unicode.IsControl(r) || r == utf8.RuneError:
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// fitName trunca a 200 caracteres conservando la extensión.
func fitName(name, fallback string) string {
	if name == "" || name == "." || name == ".." {
		name = fallback
	}
	if utf8.RuneCountInString(name) <= maxNameRunes {
		return name
	}
	base, ext := splitExt(name)
	if utf8.RuneCountInString(ext) > 20 {
		base, ext = name, ""
	}
	return truncateRunes(base, maxNameRunes-utf8.RuneCountInString(ext)) + ext
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// splitExt separa la extensión; ".bashrc" no tiene extensión.
func splitExt(name string) (string, string) {
	ext := path.Ext(name)
	if ext == name || ext == "." {
		return name, ""
	}
	return strings.TrimSuffix(name, ext), ext
}

// checkZipPath: rutas relativas con "/", sin "..", sin absolutas ni vacías
// (§5). Vienen del recorrido de stage; esto es una defensa adicional.
func checkZipPath(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, `\`) || !utf8.ValidString(name) {
		return &stage.Error{Code: stage.CodePathNotAllowed, Reason: "ruta inválida en el ZIP"}
	}
	for seg := range strings.SplitSeq(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return &stage.Error{Code: stage.CodePathNotAllowed, Reason: "ruta inválida en el ZIP"}
		}
	}
	return nil
}

// Extensiones que ya vienen comprimidas: deflate no ganaría nada.
var storedExt = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`zip jpg jpeg png gif webp heic heif avif mp4 mov m4v mkv avi webm
		mp3 m4a aac ogg opus flac pdf gz tgz bz2 xz zst 7z rar docx xlsx pptx odt ods odp epub jar apk`) {
		storedExt["."+e] = true
	}
}

func compressed(name string) bool { return storedExt[strings.ToLower(path.Ext(name))] }

func mimeOf(name string) string {
	t := mime.TypeByExtension(strings.ToLower(path.Ext(name)))
	if t == "" {
		return "application/octet-stream"
	}
	mt, _, err := mime.ParseMediaType(t)
	if err != nil {
		return "application/octet-stream"
	}
	return mt
}

func stripPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}
