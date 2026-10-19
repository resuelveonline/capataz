// Package safepath resuelve rutas relativas dentro de una carpeta de
// Syncthing sin permitir salir de ella.
//
// Hay dos capas:
//  1. Clean valida el texto de la ruta (sin "..", sin rutas absolutas, sin
//     bytes nulos) para responder con un error claro.
//  2. Folder accede al disco a través de os.Root, que hace que el sistema
//     operativo rechace cualquier salida de la carpeta, incluidos enlaces
//     simbólicos que apunten fuera. Como la comprobación ocurre al abrir, no
//     hay carrera entre "validar" y "usar" la ruta.
package safepath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"unicode/utf8"
)

var (
	// ErrNotAllowed: la ruta intenta salir de la carpeta o no es válida.
	ErrNotAllowed = errors.New("ruta no permitida")
	// ErrNotFound: la ruta no existe (o es un enlace simbólico roto).
	ErrNotFound = errors.New("ruta inexistente")
	// ErrIsDir: se pidió un archivo y la ruta es un directorio.
	ErrIsDir = errors.New("la ruta es un directorio")
	// ErrNotDir: se pidió un directorio y la ruta es un archivo.
	ErrNotDir = errors.New("la ruta no es un directorio")
	// ErrNotRegular: ni archivo regular ni directorio (FIFO, socket, dispositivo).
	ErrNotRegular = errors.New("la ruta no es un archivo regular")
)

// Clean valida una ruta relativa recibida de un cliente y devuelve su forma
// canónica con "/" como separador ("." es la raíz de la carpeta). La ruta
// vacía también es la raíz.
func Clean(rel string) (string, error) {
	switch {
	case !utf8.ValidString(rel),
		strings.ContainsRune(rel, 0),
		strings.Contains(rel, `\`), // separador de Windows: ambiguo, se rechaza
		strings.HasPrefix(rel, "/"):
		return "", ErrNotAllowed
	}
	for seg := range strings.SplitSeq(rel, "/") {
		if seg == ".." {
			return "", ErrNotAllowed
		}
	}
	// Con "/" delante, path.Clean nunca produce "..": solo quita "." y "//".
	c := path.Clean("/" + rel)[1:]
	if c == "" {
		c = "."
	}
	return c, nil
}

// IsHidden indica si un nombre (un solo segmento) es interno de Syncthing.
// Compara sin distinguir mayúsculas porque APFS (macOS) no las distingue por
// defecto: ".STFOLDER" abre la misma carpeta que ".stfolder".
func IsHidden(name string) bool {
	n := strings.ToLower(name)
	switch n {
	case ".stfolder", ".stversions", ".stignore":
		return true
	}
	return strings.HasSuffix(n, ".tmp") &&
		(strings.HasPrefix(n, ".syncthing.") || strings.HasPrefix(n, "~syncthing~"))
}

// HasHidden indica si algún segmento de una ruta limpia es oculto.
func HasHidden(clean string) bool {
	for seg := range strings.SplitSeq(clean, "/") {
		if IsHidden(seg) {
			return true
		}
	}
	return false
}

// Folder da acceso de solo lectura a una carpeta sincronizada.
type Folder struct {
	root *os.Root
}

// OpenFolder abre la carpeta dir (ruta absoluta según Syncthing).
func OpenFolder(dir string) (*Folder, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, classify(err)
	}
	return &Folder{root: r}, nil
}

// Close libera el descriptor de la carpeta.
func (f *Folder) Close() error { return f.root.Close() }

// Stat devuelve la información de rel siguiendo enlaces simbólicos que no
// salgan de la carpeta.
func (f *Folder) Stat(rel string) (fs.FileInfo, error) {
	c, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	fi, err := f.root.Stat(c)
	return fi, classify(err)
}

// OpenFile abre un archivo regular para lectura. Devuelve ErrIsDir si es un
// directorio y ErrNotRegular para FIFOs, sockets o dispositivos.
//
// Se abre con O_NONBLOCK para que un FIFO no bloquee la petición esperando
// un escritor; el tipo se comprueba después sobre el descriptor ya abierto
// (fstat), así no importa si alguien cambia el archivo entre medias.
func (f *Folder) OpenFile(rel string) (*os.File, fs.FileInfo, error) {
	c, err := Clean(rel)
	if err != nil {
		return nil, nil, err
	}
	file, err := f.root.OpenFile(c, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
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

// Entry es un elemento de un listado con los enlaces simbólicos ya resueltos.
type Entry struct {
	Name string
	Info fs.FileInfo
}

// ReadDir lista el directorio rel. Omite los nombres ocultos (IsHidden), los
// enlaces simbólicos que salen de la carpeta o están rotos, y lo que no sea
// archivo regular ni directorio.
func (f *Folder) ReadDir(rel string) ([]Entry, error) {
	c, err := Clean(rel)
	if err != nil {
		return nil, err
	}
	dir, err := f.root.Open(c)
	if err != nil {
		return nil, classify(err)
	}
	defer dir.Close()

	fi, err := dir.Stat()
	if err != nil {
		return nil, classify(err)
	}
	if !fi.IsDir() {
		return nil, ErrNotDir
	}
	des, err := dir.ReadDir(-1)
	if err != nil {
		return nil, classify(err)
	}

	out := make([]Entry, 0, len(des))
	for _, de := range des {
		if IsHidden(de.Name()) {
			continue
		}
		var info fs.FileInfo
		if de.Type()&fs.ModeSymlink != 0 {
			// root.Stat sigue el enlace y falla si sale de la carpeta.
			info, err = f.root.Stat(path.Join(c, de.Name()))
		} else {
			info, err = de.Info()
		}
		if err != nil {
			continue // desapareció, enlace roto o enlace hacia fuera
		}
		if info.IsDir() || info.Mode().IsRegular() {
			out = append(out, Entry{Name: de.Name(), Info: info})
		}
	}
	return out, nil
}

// classify traduce errores del sistema de archivos a los de este paquete y
// quita la ruta del mensaje: los nombres de archivo no deben llegar a la
// bitácora.
//
// os.Root informa un escape con un error no exportado ("path escapes from
// parent"). Los errores reales del sistema operativo siempre son
// syscall.Errno, así que un *PathError cuyo error interno no lo es solo
// puede venir de la protección de os.Root. Las pruebas con enlaces
// simbólicos reales fijan este comportamiento.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	var pe *fs.PathError
	if !errors.As(err, &pe) {
		return err
	}
	var errno syscall.Errno
	if !errors.As(pe.Err, &errno) {
		return ErrNotAllowed
	}
	switch errno {
	case syscall.ENOTDIR, syscall.ELOOP, syscall.ENAMETOOLONG:
		// "archivo.txt/x", bucle de enlaces o nombre imposible: no existe.
		return ErrNotFound
	}
	return fmt.Errorf("%s: %w", pe.Op, errno)
}
