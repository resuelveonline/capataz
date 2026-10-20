package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/resuelveonline/capataz/capataz/internal/auth"
	"github.com/resuelveonline/capataz/capataz/internal/links"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

const testToken = "token-de-desarrollo-muy-largo-0123456789"

// fakeSyncthing es un doble de Syncthing con carpetas fijas.
type fakeSyncthing struct {
	folders []syncthing.Folder
	err     error
}

func (f *fakeSyncthing) Folders(context.Context) ([]syncthing.Folder, error) {
	return f.folders, f.err
}

// syncBuffer: el logger escribe desde la goroutine del handler.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testEnv struct {
	srv  *Server
	st   *fakeSyncthing
	logs *syncBuffer
}

func newTestEnv(t *testing.T, folders ...syncthing.Folder) *testEnv {
	t.Helper()
	logs := &syncBuffer{}
	st := &fakeSyncthing{folders: folders}
	srv := New(Options{
		Auth:      auth.NewDevToken(testToken),
		Syncthing: st,
		Instance:  Instance{InstanceID: "abc123", Name: "Capataz", Version: "test", PublicMode: "local"},
		Logger:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return &testEnv{srv: srv, st: st, logs: logs}
}

// do hace una petición; target es la URL cruda (con la consulta ya
// codificada como la enviaría un cliente).
func (e *testEnv) do(method, target string, authed bool, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if authed {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, r)
	return w
}

// wantError comprueba estado y cuerpo JSON {code, message} del contrato.
func wantError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Errorf("estado %d; quiero %d (cuerpo %q)", w.Code, status, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type %q; quiero JSON", ct)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("cuerpo no es JSON: %q", w.Body.String())
	}
	if body["code"] != code {
		t.Errorf("code %v; quiero %q", body["code"], code)
	}
	if msg, ok := body["message"].(string); !ok || msg == "" {
		t.Errorf("message ausente: %v", body)
	}
	if len(body) != 2 {
		t.Errorf("campos extra en el error: %v", body)
	}
}

func TestInstanceIsPublic(t *testing.T) {
	e := newTestEnv(t)
	w := e.do("GET", "/instance", false)
	if w.Code != 200 {
		t.Fatalf("estado %d", w.Code)
	}
	var got Instance
	json.Unmarshal(w.Body.Bytes(), &got)
	if got != (Instance{InstanceID: "abc123", Name: "Capataz", Version: "test", PublicMode: "local"}) {
		t.Errorf("instance = %+v", got)
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Error("falta X-Request-Id")
	}
}

func TestFoldersRequiresAuth(t *testing.T) {
	e := newTestEnv(t)
	for name, hdr := range map[string][]string{
		"sin header":       nil,
		"token incorrecto": {"Authorization", "Bearer otro-token-que-no-es-el-bueno-000000"},
		"esquema basic":    {"Authorization", "Basic " + testToken},
	} {
		t.Run(name, func(t *testing.T) {
			w := e.do("GET", "/folders", false, hdr...)
			wantError(t, w, 401, codeUnauthorized)
			if w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Error("falta WWW-Authenticate: Bearer")
			}
		})
	}
}

func TestFolders(t *testing.T) {
	e := newTestEnv(t,
		syncthing.Folder{ID: "zzzzz-zzzzz", Label: "Zeta", Path: "/x"},
		syncthing.Folder{ID: "fgyww-edjwa", Label: "pruebas", Path: "/y"},
	)
	w := e.do("GET", "/folders", true)
	if w.Code != 200 {
		t.Fatalf("estado %d", w.Code)
	}
	// Solo id y label: la ruta en disco del host no sale de Capataz.
	want := `[{"id":"fgyww-edjwa","label":"pruebas"},{"id":"zzzzz-zzzzz","label":"Zeta"}]`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Errorf("cuerpo %s; quiero %s", got, want)
	}
}

func TestFoldersEmptyIsArray(t *testing.T) {
	e := newTestEnv(t)
	w := e.do("GET", "/folders", true)
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("cuerpo %q; quiero []", got)
	}
}

func TestFoldersSyncthingDown(t *testing.T) {
	e := newTestEnv(t)
	e.st.err = fmt.Errorf("syncthing config/folders: %w (connection refused)", syncthing.ErrUnavailable)
	wantError(t, e.do("GET", "/folders", true), 502, codeSyncthingUnavailable)
}

func TestFoldersInternalError(t *testing.T) {
	e := newTestEnv(t)
	e.st.err = fmt.Errorf("syncthing config/folders: respuesta JSON inválida")
	w := e.do("GET", "/folders", true)
	wantError(t, w, 500, codeInternal)
	if strings.Contains(w.Body.String(), "JSON inválida") {
		t.Error("el detalle interno no debe llegar al cliente")
	}
}

func TestRouterErrorsAreJSON(t *testing.T) {
	e := newTestEnv(t)
	wantError(t, e.do("GET", "/no-existe", true), 404, codeNotFound)

	w := e.do("POST", "/folders", true)
	wantError(t, w, 405, codeInvalidRequest)
	if allow := w.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow = %q", allow)
	}
}

func TestPanicReturnsJSON(t *testing.T) {
	e := newTestEnv(t)
	e.srv.mux.HandleFunc("GET /panico", func(http.ResponseWriter, *http.Request) {
		panic("expediente-secreto.pdf")
	})
	wantError(t, e.do("GET", "/panico", true), 500, codeInternal)
	if strings.Contains(e.logs.String(), "expediente-secreto") {
		t.Error("el valor del pánico no debe llegar a la bitácora")
	}
}

// Todos los códigos que usa la API deben existir en el enum del contrato.
func TestErrorCodesInContract(t *testing.T) {
	spec, err := os.ReadFile("../../../api/capataz-api.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{codeUnauthorized, codeNotFound, codeInvalidRequest,
		codePathNotAllowed, codeSyncthingUnavailable, codeInternal, codeForbidden, codeRateLimited,
		codePayloadTooLarge, codeCloudUnreachable, links.CodeCloudQuota, stage.CodeFileNotSynced,
		stage.CodeFileInUse, stage.CodeFileChanged, stage.CodeInsufficientStorage} {
		if !bytes.Contains(spec, []byte("            - "+code+"\n")) {
			t.Errorf("el código %q no está en Error.code del contrato", code)
		}
	}
}

func TestRoutesRegistered(t *testing.T) {
	e := newTestEnv(t)
	for _, rt := range e.srv.Routes() {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		_, pattern := e.srv.mux.Handler(httptest.NewRequest(method, path, nil))
		if pattern != rt.Pattern {
			t.Errorf("%s: el enrutador resolvió %q", rt.Pattern, pattern)
		}
		// Solo GET /instance es pública; el resto debe exigir token.
		if w := e.do(method, path, false); (w.Code == 401) == rt.Public {
			t.Errorf("%s: público=%v pero sin token respondió %d", rt.Pattern, rt.Public, w.Code)
		}
	}
}
