// Package api expone la API HTTP de Capataz (contrato en api/capataz-api.yaml).
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/auth"
	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// writeTimeout es el plazo de escritura de las respuestas normales. No es
// global (http.Server.WriteTimeout) porque cortaría descargas grandes; las
// descargas gestionan su propio plazo (ver download.go).
const writeTimeout = 30 * time.Second

// Syncthing es lo que la API necesita del cliente de Syncthing. Es una
// interfaz para poder probar los handlers con un doble.
type Syncthing interface {
	Folders(ctx context.Context) ([]syncthing.Folder, error)
}

// Instance es el esquema Instance del contrato.
type Instance struct {
	InstanceID string `json:"instanceId"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	PublicMode string `json:"publicMode"`
}

// Options agrupa las dependencias del servidor.
type Options struct {
	Auth      auth.Authenticator
	Syncthing Syncthing
	Instance  Instance
	Logger    *slog.Logger
}

// Server es el http.Handler de la API.
type Server struct {
	auth     auth.Authenticator
	st       Syncthing
	instance Instance
	log      *slog.Logger
	mux      *http.ServeMux
	// noWriteDeadline: rutas que gestionan su propio plazo de escritura.
	noWriteDeadline map[string]bool
}

// New crea el servidor y registra las rutas.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	s := &Server{
		auth:            o.Auth,
		st:              o.Syncthing,
		instance:        o.Instance,
		log:             o.Logger,
		mux:             http.NewServeMux(),
		noWriteDeadline: map[string]bool{},
	}
	s.routes()
	return s
}

type route struct {
	pattern     string
	handler     http.HandlerFunc
	public      bool // sin autenticación
	ownDeadline bool // gestiona su propio plazo de escritura
}

func (s *Server) routeTable() []route {
	return []route{
		{pattern: "GET /instance", handler: s.handleInstance, public: true},
		{pattern: "GET /folders", handler: s.handleFolders},
	}
}

// routes registra las rutas con el enrutador de Go 1.22+, que entiende
// "MÉTODO /ruta". Un patrón GET también atiende HEAD.
func (s *Server) routes() {
	for _, rt := range s.routeTable() {
		h := rt.handler
		if !rt.public {
			h = s.authed(h)
		}
		s.mux.HandleFunc(rt.pattern, h)
		s.noWriteDeadline[rt.pattern] = rt.ownDeadline
	}
}

// Routes devuelve los patrones registrados y si son públicos.
func (s *Server) Routes() []RouteInfo {
	var out []RouteInfo
	for _, rt := range s.routeTable() {
		out = append(out, RouteInfo{Pattern: rt.pattern, Public: rt.public})
	}
	return out
}

// RouteInfo describe una ruta registrada.
type RouteInfo struct {
	Pattern string
	Public  bool
}

// authed exige credenciales válidas y guarda el Principal en el contexto.
func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	}
}

// ServeHTTP envuelve el enrutador con: id de petición, recuperación de
// pánicos, plazo de escritura, errores JSON para 404/405 y bitácora.
//
// La bitácora registra solo método, patrón de ruta (p. ej.
// "GET /files/download"), estado, bytes y duración. Nunca la URL real, la
// cadena de consulta ni headers: pueden contener nombres de expedientes o
// el token.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := newRequestID()
	w.Header().Set("X-Request-Id", id)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
	sw := &statusWriter{ResponseWriter: w}

	// mux.Handler solo consulta qué patrón corresponde, sin ejecutar nada.
	h, pattern := s.mux.Handler(r)
	route := pattern
	if route == "" {
		route = "(sin ruta)"
	}

	defer func() {
		if v := recover(); v != nil {
			if v == http.ErrAbortHandler {
				panic(v) // señal deliberada para cortar la conexión
			}
			// Solo el tipo y la pila: el valor podría contener una ruta.
			s.log.Error("pánico en handler", "requestId", id, "route", route,
				"panic", fmt.Sprintf("%T", v), "stack", string(debug.Stack()))
			if sw.status == 0 {
				writeError(sw, http.StatusInternalServerError, codeInternal, "error interno")
			}
		}
		s.log.Info("petición", "requestId", id, "method", r.Method, "route", route,
			"status", sw.status, "bytes", sw.bytes, "duration", time.Since(start).Round(time.Microsecond))
	}()

	if pattern == "" {
		s.unmatched(sw, r, h)
		return
	}
	if !s.noWriteDeadline[pattern] {
		// Si el ResponseWriter no lo admite (p. ej. en pruebas), no pasa nada.
		_ = http.NewResponseController(sw).SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	s.mux.ServeHTTP(sw, r)
}

// unmatched convierte las respuestas de texto del enrutador (404 y 405) en
// errores JSON del contrato. Para saber cuál toca, ejecuta su handler sobre
// un writer que solo anota el estado y los headers.
func (s *Server) unmatched(w http.ResponseWriter, r *http.Request, h http.Handler) {
	probe := &probeWriter{header: http.Header{}}
	h.ServeHTTP(probe, r)
	switch probe.status {
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", probe.header.Get("Allow"))
		writeError(w, http.StatusMethodNotAllowed, codeInvalidRequest, "método no permitido")
	default:
		writeError(w, http.StatusNotFound, codeNotFound, "ruta inexistente")
	}
}

type requestIDKey struct{}

func requestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// statusWriter anota estado y bytes para la bitácora. Unwrap permite que
// http.NewResponseController llegue a la conexión real.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// probeWriter descarta el cuerpo y solo recuerda estado y headers.
type probeWriter struct {
	header http.Header
	status int
}

func (p *probeWriter) Header() http.Header { return p.header }
func (p *probeWriter) WriteHeader(code int) {
	if p.status == 0 {
		p.status = code
	}
}
func (p *probeWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = http.StatusOK
	}
	return len(b), nil
}
