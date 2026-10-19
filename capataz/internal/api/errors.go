package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/resuelveonline/capataz/capataz/internal/auth"
	"github.com/resuelveonline/capataz/capataz/internal/safepath"
	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// Códigos de Error.code del contrato (api/capataz-api.yaml). Una prueba
// verifica que todos existan en el enum.
const (
	codeUnauthorized         = "unauthorized"
	codeNotFound             = "not_found"
	codeInvalidRequest       = "invalid_request"
	codePathNotAllowed       = "path_not_allowed"
	codeSyncthingUnavailable = "syncthing_unavailable"
	codeInternal             = "internal_error"
)

// errorBody es el esquema Error del contrato.
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// requestError es un error ya decidido por el handler (estado, código y
// mensaje). Los mensajes nunca repiten la ruta pedida.
type requestError struct {
	status  int
	code    string
	message string
}

func (e *requestError) Error() string { return e.message }

func badRequest(msg string) error {
	return &requestError{http.StatusBadRequest, codeInvalidRequest, msg}
}

// errNotFound se usa para carpetas desconocidas y rutas ocultas: la
// respuesta es la misma que para algo que no existe, para no revelar nada.
var errNotFound = errors.New("no existe")

// fail traduce un error de dominio a la respuesta HTTP del contrato.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var re *requestError
	switch {
	case errors.As(err, &re):
		writeError(w, re.status, re.code, re.message)
	case errors.Is(err, auth.ErrUnauthorized):
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, codeUnauthorized, "token ausente o inválido")
	case errors.Is(err, safepath.ErrNotAllowed):
		writeError(w, http.StatusBadRequest, codePathNotAllowed, "ruta no permitida")
	case errors.Is(err, errNotFound),
		errors.Is(err, safepath.ErrNotFound),
		errors.Is(err, safepath.ErrNotRegular):
		writeError(w, http.StatusNotFound, codeNotFound, "no existe")
	case errors.Is(err, syncthing.ErrUnavailable):
		// Los errores de syncthing no llevan URL, API key ni nombres.
		s.log.Warn("syncthing no disponible", "requestId", requestID(r), "error", err)
		writeError(w, http.StatusBadGateway, codeSyncthingUnavailable, "no se pudo consultar Syncthing")
	default:
		// safepath y syncthing ya quitan rutas de sus errores; el detalle
		// solo va a la bitácora, nunca al cliente.
		s.log.Error("error interno", "requestId", requestID(r), "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "error interno")
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
