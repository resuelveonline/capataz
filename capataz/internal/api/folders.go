package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// handleInstance atiende GET /instance (pública, sin datos sensibles).
func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.instance)
}

// folderDTO es el esquema Folder del contrato. La API de Syncthing usa el
// id; el label es el nombre que ve la persona.
type folderDTO struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// handleFolders atiende GET /folders: todas las carpetas que reporta
// Syncthing (en la Fase 1 no hay permisos por cuenta).
func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := s.st.Folders(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]folderDTO, 0, len(folders))
	for _, f := range folders {
		out = append(out, folderDTO{ID: f.ID, Label: f.Label})
	}
	slices.SortFunc(out, func(a, b folderDTO) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Label), strings.ToLower(b.Label)), cmp.Compare(a.ID, b.ID))
	})
	writeJSON(w, http.StatusOK, out)
}

// findFolder busca la carpeta por id entre las que reporta Syncthing. Es
// obligatorio antes de tocar el disco: solo se aceptan rutas dentro de
// carpetas que Syncthing conoce.
func (s *Server) findFolder(ctx context.Context, id string) (syncthing.Folder, error) {
	folders, err := s.st.Folders(ctx)
	if err != nil {
		return syncthing.Folder{}, err
	}
	for _, f := range folders {
		if f.ID == id {
			return f, nil
		}
	}
	return syncthing.Folder{}, errNotFound
}
