package stage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// copyOnce hace un intento de copia. changed indica que el origen cambió
// (antes, durante o justo después) y la copia ya se borró.
func (sn *Snapshot) copyOnce(ctx context.Context, e *Entry) (sf StagedFile, changed bool, err error) {
	n := e.Item + 1
	root, window := sn.roots[e.folder], sn.windows[e.folder]

	src, before, err := root.OpenNoFollow(e.rel)
	if err != nil {
		return sf, false, pathErr(n, err)
	}
	defer src.Close()
	if before.Size() != e.Size || !sameTime(before.ModTime(), e.ModTime, window) {
		return sf, true, nil // cambió desde la validación
	}

	sn.seq++
	dst := filepath.Join(sn.dir, fmt.Sprintf("%06d", sn.seq)) // sin el nombre original
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return sf, false, newErr(n, CodeInternal, "no se pudo escribir en el staging", err)
	}
	removeCopy := true
	defer func() {
		if removeCopy {
			os.Remove(dst)
		}
	}()

	h := sha256.New()
	// Copia real byte a byte (no enlace duro ni clon): la copia no comparte
	// nada con el original.
	copied, err := io.Copy(io.MultiWriter(out, h), src)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return sf, false, newErr(n, CodeInternal, "error copiando al staging", stripPath(err))
	}
	if sn.s.afterCopy != nil {
		sn.s.afterCopy(e.rel)
	}

	// Re-verificar: mismo archivo (descriptor y ruta), mismo tamaño y fecha,
	// y el índice sigue en la misma versión.
	after, err := src.Stat()
	if err != nil {
		return sf, true, nil
	}
	now, err := root.StatNoFollow(e.rel)
	if err != nil || !os.SameFile(after, now) {
		return sf, true, nil
	}
	for _, fi := range []os.FileInfo{after, now} {
		if fi.Size() != e.Size || !sameTime(fi.ModTime(), e.ModTime, window) {
			return sf, true, nil
		}
	}
	if copied != e.Size {
		return sf, true, nil
	}
	st, err := sn.s.st.File(ctx, e.folder, e.rel)
	if errors.Is(err, syncthing.ErrNotIndexed) {
		return sf, true, nil
	}
	if err != nil {
		return sf, false, syncErr(n, err)
	}
	if !slices.Equal(st.Local.Version, e.version) || st.Local.Sequence != e.seq || st.Local.Deleted {
		return sf, true, nil
	}

	if err := os.Chtimes(dst, e.ModTime, e.ModTime); err != nil {
		return sf, false, newErr(n, CodeInternal, "error en el staging", stripPath(err))
	}
	removeCopy = false
	sf = StagedFile{Path: dst, Size: copied, ModTime: e.ModTime}
	copy(sf.SHA256[:], h.Sum(nil))
	return sf, false, nil
}

// stripPath quita la ruta de un *PathError: aunque el staging usa nombres
// numéricos, la ruta incluye el directorio del usuario.
func stripPath(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}
