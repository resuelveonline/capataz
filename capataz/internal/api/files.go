package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/config"
	"github.com/resuelveonline/capataz/capataz/internal/safepath"
)

// entryDTO es el esquema Entry del contrato.
type entryDTO struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"` // relativa a la carpeta, con "/"
	IsDir    bool      `json:"isDir"`
	Size     int64     `json:"size"` // 0 en carpetas
	Modified time.Time `json:"modified"`
}

// openTarget valida los parámetros folder y path y abre la carpeta. El
// orden importa: primero el texto de la ruta (barato, sin tocar nada),
// después la carpeta contra Syncthing y al final el disco.
func (s *Server) openTarget(r *http.Request, pathRequired bool) (*safepath.Folder, string, error) {
	q := r.URL.Query()
	id := q.Get("folder")
	if id == "" {
		return nil, "", badRequest("falta el parámetro folder")
	}
	if pathRequired && q.Get("path") == "" {
		return nil, "", badRequest("falta el parámetro path")
	}
	rel, err := safepath.Clean(q.Get("path"))
	if err != nil {
		return nil, "", err
	}
	if safepath.HasHidden(rel) {
		return nil, "", errNotFound
	}

	f, err := s.findFolder(r.Context(), id)
	if err != nil {
		return nil, "", err
	}
	dir, err := config.ExpandHome(f.Path)
	if err != nil {
		return nil, "", err
	}
	folder, err := safepath.OpenFolder(dir)
	if err != nil {
		// Syncthing la conoce pero no está en disco: fallo del host, no
		// del cliente. Sin %w para que no se traduzca a 404.
		return nil, "", fmt.Errorf("la carpeta %s no se puede abrir en disco: %v", id, err)
	}
	return folder, rel, nil
}

// handleFiles atiende GET /files?folder=&path=. Lee el disco, no el índice
// de Syncthing (que puede ir por detrás, hallazgo de la Fase 0).
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	folder, rel, err := s.openTarget(r, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer folder.Close()

	entries, err := folder.ReadDir(rel)
	if errors.Is(err, safepath.ErrNotDir) {
		err = badRequest("la ruta no es una carpeta")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}

	out := make([]entryDTO, 0, len(entries))
	for _, e := range entries {
		d := entryDTO{
			Name:     e.Name,
			Path:     path.Join(rel, e.Name), // path.Join(".", x) == x
			IsDir:    e.Info.IsDir(),
			Modified: e.Info.ModTime().UTC(),
		}
		if !d.IsDir {
			d.Size = e.Info.Size()
		}
		out = append(out, d)
	}
	// Carpetas primero; después por nombre sin distinguir mayúsculas.
	slices.SortFunc(out, func(a, b entryDTO) int {
		if a.IsDir != b.IsDir {
			if a.IsDir {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
	})
	writeJSON(w, http.StatusOK, out)
}
