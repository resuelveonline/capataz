package nube_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/nube"
	"github.com/resuelveonline/capataz/capataz/internal/nube/nubetest"
)

const instanceID = "instanciadeprueba0000000000"

func setup(t *testing.T) (*nube.Client, *nubetest.Server) {
	t.Helper()
	key := make([]byte, nube.KeySize)
	rand.Read(key)
	srv := nubetest.New(instanceID, key)
	t.Cleanup(srv.Close)
	c, err := nube.New(srv.URL, instanceID, key)
	if err != nil {
		t.Fatal(err)
	}
	return c, srv
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func openReq(id string, size int64, partSize int) nube.OpenUploadRequest {
	tok := randomBytes(32)
	sum := sha256.Sum256([]byte(base64.RawURLEncoding.EncodeToString(tok)))
	return nube.OpenUploadRequest{
		LinkID:        id,
		TokenHash:     base64.RawURLEncoding.EncodeToString(sum[:]),
		ExpiresAt:     time.Now().Add(24 * time.Hour),
		CipherVersion: "cpk1",
		EncryptedSize: size,
		PartSize:      partSize,
		MetaBlob:      base64.RawURLEncoding.EncodeToString(randomBytes(100)),
	}
}

var fastRetry = nube.Retry{Attempts: 4, Base: time.Millisecond, Max: 5 * time.Millisecond}

func TestNewRejects(t *testing.T) {
	key := make([]byte, nube.KeySize)
	for _, u := range []string{"http://sync.softmx.net", "ftp://x", "https://u:p@x", "https://x?a=1", "://"} {
		if _, err := nube.New(u, instanceID, key); err == nil {
			t.Errorf("%q: se esperaba error", u)
		}
	}
	if _, err := nube.New("https://x", instanceID, key[:31]); err == nil {
		t.Error("clave corta aceptada")
	}
	for _, u := range []string{"https://sync.softmx.net", "http://127.0.0.1:9", "http://localhost:9", "http://[::1]:9"} {
		if _, err := nube.New(u, instanceID, key); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
}

func TestParseKey(t *testing.T) {
	key := randomBytes(32)
	for _, s := range []string{base64.RawURLEncoding.EncodeToString(key), base64.StdEncoding.EncodeToString(key) + "\n"} {
		got, err := nube.ParseKey(s)
		if err != nil || !bytes.Equal(got, key) {
			t.Errorf("ParseKey: %v", err)
		}
	}
	if _, err := nube.ParseKey(base64.RawURLEncoding.EncodeToString(key[:16])); err == nil {
		t.Error("clave de 16 bytes aceptada")
	}
	_, err := nube.ParseKey("no-es-base64-" + strings.Repeat("!", 30))
	if err == nil || strings.Contains(err.Error(), "!!!") {
		t.Errorf("el error no debe repetir el valor: %v", err)
	}
}

func TestInstanceAndSignature(t *testing.T) {
	c, _ := setup(t)
	inst, err := c.Instance(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inst.InstanceID != instanceID || inst.Limits.MaxPartSize != 8<<20 {
		t.Errorf("instancia inesperada: %+v", inst)
	}
	if _, err := c.OpenUpload(context.Background(), openReq("enlace-cuenta-01", 1<<20, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if inst, _ := c.Instance(context.Background()); inst.Usage.ActiveLinks != 1 || inst.Usage.StoredBytes != 1<<20 {
		t.Errorf("uploading debe contar en usage: %+v", inst.Usage)
	}

	// Otra clave: la firma no vale.
	other, _ := nube.New(c.BaseURL(), instanceID, randomBytes(32))
	if _, err := other.Instance(context.Background()); !nube.IsCode(err, "invalid_signature") {
		t.Errorf("clave ajena: %v", err)
	}
	unknown, _ := nube.New(c.BaseURL(), "otra-instancia-000", randomBytes(32))
	if _, err := unknown.Instance(context.Background()); !nube.IsCode(err, "unknown_instance") {
		t.Errorf("instancia ajena: %v", err)
	}
}

func TestClockSkewIsCorrected(t *testing.T) {
	c, srv := setup(t)
	srv.Lock()
	srv.Now = func() time.Time { return time.Now().Add(20 * time.Minute) }
	srv.Unlock()
	if _, err := c.Instance(context.Background()); err != nil {
		t.Fatalf("con el reloj desfasado: %v", err)
	}
	if got := srv.Requests("GET /v1/instance"); got != 1 {
		t.Errorf("se atendieron %d peticiones, quiero 1 (el primer intento se rechaza antes)", got)
	}
	// El desfase se recuerda: la siguiente ya no necesita reintento.
	if _, err := c.Instance(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestUploadCompleteAndDownload(t *testing.T) {
	c, srv := setup(t)
	ctx := context.Background()
	const partSize = 1 << 20
	pkg := randomBytes(3*partSize + 12345)
	req := openReq("enlace-uno-0001", int64(len(pkg)), partSize)

	l, err := c.OpenUpload(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if l.State != nube.StateUploading || l.PartCount != 4 || len(l.ReceivedParts) != 0 {
		t.Fatalf("apertura: %+v", l)
	}

	var last int64
	err = c.Upload(ctx, req.LinkID, bytes.NewReader(pkg), int64(len(pkg)), partSize, nil, fastRetry,
		func(d int64) { last = d })
	if err != nil {
		t.Fatal(err)
	}
	if last != int64(len(pkg)) {
		t.Errorf("progreso final %d, quiero %d", last, len(pkg))
	}

	sum := sha256.Sum256(pkg)
	l, err = c.Complete(ctx, req.LinkID, hex.EncodeToString(sum[:]))
	if err != nil || l.State != nube.StateActive {
		t.Fatalf("complete: %+v %v", l, err)
	}
	view, _ := srv.Link(req.LinkID)
	if !bytes.Equal(view.Blob, pkg) {
		t.Error("el blob ensamblado no coincide con el paquete")
	}
	// Completar otra vez con el mismo hash es idempotente.
	if _, err := c.Complete(ctx, req.LinkID, hex.EncodeToString(sum[:])); err != nil {
		t.Errorf("complete repetido: %v", err)
	}
	if _, err := c.Link(ctx, req.LinkID); err != nil {
		t.Error(err)
	}
}

func TestCompleteChecksumMismatch(t *testing.T) {
	c, _ := setup(t)
	ctx := context.Background()
	pkg := randomBytes(2 << 20)
	req := openReq("enlace-malo-001", int64(len(pkg)), 1<<20)
	if _, err := c.OpenUpload(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(ctx, req.LinkID, bytes.NewReader(pkg), int64(len(pkg)), 1<<20, nil, fastRetry, nil); err != nil {
		t.Fatal(err)
	}
	_, err := c.Complete(ctx, req.LinkID, strings.Repeat("0", 64))
	var e *nube.Error
	if !errors.As(err, &e) || e.Status != http.StatusUnprocessableEntity {
		t.Fatalf("se esperaba 422: %v", err)
	}
	if l, _ := c.Link(ctx, req.LinkID); l.State != nube.StateFailed {
		t.Errorf("estado %q, quiero failed", l.State)
	}
}

func TestResumeSkipsReceivedParts(t *testing.T) {
	c, srv := setup(t)
	ctx := context.Background()
	const partSize = 1 << 20
	pkg := randomBytes(4 * partSize)
	req := openReq("enlace-reanuda1", int64(len(pkg)), partSize)
	if _, err := c.OpenUpload(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Primera subida: la parte 3 falla con un error no transitorio.
	srv.Lock()
	srv.Fail = func(r *http.Request) (int, string) {
		if strings.HasSuffix(r.URL.Path, "/parts/3") {
			return http.StatusBadRequest, "invalid_request"
		}
		return 0, ""
	}
	srv.Unlock()
	if err := c.Upload(ctx, req.LinkID, bytes.NewReader(pkg), int64(len(pkg)), partSize, nil, fastRetry, nil); !nube.IsCode(err, "invalid_request") {
		t.Fatalf("se esperaba el fallo inyectado: %v", err)
	}
	srv.Lock()
	srv.Fail = nil
	srv.Unlock()

	// Reabrir con el mismo cuerpo devuelve lo recibido; subir solo lo que falta.
	l, err := c.OpenUpload(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.ReceivedParts) != 2 {
		t.Fatalf("partes recibidas %v, quiero [1 2]", l.ReceivedParts)
	}
	before := srv.Requests("PUT /v1/links/{id}/parts/{n}")
	if err := c.Upload(ctx, req.LinkID, bytes.NewReader(pkg), int64(len(pkg)), partSize, l.ReceivedParts, fastRetry, nil); err != nil {
		t.Fatal(err)
	}
	if got := srv.Requests("PUT /v1/links/{id}/parts/{n}") - before; got != 2 {
		t.Errorf("se subieron %d partes al reanudar, quiero 2", got)
	}
	sum := sha256.Sum256(pkg)
	if _, err := c.Complete(ctx, req.LinkID, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}

	// Mismo linkId con otro cuerpo: conflict.
	other := openReq(req.LinkID, int64(len(pkg)), partSize)
	if _, err := c.OpenUpload(ctx, other); !nube.IsCode(err, "conflict") {
		t.Errorf("cuerpo distinto: %v", err)
	}
}

func TestTransientErrorsAreRetried(t *testing.T) {
	c, srv := setup(t)
	ctx := context.Background()
	pkg := randomBytes(1 << 20)
	req := openReq("enlace-reintento", int64(len(pkg)), 1<<20)
	if _, err := c.OpenUpload(ctx, req); err != nil {
		t.Fatal(err)
	}
	fails := 2
	srv.Lock()
	srv.Fail = func(r *http.Request) (int, string) {
		if r.Method == http.MethodPut && fails > 0 {
			fails--
			if fails == 1 {
				return http.StatusServiceUnavailable, "internal_error"
			}
			return http.StatusTooManyRequests, "rate_limited"
		}
		return 0, ""
	}
	srv.Unlock()
	if err := c.Upload(ctx, req.LinkID, bytes.NewReader(pkg), int64(len(pkg)), 1<<20, nil, fastRetry, nil); err != nil {
		t.Fatalf("los errores transitorios deberían reintentarse: %v", err)
	}

	// Con la nube caída del todo, se rinde y devuelve ErrUnreachable.
	srv.Lock()
	srv.Fail = func(*http.Request) (int, string) { return http.StatusBadGateway, "internal_error" }
	srv.Unlock()
	err := fastRetry.Do(ctx, func() error { _, err := c.Instance(ctx); return err })
	if !errors.Is(err, nube.ErrUnreachable) {
		t.Errorf("se esperaba ErrUnreachable: %v", err)
	}
}

func TestUnreachableErrorHidesURL(t *testing.T) {
	key := randomBytes(32)
	c, _ := nube.New("http://127.0.0.1:1", instanceID, key)
	_, err := c.Instance(context.Background())
	if !errors.Is(err, nube.ErrUnreachable) {
		t.Fatalf("se esperaba ErrUnreachable: %v", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "/v1/") {
		t.Errorf("el error revela la URL: %v", err)
	}
}

func TestRevokeListAndExpiry(t *testing.T) {
	c, srv := setup(t)
	ctx := context.Background()
	ids := []string{"enlace-lista-01", "enlace-lista-02", "enlace-lista-03"}
	for _, id := range ids {
		if _, err := c.OpenUpload(ctx, openReq(id, 1<<20, 1<<20)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Revoke(ctx, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(ctx, ids[0]); err != nil {
		t.Errorf("revocar dos veces debe ser idempotente: %v", err)
	}
	if err := c.Revoke(ctx, "nunca-existio-01"); !nube.IsCode(err, "not_found") {
		t.Errorf("enlace inexistente: %v", err)
	}

	page, err := c.Links(ctx, "", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].LinkID != ids[2] || page.NextCursor == nil {
		t.Fatalf("primera página: %+v", page)
	}
	page, err = c.Links(ctx, "", *page.NextCursor, 2)
	if err != nil || len(page.Items) != 1 || page.Items[0].State != nube.StateRevoked || page.NextCursor != nil {
		t.Fatalf("segunda página: %+v %v", page, err)
	}

	// La nube vence el enlace por su cuenta.
	srv.Lock()
	srv.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	srv.Unlock()
	if v, _ := srv.Link(ids[1]); v.State != nube.StateExpired {
		t.Errorf("estado %q, quiero expired", v.State)
	}
}

func TestInvalidLinkIDNeverSent(t *testing.T) {
	c, srv := setup(t)
	ctx := context.Background()
	for _, id := range []string{"corto", "con/barra-0000", "con%2Fcodificada", strings.Repeat("a", 65)} {
		if _, err := c.Link(ctx, id); err == nil {
			t.Errorf("%q aceptado", id)
		}
		if err := c.Revoke(ctx, id); err == nil {
			t.Errorf("%q aceptado al revocar", id)
		}
	}
	if n := srv.Requests("GET /v1/links/{id}") + srv.Requests("DELETE /v1/links/{id}"); n != 0 {
		t.Errorf("llegaron %d peticiones con linkId inválido", n)
	}
}
