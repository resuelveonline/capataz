package nubetest_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/nube"
	"github.com/resuelveonline/capataz/capataz/internal/nube/nubetest"
)

const instanceID = "instanciadeprueba0000000000"

type rangeCase struct {
	Headers []string `json:"headers"`
	Status  int      `json:"status"`
	Start   int64    `json:"start"`
	End     int64    `json:"end"`
}

func loadRangeVectors(t *testing.T) (int64, []rangeCase) {
	t.Helper()
	data, err := os.ReadFile("../../../../docs/vectores-range.json")
	if err != nil {
		t.Fatal(err)
	}
	var d struct {
		Size  int64       `json:"size"`
		Cases []rangeCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatal(err)
	}
	return d.Size, d.Cases
}

func TestParseRangeVectors(t *testing.T) {
	size, cases := loadRangeVectors(t)
	for _, c := range cases {
		start, end, partial, ok := nubetest.ParseRange(c.Headers, size)
		status := 416
		if ok {
			status = 200
			if partial {
				status = 206
			}
		}
		if status != c.Status || ok && (start != c.Start || end != c.End) {
			t.Errorf("%q: %d %d-%d; el vector dice %d %d-%d", c.Headers, status, start, end, c.Status, c.Start, c.End)
		}
	}
}

// activeLink sube un paquete aleatorio y devuelve el token y los bytes.
func activeLink(t *testing.T, srv *nubetest.Server, c *nube.Client, id string, size int, maxDownloads *int) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	pkg := make([]byte, size)
	rand.Read(pkg)
	tokRaw := make([]byte, 32)
	rand.Read(tokRaw)
	token := base64.RawURLEncoding.EncodeToString(tokRaw)
	partSize := 1 << 20
	req := nube.OpenUploadRequest{LinkID: id, TokenHash: nube.TokenHash(token), ExpiresAt: time.Now().Add(2 * time.Hour),
		MaxDownloads: maxDownloads, CipherVersion: "cpk1", EncryptedSize: int64(size), PartSize: partSize, MetaBlob: "AAAA"}
	if _, err := c.OpenUpload(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := c.Upload(ctx, id, bytes.NewReader(pkg), int64(size), partSize, nil, nube.Retry{Attempts: 1}, nil); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(pkg)
	if _, err := c.Complete(ctx, id, hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	return token, pkg
}

func setup(t *testing.T) (*nubetest.Server, *nube.Client) {
	t.Helper()
	key := make([]byte, 32)
	rand.Read(key)
	srv := nubetest.New(instanceID, key)
	t.Cleanup(srv.Close)
	c, err := nube.New(srv.URL, instanceID, key)
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func get(t *testing.T, method, url string, rng ...string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	for _, r := range rng {
		req.Header.Add("Range", r)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestBlobRandomRanges(t *testing.T) {
	srv, c := setup(t)
	size := 3<<20 + 12345 // cuatro partes de 1 MiB
	token, pkg := activeLink(t, srv, c, "enlace-rango-001", size, nil)
	url := srv.URL + "/s/" + token + "/blob"
	r := mrand.New(mrand.NewPCG(1, 2))
	for i := range 300 {
		a := r.Int64N(int64(size))
		b := a + r.Int64N(int64(size)-a)
		spec := fmt.Sprintf("bytes=%d-%d", a, b)
		switch i % 3 {
		case 1:
			spec, b = fmt.Sprintf("bytes=%d-", a), int64(size)-1
		case 2:
			n := 1 + r.Int64N(int64(size))
			spec, a, b = fmt.Sprintf("bytes=-%d", n), int64(size)-n, int64(size)-1
		}
		resp, body := get(t, "GET", url, spec)
		want := fmt.Sprintf("bytes %d-%d/%d", a, b, size)
		if resp.StatusCode != 206 || resp.Header.Get("Content-Range") != want || !bytes.Equal(body, pkg[a:b+1]) ||
			resp.Header.Get("Content-Length") != strconv.FormatInt(b-a+1, 10) {
			t.Fatalf("%s: %d %q, %d bytes", spec, resp.StatusCode, resp.Header.Get("Content-Range"), len(body))
		}
	}
	resp, body := get(t, "GET", url)
	if resp.StatusCode != 200 || !bytes.Equal(body, pkg) || resp.Header.Get("Accept-Ranges") != "bytes" ||
		resp.Header.Get("Content-Disposition") != "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("sin Range: %d, cabeceras %v", resp.StatusCode, resp.Header)
	}
	resp, _ = get(t, "GET", url, "bytes=0-1,4-5")
	if resp.StatusCode != 416 || resp.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", size) {
		t.Errorf("varios intervalos: %d %q", resp.StatusCode, resp.Header.Get("Content-Range"))
	}
}

func TestDownloadCountingAndExhaustion(t *testing.T) {
	srv, c := setup(t)
	two := 2
	token, pkg := activeLink(t, srv, c, "enlace-cupo-0001", 1000, &two)
	base := srv.URL + "/s/" + token

	// HEAD no cuenta; las continuaciones tampoco.
	if resp, body := get(t, "HEAD", base+"/blob"); resp.StatusCode != 200 || len(body) != 0 {
		t.Fatalf("HEAD: %d", resp.StatusCode)
	}
	get(t, "GET", base+"/blob", "bytes=500-")
	if v, _ := srv.Link("enlace-cupo-0001"); v.Downloads != 0 {
		t.Fatalf("HEAD o continuación contaron: %d", v.Downloads)
	}
	get(t, "GET", base+"/blob")              // 1
	get(t, "GET", base+"/blob", "bytes=0-9") // 2: empieza en 0, cuenta y agota
	if v, _ := srv.Link("enlace-cupo-0001"); v.Downloads != 2 || v.State != nube.StateExhausted {
		t.Fatalf("tras dos descargas: %+v", v)
	}
	for _, rng := range [][]string{nil, {"bytes=0-"}, {"bytes=-1000"}} {
		if resp, _ := get(t, "GET", base+"/blob", rng...); resp.StatusCode != 410 {
			t.Errorf("%v sobre agotado: %d", rng, resp.StatusCode)
		}
	}
	if resp, _ := get(t, "HEAD", base+"/blob"); resp.StatusCode != 410 {
		t.Errorf("HEAD sobre agotado: %d", resp.StatusCode)
	}
	if resp, _ := get(t, "GET", base+"/meta"); resp.StatusCode != 410 {
		t.Errorf("meta sobre agotado: %d", resp.StatusCode)
	}
	// Una continuación de la última descarga se sigue sirviendo...
	if resp, body := get(t, "GET", base+"/blob", "bytes=10-"); resp.StatusCode != 206 || !bytes.Equal(body, pkg[10:]) {
		t.Errorf("continuación dentro del margen: %d", resp.StatusCode)
	}
	// ...hasta que pasa el margen.
	srv.Lock()
	srv.Now = func() time.Time { return time.Now().Add(61 * time.Minute) }
	srv.Unlock()
	if resp, _ := get(t, "GET", base+"/blob", "bytes=10-"); resp.StatusCode != 410 {
		t.Errorf("continuación fuera del margen: %d", resp.StatusCode)
	}
	// Al vencer, expired.
	srv.Lock()
	srv.Now = func() time.Time { return time.Now().Add(3 * time.Hour) }
	srv.Unlock()
	if v, _ := srv.Link("enlace-cupo-0001"); v.State != nube.StateExpired {
		t.Errorf("agotado y vencido: %q", v.State)
	}
}

func TestPublicStates(t *testing.T) {
	srv, c := setup(t)
	ctx := context.Background()
	tokRaw := make([]byte, 32)
	rand.Read(tokRaw)
	token := base64.RawURLEncoding.EncodeToString(tokRaw)
	req := nube.OpenUploadRequest{LinkID: "enlace-estados01", TokenHash: nube.TokenHash(token), ExpiresAt: time.Now().Add(2 * time.Hour),
		CipherVersion: "cpk1", EncryptedSize: 2 << 20, PartSize: 1 << 20, MetaBlob: "AAAA"}
	if _, err := c.OpenUpload(ctx, req); err != nil {
		t.Fatal(err)
	}
	base := srv.URL + "/s/" + token
	for _, m := range []string{"GET", "HEAD"} {
		for _, p := range []string{"/meta", "/blob"} {
			if resp, _ := get(t, m, base+p); resp.StatusCode != 409 {
				t.Errorf("uploading %s %s: %d", m, p, resp.StatusCode)
			}
		}
	}
	if resp, _ := get(t, "GET", srv.URL+"/s/"+token[:42]+"x/meta"); resp.StatusCode != 404 {
		t.Errorf("token desconocido: %d", resp.StatusCode)
	}
	// Hash equivocado: failed, y en público como si no existiera.
	c.Upload(ctx, req.LinkID, bytes.NewReader(make([]byte, 2<<20)), 2<<20, 1<<20, nil, nube.Retry{Attempts: 1}, nil)
	c.Complete(ctx, req.LinkID, hex.EncodeToString(make([]byte, 32)))
	for _, m := range []string{"GET", "HEAD"} {
		if resp, _ := get(t, m, base+"/meta"); resp.StatusCode != 404 {
			t.Errorf("failed %s: %d", m, resp.StatusCode)
		}
	}

	token2, _ := activeLink(t, srv, c, "enlace-estados02", 100, nil)
	c.Revoke(ctx, "enlace-estados02")
	for _, m := range []string{"GET", "HEAD"} {
		if resp, _ := get(t, m, srv.URL+"/s/"+token2+"/blob"); resp.StatusCode != 410 {
			t.Errorf("revocado %s: %d", m, resp.StatusCode)
		}
	}
}
