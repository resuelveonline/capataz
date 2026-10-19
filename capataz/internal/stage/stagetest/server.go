package stagetest

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// Server expone el Fake con la REST API de Syncthing (config/folders,
// db/file, db/scan, db/need) y las mismas respuestas de error que
// Syncthing v2.1.6, para probar el cliente real de extremo a extremo.
func Server(t testing.TB, f *Fake, apiKey string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != apiKey {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		q := r.URL.Query()
		switch r.URL.Path {
		case "/rest/config/folders":
			fs, _ := f.Folders(r.Context())
			writeJSON(w, fs)
		case "/rest/db/file":
			st, err := f.File(r.Context(), q.Get("folder"), q.Get("file"))
			if errors.Is(err, syncthing.ErrNotIndexed) {
				http.Error(w, "No such object in the index", http.StatusNotFound)
				return
			}
			writeJSON(w, st)
		case "/rest/db/scan":
			io.WriteString(w, "")
		case "/rest/db/need":
			writeJSON(w, syncthing.Need{Progress: []syncthing.FileInfo{}, Queued: []syncthing.FileInfo{}, Rest: []syncthing.FileInfo{}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
