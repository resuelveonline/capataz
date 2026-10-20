package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/resuelveonline/capataz/capataz/internal/auth"
	"github.com/resuelveonline/capataz/capataz/internal/links"
	"github.com/resuelveonline/capataz/capataz/internal/safepath"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
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
	codeForbidden            = "forbidden"
	codeRateLimited          = "rate_limited"
	codePayloadTooLarge      = "payload_too_large"
	codeCloudUnreachable     = "cloud_unreachable"
)

// statusOf da el estado HTTP de un código de links o stage que llega como
// respuesta (no como failureCode).
var statusOf = map[string]int{
	codeInvalidRequest:       http.StatusBadRequest,
	codePathNotAllowed:       http.StatusBadRequest,
	codeNotFound:             http.StatusNotFound,
	codeRateLimited:          http.StatusTooManyRequests,
	codePayloadTooLarge:      http.StatusRequestEntityTooLarge,
	codeCloudUnreachable:     http.StatusBadGateway,
	codeSyncthingUnavailable: http.StatusBadGateway,
}

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
	var le *links.Error
	var se *stage.Error
	switch {
	case errors.As(err, &re):
		writeError(w, re.status, re.code, re.message)
	case errors.As(err, &le):
		s.coded(w, r, le.Code, le.Message, err)
	case errors.As(err, &se):
		msg := se.Reason
		if se.Item > 0 {
			msg = fmt.Sprintf("elemento %d: %s", se.Item, se.Reason)
		}
		s.coded(w, r, se.Code, msg, err)
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

// coded responde un error con código de dominio. Los mensajes de links y
// stage nunca llevan rutas; un código sin estado conocido es interno.
func (s *Server) coded(w http.ResponseWriter, r *http.Request, code, msg string, err error) {
	status, ok := statusOf[code]
	if !ok {
		s.log.Error("error interno", "requestId", requestID(r), "error", err)
		writeError(w, http.StatusInternalServerError, codeInternal, "error interno")
		return
	}
	if status >= 500 {
		s.log.Warn("servicio externo no disponible", "requestId", requestID(r), "code", code, "error", err)
	}
	if code == codeRateLimited {
		w.Header().Set("Retry-After", "30")
	}
	writeError(w, status, code, msg)
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
