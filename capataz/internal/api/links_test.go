package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/resuelveonline/capataz/capataz/internal/auth"
	"github.com/resuelveonline/capataz/capataz/internal/links"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
)

// fakeLinks es un doble del gestor de enlaces.
type fakeLinks struct {
	got     links.Request
	owner   string
	err     error
	revoked string
	byID    map[string]links.Link
}

func (f *fakeLinks) Create(_ context.Context, owner string, req links.Request) (links.Link, error) {
	f.got, f.owner = req, owner
	if f.err != nil {
		return links.Link{}, f.err
	}
	return links.Link{ID: "enlace-nuevo-01", Status: links.StatusPreparing, Items: req.Items, Encryption: "client"}, nil
}

func (f *fakeLinks) Get(id string) (links.Link, bool) { l, ok := f.byID[id]; return l, ok }

func (f *fakeLinks) List(status string) []links.Link {
	out := []links.Link{}
	for _, l := range f.byID {
		if status == "" || l.Status == status {
			out = append(out, l)
		}
	}
	return out
}

func (f *fakeLinks) Revoke(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	if _, ok := f.byID[id]; !ok {
		return links.ErrNotFound
	}
	f.revoked = id
	return nil
}

// roleAuth autentica cualquier Bearer con el rol dado.
type roleAuth struct{ role auth.Role }

func (a roleAuth) Authenticate(r *http.Request) (auth.Principal, error) {
	if r.Header.Get("Authorization") == "" {
		return auth.Principal{}, auth.ErrUnauthorized
	}
	return auth.Principal{DeviceID: "dispositivo-1", Role: a.role}, nil
}

func newLinksEnv(t *testing.T, role auth.Role, fl Links) (*Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	srv := New(Options{
		Auth: roleAuth{role}, Syncthing: &fakeSyncthing{}, Links: fl,
		Logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return srv, logs
}

func send(srv *Server, method, target, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer x")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, r)
	return w
}

const secretPath = "expedientes/confidencial.pdf"

func TestCreateLink(t *testing.T) {
	fl := &fakeLinks{}
	srv, logs := newLinksEnv(t, auth.RoleTitular, fl)
	w := send(srv, "POST", "/links", `{"items":[{"folder":"f1","path":"`+secretPath+`"}],"ttlSeconds":7200,"maxDownloads":3,"label":"x"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("estado %d: %s", w.Code, w.Body)
	}
	var l map[string]any
	json.Unmarshal(w.Body.Bytes(), &l)
	if l["status"] != "preparing" || l["url"] != nil || w.Header().Get("Location") != "/links/enlace-nuevo-01" {
		t.Errorf("respuesta: %v", l)
	}
	for _, k := range []string{"payloadSize", "payloadSha256", "packageSize", "packageSha256", "skipped", "failureCode"} {
		if _, ok := l[k]; !ok {
			t.Errorf("falta %q en Link", k)
		}
	}
	if fl.owner != "dispositivo-1" || *fl.got.TTLSeconds != 7200 || *fl.got.MaxDownloads != 3 || fl.got.Items[0].Path != secretPath {
		t.Errorf("petición traducida: %+v", fl.got)
	}
	if strings.Contains(logs.String(), "confidencial") {
		t.Error("la bitácora contiene la ruta")
	}

	// Sin ttlSeconds: lo decide links (nil = por defecto).
	send(srv, "POST", "/links", `{"items":[{"folder":"f1","path":"a"}]}`)
	if fl.got.TTLSeconds != nil {
		t.Error("ttlSeconds ausente debe llegar como nil")
	}
}

func TestCreateLinkRejects(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		err    error
		status int
		code   string
	}{
		{"campo desconocido", `{"items":[],"extra":1}`, nil, 400, codeInvalidRequest},
		{"no es JSON", `{`, nil, 400, codeInvalidRequest},
		{"dos objetos", `{"items":[]}{}`, nil, 400, codeInvalidRequest},
		{"demasiado grande", `{"label":"` + strings.Repeat("a", 70<<10) + `"}`, nil, 413, codePayloadTooLarge},
		{"invalid_request de links", `{}`, &links.Error{Code: links.CodeInvalidRequest, Message: "m"}, 400, codeInvalidRequest},
		{"ruta no permitida", `{}`, &stage.Error{Code: stage.CodePathNotAllowed, Reason: "r", Item: 2}, 400, codePathNotAllowed},
		{"no existe", `{}`, &stage.Error{Code: stage.CodeNotFound, Reason: "r", Item: 1}, 404, codeNotFound},
		{"demasiados a la vez", `{}`, &links.Error{Code: links.CodeRateLimited, Message: "m"}, 429, codeRateLimited},
		{"sin nube", `{}`, &links.Error{Code: links.CodeCloudUnreach, Message: "m"}, 502, codeCloudUnreachable},
		{"syncthing caído", `{}`, &stage.Error{Code: stage.CodeSyncthingUnavailable, Reason: "r"}, 502, codeSyncthingUnavailable},
		{"interno", `{}`, &stage.Error{Code: stage.CodeInternal, Reason: "r"}, 500, codeInternal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := newLinksEnv(t, auth.RoleTitular, &fakeLinks{err: c.err})
			w := send(srv, "POST", "/links", c.body)
			wantError(t, w, c.status, c.code)
			if c.status == 429 && w.Header().Get("Retry-After") == "" {
				t.Error("429 sin Retry-After")
			}
		})
	}
	srv, _ := newLinksEnv(t, auth.RoleTitular, &fakeLinks{err: &stage.Error{Code: stage.CodeNotFound, Reason: "no existe", Item: 3}})
	w := send(srv, "POST", "/links", `{}`)
	if !strings.Contains(w.Body.String(), "elemento 3") {
		t.Errorf("el mensaje debe decir qué elemento falló: %s", w.Body)
	}
}

func TestLinksRequireTitular(t *testing.T) {
	srv, _ := newLinksEnv(t, auth.RoleDevice, &fakeLinks{})
	for _, rt := range [][2]string{{"POST", "/links"}, {"GET", "/links"}, {"GET", "/links/x"}, {"DELETE", "/links/x"}} {
		wantError(t, send(srv, rt[0], rt[1], `{}`), 403, codeForbidden)
	}
	srv, _ = newLinksEnv(t, auth.RoleTitular, nil)
	wantError(t, send(srv, "GET", "/links", ""), 502, codeCloudUnreachable)
}

func TestGetListRevokeLink(t *testing.T) {
	fl := &fakeLinks{byID: map[string]links.Link{
		"a-0000000": {ID: "a-0000000", Status: links.StatusActive},
		"b-0000000": {ID: "b-0000000", Status: links.StatusFailed},
	}}
	srv, _ := newLinksEnv(t, auth.RoleTitular, fl)

	if w := send(srv, "GET", "/links/a-0000000", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"active"`) {
		t.Errorf("GET: %d %s", w.Code, w.Body)
	}
	wantError(t, send(srv, "GET", "/links/no-existe", ""), 404, codeNotFound)

	var list []links.Link
	w := send(srv, "GET", "/links?status=failed", "")
	json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list) != 1 || list[0].ID != "b-0000000" {
		t.Errorf("lista filtrada: %d %s", w.Code, w.Body)
	}
	wantError(t, send(srv, "GET", "/links?status=exhausted", ""), 400, codeInvalidRequest)

	if w := send(srv, "DELETE", "/links/a-0000000", ""); w.Code != 204 || fl.revoked != "a-0000000" {
		t.Errorf("DELETE: %d", w.Code)
	}
	wantError(t, send(srv, "DELETE", "/links/no-existe", ""), 404, codeNotFound)
	fl.err = &links.Error{Code: links.CodeCloudUnreach, Message: "m"}
	wantError(t, send(srv, "DELETE", "/links/a-0000000", ""), 502, codeCloudUnreachable)
	wantError(t, send(srv, "PUT", "/links/a-0000000", ""), 405, codeInvalidRequest)
}
