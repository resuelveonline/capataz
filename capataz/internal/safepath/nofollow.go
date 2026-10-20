package safepath

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
)

// ErrSymlink: algún segmento de la ruta es un enlace simbólico. La
// preparación de copias (Fase 2a) no sigue enlaces, a diferencia del
// listado y la descarga de la Fase 1: Syncthing sincroniza el enlace como
// enlace (tipo SYMLINK en su índice), no el archivo al que apunta, así que
// el destino no se puede validar contra el índice.
var ErrSymlink = errors.New("la ruta pasa por un enlace simbólico")

// StatNoFollow devuelve la información de rel sin seguir enlaces en
// ningún segmento: si alguno es simbólico, devuelve ErrSymlink.
func (f *Folder) StatNoFollow(rel string) (fs.FileInfo, error) {
	c, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	if c == "." {
		fi, err := f.root.Stat(".")
		return fi, classify(err)
	}
	var fi fs.FileInfo
	prefix := ""
	for seg := range strings.SplitSeq(c, "/") {
		prefix = path.Join(prefix, seg)
		fi, err = f.root.Lstat(prefix)
		if err != nil {
			return nil, classify(err)
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			return nil, ErrSymlink
		}
	}
	return fi, nil
}

// OpenNoFollow abre un archivo regular sin seguir enlaces: comprueba los
// segmentos intermedios con StatNoFollow y abre el último con O_NOFOLLOW,
// de modo que si alguien lo cambia por un enlace entre medias, la apertura
// falla. Igual que OpenFile, usa O_NONBLOCK y comprueba el tipo con fstat.
func (f *Folder) OpenNoFollow(rel string) (*os.File, fs.FileInfo, error) {
	if _, err := f.StatNoFollow(rel); err != nil {
		return nil, nil, err
	}
	c, _ := Clean(rel)
	file, err := f.root.OpenFile(c, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, nil, ErrSymlink
		}
		return nil, nil, classify(err)
	}
	fi, err := file.Stat()
	switch {
	case err != nil:
		err = classify(err)
	case fi.IsDir():
		err = ErrIsDir
	case !fi.Mode().IsRegular():
		err = ErrNotRegular
	}
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, fi, nil
}

// Kind clasifica lo que encuentra Walk.
type Kind int

const (
	KindFile    Kind = iota // archivo regular
	KindDir                 // carpeta (se recorre)
	KindHidden              // interno de Syncthing (no se recorre)
	KindSymlink             // enlace simbólico (no se sigue)
	KindSpecial             // FIFO, socket, dispositivo
)

// WalkEntry es un elemento del recorrido. Path es relativa a la carpeta
// sincronizada, con "/".
type WalkEntry struct {
	Path string
	Kind Kind
	Info fs.FileInfo // de Lstat; nil para KindSymlink si desapareció
}

// Walk recorre rel (que debe ser una carpeta, sin enlaces en la ruta) en
// orden alfabético y profundidad primero, sin seguir enlaces simbólicos.
// Llama a fn también con las carpetas (antes de su contenido), los ocultos,
// los enlaces y los especiales, para que quien llama decida y cuente.
// Si fn devuelve error, el recorrido se detiene con ese error.
func (f *Folder) Walk(rel string, fn func(WalkEntry) error) error {
	fi, err := f.StatNoFollow(rel)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return ErrNotDir
	}
	c, _ := Clean(rel)
	return f.walkDir(c, fn)
}

func (f *Folder) walkDir(dir string, fn func(WalkEntry) error) error {
	d, err := f.root.Open(dir)
	if err != nil {
		return classify(err)
	}
	des, err := d.ReadDir(-1) // ReadDir de *os.File no ordena; se ordena abajo
	d.Close()
	if err != nil {
		return classify(err)
	}
	sortEntries(des)

	for _, de := range des {
		p := path.Join(dir, de.Name())
		e := WalkEntry{Path: p}
		switch t := de.Type(); {
		case IsHidden(de.Name()):
			e.Kind = KindHidden
		case t&fs.ModeSymlink != 0:
			e.Kind = KindSymlink
		case t.IsDir():
			e.Kind = KindDir
		case t.IsRegular():
			e.Kind = KindFile
		default:
			e.Kind = KindSpecial
		}
		info, err := de.Info() // Lstat: no sigue enlaces
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // desapareció durante el recorrido
			}
			return classify(err)
		}
		e.Info = info
		if err := fn(e); err != nil {
			return err
		}
		if e.Kind == KindDir {
			if err := f.walkDir(p, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// sortEntries ordena por bytes del nombre: estable entre sistemas y ejecuciones.
func sortEntries(des []fs.DirEntry) {
	slices.SortFunc(des, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
}
