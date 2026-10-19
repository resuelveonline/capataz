package api

import (
	"errors"
	"mime"
	"net/http"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/safepath"
)

// downloadIdle es cuánto puede pasar una descarga sin avanzar antes de
// cortarse. No limita la duración total: un archivo de varios GB por una
// red lenta termina siempre que siga avanzando.
const downloadIdle = 2 * time.Minute

// handleDownload atiende GET /files/download?folder=&path=.
//
// http.ServeContent resuelve Range (206), If-Range, If-Modified-Since (304)
// y Last-Modified, que es lo que permite reanudar descargas.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	folder, rel, err := s.openTarget(r, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer folder.Close()

	file, fi, err := folder.OpenFile(rel)
	if errors.Is(err, safepath.ErrIsDir) {
		err = badRequest("la ruta es una carpeta")
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer file.Close()

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": fi.Name()})
	if disposition == "" {
		disposition = "attachment"
	}
	h := w.Header()
	// Tipo fijo: nunca se interpreta el contenido (un .html no se renderiza).
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", disposition) // RFC 6266; codifica nombres no ASCII
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "private, no-cache")

	dw := &downloadWriter{ResponseWriter: w, rc: http.NewResponseController(w)}
	dw.extendDeadline(time.Now())
	http.ServeContent(dw, r, fi.Name(), fi.ModTime(), file)
}

// downloadWriter hace dos cosas sobre la respuesta de ServeContent:
//
//  1. Plazo de escritura por inactividad: cada cierto tiempo lo empuja
//     hacia delante con ResponseController. El servidor no tiene
//     WriteTimeout global.
//  2. Errores en JSON: ServeContent responde con texto plano (p. ej. 416
//     con "invalid range"). Se sustituye por el JSON del contrato,
//     conservando los headers que ya puso, como Content-Range: bytes */N.
type downloadWriter struct {
	http.ResponseWriter
	rc       *http.ResponseController
	extended time.Time
	swallow  bool
}

func (d *downloadWriter) WriteHeader(code int) {
	if code < 400 {
		d.ResponseWriter.WriteHeader(code)
		return
	}
	d.swallow = true // descartar el cuerpo de texto que viene detrás
	switch code {
	case http.StatusRequestedRangeNotSatisfiable:
		writeError(d.ResponseWriter, code, codeInvalidRequest, "rango no satisfacible")
	case http.StatusPreconditionFailed:
		writeError(d.ResponseWriter, code, codeInvalidRequest, "precondición no cumplida")
	default:
		writeError(d.ResponseWriter, http.StatusInternalServerError, codeInternal, "error interno")
	}
}

func (d *downloadWriter) Write(b []byte) (int, error) {
	if d.swallow {
		return len(b), nil
	}
	// Renovar el plazo en cada Write sería un temporizador por cada 32 KiB;
	// basta con hacerlo cuando ha pasado una fracción del plazo.
	if now := time.Now(); now.Sub(d.extended) > downloadIdle/4 {
		d.extendDeadline(now)
	}
	return d.ResponseWriter.Write(b)
}

func (d *downloadWriter) extendDeadline(now time.Time) {
	_ = d.rc.SetWriteDeadline(now.Add(downloadIdle)) // ErrNotSupported en pruebas con httptest.Recorder
	d.extended = now
}

func (d *downloadWriter) Unwrap() http.ResponseWriter { return d.ResponseWriter }
