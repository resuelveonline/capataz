package links

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/cpk"
	"github.com/resuelveonline/capataz/capataz/internal/nube"
	"github.com/resuelveonline/capataz/capataz/internal/nube/nubetest"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
	"github.com/resuelveonline/capataz/capataz/internal/stage/stagetest"
)

const (
	folderID   = "fgyww-edjwa"
	instanceID = "instanciadeprueba0000000000"
	owner      = "dispositivo-dev"
	secretName = "expediente-confidencial.txt"
	secretBody = "contenido-secreto-del-expediente"
	secretLbl  = "etiqueta-secreta"
)

// syncLog es un búfer seguro para escribir desde varias goroutines.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type env struct {
	t       *testing.T
	dir     string
	state   string
	staging string
	fake    *stagetest.Fake
	stager  *stage.Stager
	cloud   *nubetest.Server
	client  *nube.Client
	logs    *syncLog
	now     atomic.Pointer[time.Time] // nil = time.Now
	opts    Options
}

func newEnv(t *testing.T) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{t: t, dir: filepath.Join(base, "pruebas"), state: filepath.Join(base, "state"),
		staging: filepath.Join(base, "staging"), logs: &syncLog{}}
	for rel, c := range map[string]string{
		secretName:      secretBody,
		"fotos/a.jpg":   "JPEG",
		"fotos/sub/b.t": "bbb",
	} {
		p := filepath.Join(e.dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	big := make([]byte, 9<<20) // dos partes de subida
	rand.Read(big)
	if err := os.WriteFile(filepath.Join(e.dir, "grande.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.fake = stagetest.New(t, e.dir, folderID, "pruebas")
	s, err := stage.New(stage.Options{Syncthing: e.fake, StagingDir: e.staging, MaxPayload: cpk.MaxPayload, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	e.stager = s

	key := make([]byte, nube.KeySize)
	rand.Read(key)
	e.cloud = nubetest.New(instanceID, key)
	t.Cleanup(e.cloud.Close)
	e.client, err = nube.New(e.cloud.URL, instanceID, key)
	if err != nil {
		t.Fatal(err)
	}
	e.opts = Options{
		StateDir: e.state, StagingDir: e.staging, Stager: s, Nube: e.client, Logger: log,
		Now: func() time.Time {
			if p := e.now.Load(); p != nil {
				return *p
			}
			return time.Now()
		},
		Retry:      nube.Retry{Attempts: 3, Base: time.Millisecond, Max: 2 * time.Millisecond},
		SweepEvery: time.Hour, RefreshEvery: time.Hour, // las pruebas llaman a mano
	}
	return e
}

func (e *env) open() *Manager {
	e.t.Helper()
	m, err := Open(e.opts)
	if err != nil {
		e.t.Fatal(err)
	}
	m.Start()
	e.t.Cleanup(m.Close)
	return m
}

func (e *env) setCloudFail(f func(*http.Request) (int, string)) {
	e.cloud.Lock()
	e.cloud.Fail = f
	e.cloud.Unlock()
}

func req(paths ...string) Request {
	var r Request
	for _, p := range paths {
		r.Items = append(r.Items, Item{Folder: folderID, Path: p})
	}
	return r
}

// wait espera a que el enlace llegue a un estado terminal o activo.
func wait(t *testing.T, m *Manager, id string, want string) Link {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		l, ok := m.Get(id)
		if !ok {
			t.Fatalf("el enlace desapareció")
		}
		if l.Status == want {
			return l
		}
		if l.Status != StatusPreparing && l.Status != StatusUploading || time.Now().After(deadline) {
			t.Fatalf("estado %q (failureCode %v), quiero %q", l.Status, deref(l.FailureCode), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := failureCode(err); err == nil || got != code {
		t.Fatalf("error %v (código %q); quiero %q", err, got, code)
	}
}

// download baja el paquete por la cara pública de la nube y lo descifra
// con la clave del fragmento, como lo haría el navegador.
func download(t *testing.T, cloudURL, link string) (content []byte, meta map[string]any) {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimPrefix(u.Path, "/s/")
	key, err := cpk.ParseKey(u.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	get := func(p string) []byte {
		resp, err := http.Get(cloudURL + "/s/" + token + p)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d %s", p, resp.StatusCode, b)
		}
		return b
	}
	var pm struct{ MetaBlob string }
	json.Unmarshal(get("/meta"), &pm)
	blob, _ := base64.RawURLEncoding.DecodeString(pm.MetaBlob)
	mj, err := cpk.DecryptMeta(key, blob)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(mj, &meta)
	var out bytes.Buffer
	if _, err := cpk.Decrypt(&out, bytes.NewReader(get("/blob")), key); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), meta
}

func (e *env) record(id string) record {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(e.state, "links", id+".json"))
	if err != nil {
		e.t.Fatal(err)
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) linkDirs() []string {
	des, _ := os.ReadDir(e.staging)
	var out []string
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

// checkLogs: la bitácora nunca lleva nombres, contenido, etiqueta, token
// ni clave.
func (e *env) checkLogs(secrets ...string) {
	e.t.Helper()
	logs := e.logs.String()
	for _, s := range append(secrets, secretName, secretBody, secretLbl, "fotos", "grande.bin") {
		if s != "" && strings.Contains(logs, s) {
			e.t.Errorf("la bitácora contiene %q:\n%s", s, logs)
		}
	}
}

func TestCreateEndToEnd(t *testing.T) {
	e := newEnv(t)
	m := e.open()
	r := req(secretName)
	r.Label = secretLbl
	l, err := m.Create(context.Background(), owner, r)
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != StatusPreparing || l.URL != nil || l.Encryption != "client" || l.Progress != 0 {
		t.Fatalf("respuesta inicial: %+v", l)
	}
	l = wait(t, m, l.ID, StatusActive)
	if l.URL == nil || !strings.HasPrefix(*l.URL, e.cloud.URL+"/s/") || l.Progress != 1 {
		t.Fatalf("enlace activo sin url: %+v", l)
	}
	if *l.PayloadSize != int64(len(secretBody)) || l.PackageSize == nil || l.Skipped == nil {
		t.Errorf("tamaños: %+v", l)
	}
	content, meta := download(t, e.cloud.URL, *l.URL)
	if string(content) != secretBody || meta["name"] != secretName || meta["label"] != secretLbl {
		t.Errorf("descarga: %q %v", content, meta)
	}

	// El token y la clave solo están en el estado del host (0600).
	u, _ := url.Parse(*l.URL)
	rec := e.record(l.ID)
	if rec.Key != u.Fragment || rec.Token != strings.TrimPrefix(u.Path, "/s/") || rec.Owner != owner {
		t.Error("el registro no guarda token, clave o titular")
	}
	fi, _ := os.Stat(filepath.Join(e.state, "links", l.ID+".json"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("permisos del registro %o", fi.Mode().Perm())
	}
	view, _ := e.cloud.Link(l.ID)
	if strings.Contains(view.TokenHash, rec.Token) {
		t.Error("la nube recibió el token en claro")
	}
	if left := e.linkDirs(); len(left) != 0 {
		t.Errorf("quedó el paquete en el staging: %v", left)
	}
	e.checkLogs(rec.Token, rec.Key)
}

func TestCreateZipAndBigPackage(t *testing.T) {
	e := newEnv(t)
	m := e.open()
	l, err := m.Create(context.Background(), owner, req("fotos", "grande.bin"))
	if err != nil {
		t.Fatal(err)
	}
	l = wait(t, m, l.ID, StatusActive)
	if got := e.cloud.Requests("PUT /v1/links/{id}/parts/{n}"); got != 2 {
		t.Errorf("se subieron %d partes, quiero 2", got)
	}
	_, meta := download(t, e.cloud.URL, *l.URL)
	if meta["kind"] != "zip" {
		t.Errorf("kind %v", meta["kind"])
	}
	e.checkLogs()
}

func TestCreateRejects(t *testing.T) {
	e := newEnv(t)
	m := e.open()
	zero := 0
	cases := []struct {
		name string
		mod  func(*Request)
		code string
	}{
		{"sin elementos", func(r *Request) { r.Items = nil }, CodeInvalidRequest},
		{"ttl corto", func(r *Request) { r.TTLSeconds = ptr(int64(3599)) }, CodeInvalidRequest},
		{"ttl largo", func(r *Request) { r.TTLSeconds = ptr(int64(2592001)) }, CodeInvalidRequest},
		{"ttl cero", func(r *Request) { r.TTLSeconds = ptr(int64(0)) }, CodeInvalidRequest},
		{"encryption none", func(r *Request) { r.Encryption = "none" }, CodeInvalidRequest},
		{"maxDownloads 0", func(r *Request) { r.MaxDownloads = &zero }, CodeInvalidRequest},
		{"label largo", func(r *Request) { r.Label = strings.Repeat("ñ", 121) }, CodeInvalidRequest},
		{"repetidos", func(r *Request) { r.Items = append(r.Items, r.Items[0]) }, CodeInvalidRequest},
		{"demasiados", func(r *Request) {
			for i := range 100 {
				r.Items = append(r.Items, Item{Folder: folderID, Path: "x" + string(rune('a'+i%26)) + strings.Repeat("y", i)})
			}
		}, CodeInvalidRequest},
		{"ruta con ..", func(r *Request) { r.Items[0].Path = "../state" }, stage.CodePathNotAllowed},
		{"no existe", func(r *Request) { r.Items[0].Path = "no-existe" }, stage.CodeNotFound},
		{"carpeta desconocida", func(r *Request) { r.Items[0].Folder = "otra" }, stage.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := req(secretName)
			c.mod(&r)
			_, err := m.Create(context.Background(), owner, r)
			wantCode(t, err, c.code)
		})
	}
	if l := m.List(""); len(l) != 0 {
		t.Errorf("un rechazo no debe crear enlaces: %d", len(l))
	}

	e.opts.Nube = nil
	e.opts.StateDir = filepath.Join(t.TempDir(), "otro")
	off := e.open()
	_, err := off.Create(context.Background(), owner, req(secretName))
	wantCode(t, err, CodeCloudUnreach)
}

func TestRateLimited(t *testing.T) {
	e := newEnv(t)
	e.opts.MaxActive = 1
	release := make(chan struct{})
	var once sync.Once
	e.setCloudFail(func(r *http.Request) (int, string) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/links" {
			<-release
		}
		return 0, ""
	})
	m := e.open()
	defer once.Do(func() { close(release) })
	l, err := m.Create(context.Background(), owner, req(secretName))
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Create(context.Background(), owner, req("fotos"))
	wantCode(t, err, CodeRateLimited)
	once.Do(func() { close(release) })
	wait(t, m, l.ID, StatusActive)
	if _, err := m.Create(context.Background(), owner, req("fotos")); err != nil {
		t.Errorf("tras terminar el primero: %v", err)
	}
}

func TestFailures(t *testing.T) {
	t.Run("archivo sin sincronizar", func(t *testing.T) {
		e := newEnv(t)
		m := e.open()
		e.fake.Forget(secretName)
		l, err := m.Create(context.Background(), owner, req(secretName))
		if err != nil {
			t.Fatal(err)
		}
		l = wait(t, m, l.ID, StatusFailed)
		if deref(l.FailureCode) != stage.CodeFileNotSynced || l.URL != nil {
			t.Errorf("failureCode %v", deref(l.FailureCode))
		}
		if e.cloud.Requests("POST /v1/links") != 0 {
			t.Error("no debió llegar a la nube")
		}
		e.checkLogs()
	})
	t.Run("cupo de la nube", func(t *testing.T) {
		e := newEnv(t)
		e.cloud.Lock()
		e.cloud.Limits.MaxStoredBytes = 10
		e.cloud.Unlock()
		m := e.open()
		l, _ := m.Create(context.Background(), owner, req(secretName))
		l = wait(t, m, l.ID, StatusFailed)
		if deref(l.FailureCode) != CodeCloudQuota {
			t.Errorf("failureCode %v", deref(l.FailureCode))
		}
		rec := e.record(l.ID)
		if rec.Token != "" || rec.Key != "" || rec.Open != nil {
			t.Error("un enlace fallido no debe conservar token ni clave")
		}
		if left := e.linkDirs(); len(left) != 0 {
			t.Errorf("quedó el paquete: %v", left)
		}
	})
	t.Run("nube caída", func(t *testing.T) {
		e := newEnv(t)
		e.setCloudFail(func(*http.Request) (int, string) { return http.StatusServiceUnavailable, "internal_error" })
		m := e.open()
		l, _ := m.Create(context.Background(), owner, req(secretName))
		l = wait(t, m, l.ID, StatusFailed)
		if deref(l.FailureCode) != CodeCloudUnreach {
			t.Errorf("failureCode %v", deref(l.FailureCode))
		}
	})
}

func TestRevoke(t *testing.T) {
	e := newEnv(t)
	m := e.open()
	ctx := context.Background()
	l, _ := m.Create(ctx, owner, req(secretName))
	l = wait(t, m, l.ID, StatusActive)

	// Sin nube, revocar falla y el enlace sigue activo.
	e.setCloudFail(func(*http.Request) (int, string) { return http.StatusBadGateway, "internal_error" })
	wantCode(t, m.Revoke(ctx, l.ID), CodeCloudUnreach)
	if got, _ := m.Get(l.ID); got.Status != StatusActive || got.URL == nil {
		t.Fatalf("tras fallar la revocación: %+v", got)
	}
	e.setCloudFail(nil)

	if err := m.Revoke(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := m.Get(l.ID)
	if got.Status != StatusRevoked || got.URL != nil {
		t.Errorf("tras revocar: %+v", got)
	}
	if v, _ := e.cloud.Link(l.ID); v.State != nube.StateRevoked {
		t.Errorf("en la nube: %q", v.State)
	}
	if rec := e.record(l.ID); rec.Key != "" || rec.Token != "" {
		t.Error("revocado con clave guardada")
	}
	if err := m.Revoke(ctx, l.ID); err != nil {
		t.Errorf("revocar dos veces: %v", err)
	}
	if err := m.Revoke(ctx, "no-existe-0000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("inexistente: %v", err)
	}
}

func TestRevokeWhilePreparing(t *testing.T) {
	e := newEnv(t)
	release := make(chan struct{})
	e.setCloudFail(func(r *http.Request) (int, string) {
		<-release
		return 0, ""
	})
	m := e.open()
	l, _ := m.Create(context.Background(), owner, req(secretName))
	// Aún sin abrir en la nube: se cancela en local.
	if err := m.Revoke(context.Background(), l.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	m.Close()
	if got, _ := m.Get(l.ID); got.Status != StatusRevoked {
		t.Errorf("estado %q", got.Status)
	}
	if left := e.linkDirs(); len(left) != 0 {
		t.Errorf("quedó algo en el staging: %v", left)
	}
}

func TestExpiryAndRefresh(t *testing.T) {
	e := newEnv(t)
	m := e.open()
	ctx := context.Background()
	one := 1
	r := req(secretName)
	r.MaxDownloads = &one
	a, _ := m.Create(ctx, owner, r)
	a = wait(t, m, a.ID, StatusActive)
	b, _ := m.Create(ctx, owner, req("fotos"))
	b = wait(t, m, b.ID, StatusActive)

	// Una descarga agota a; la conciliación lo ve como expired.
	download(t, e.cloud.URL, *a.URL)
	m.Refresh(ctx)
	got, _ := m.Get(a.ID)
	if got.Status != StatusExpired || got.Downloads != 1 || got.URL != nil {
		t.Errorf("a tras conciliar: %+v", got)
	}
	if !strings.Contains(e.logs.String(), "enlace descargado") {
		t.Error("la bitácora no registra la descarga")
	}

	// b vence por hora local, sin hablar con la nube.
	e.setCloudFail(func(*http.Request) (int, string) { return http.StatusBadGateway, "internal_error" })
	later := b.ExpiresAt.Add(time.Second)
	e.now.Store(&later)
	m.Sweep()
	if got, _ := m.Get(b.ID); got.Status != StatusExpired {
		t.Errorf("b: %q", got.Status)
	}
	if rec := e.record(b.ID); rec.Key != "" {
		t.Error("vencido con clave guardada")
	}

	list := m.List("")
	if len(list) != 2 || list[0].ID != b.ID {
		t.Errorf("lista: %d, primero %q", len(list), list[0].ID)
	}
	if n := len(m.List(StatusActive)); n != 0 {
		t.Errorf("activos: %d", n)
	}
}

func TestRestartResumesUpload(t *testing.T) {
	e := newEnv(t)
	e.opts.Retry = nube.Retry{Attempts: 1000, Base: time.Millisecond, Max: 5 * time.Millisecond}
	e.setCloudFail(func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/parts/2") {
			return http.StatusServiceUnavailable, "internal_error"
		}
		return 0, ""
	})
	m, err := Open(e.opts)
	if err != nil {
		t.Fatal(err)
	}
	m.Start()
	l, _ := m.Create(context.Background(), owner, req("grande.bin"))
	for e.cloud.Requests("PUT /v1/links/{id}/parts/{n}") < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	m.Close() // apagado a media subida
	if got, _ := m.Get(l.ID); got.Status != StatusUploading {
		t.Fatalf("tras apagar: %q", got.Status)
	}
	if len(e.linkDirs()) != 1 {
		t.Fatal("el paquete debe quedarse para reanudar")
	}

	e.setCloudFail(nil)
	before := e.cloud.Requests("PUT /v1/links/{id}/parts/{n}")
	m2 := e.open()
	got := wait(t, m2, l.ID, StatusActive)
	if n := e.cloud.Requests("PUT /v1/links/{id}/parts/{n}") - before; n != 1 {
		t.Errorf("al reanudar se subieron %d partes, quiero 1", n)
	}
	content, _ := download(t, e.cloud.URL, *got.URL)
	orig, _ := os.ReadFile(filepath.Join(e.dir, "grande.bin"))
	if !bytes.Equal(content, orig) {
		t.Error("el contenido reanudado no coincide")
	}
}

func TestRestartPreparingFailsAndCleansOrphans(t *testing.T) {
	e := newEnv(t)
	st, err := openStore(e.state)
	if err != nil {
		t.Fatal(err)
	}
	r := &record{Link: Link{ID: "a-medias-000000", Status: StatusPreparing, CreatedAt: time.Now(), Items: []Item{{folderID, secretName}}}}
	st.save(r)
	os.MkdirAll(filepath.Join(e.staging, linkDirPrefix+r.ID), 0o700)
	os.MkdirAll(filepath.Join(e.staging, linkDirPrefix+"sin-registro-0000"), 0o700)
	os.WriteFile(filepath.Join(e.state, "links", "dañado.json"), []byte("{"), 0o600)

	m := e.open()
	got, _ := m.Get(r.ID)
	if got.Status != StatusFailed || deref(got.FailureCode) != CodeInternal {
		t.Errorf("tras reiniciar: %+v", got)
	}
	if left := e.linkDirs(); len(left) != 0 {
		t.Errorf("huérfanos: %v", left)
	}
	if !strings.Contains(e.logs.String(), "registros de enlace dañados") {
		t.Error("no avisa del registro dañado")
	}
}

func TestStorePermissions(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "links"), 0o755)
	os.Chmod(filepath.Join(dir, "links"), 0o755)
	if _, err := openStore(dir); err == nil {
		t.Error("aceptó un directorio de enlaces 0755")
	}
}

func ptr[T any](v T) *T { return &v }

func TestReconcileCloudFailureCode(t *testing.T) {
	e := newEnv(t)
	m := e.open()
	l, _ := m.Create(context.Background(), owner, req(secretName))
	l = wait(t, m, l.ID, StatusActive)
	code := "checksum_mismatch"
	m.mu.Lock()
	m.reconcile(m.recs[l.ID], nube.Link{LinkID: l.ID, State: nube.StateFailed, FailureCode: &code})
	m.mu.Unlock()
	got, _ := m.Get(l.ID)
	if got.Status != StatusFailed || deref(got.FailureCode) != CodeCloudUnreach {
		t.Errorf("estado %q, failureCode %q; quiero failed y %q", got.Status, deref(got.FailureCode), CodeCloudUnreach)
	}
}
