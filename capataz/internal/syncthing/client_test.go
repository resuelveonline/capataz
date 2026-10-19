package syncthing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const apiKey = "clave-secreta-de-syncthing"

// fakeSyncthing responde como Syncthing v2.1.6 y registra la última petición.
type fakeSyncthing struct {
	t       *testing.T
	last    *http.Request
	handler http.HandlerFunc
}

func newFake(t *testing.T, h http.HandlerFunc) (*Client, *fakeSyncthing) {
	t.Helper()
	f := &fakeSyncthing{t: t, handler: h}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.last = r.Clone(context.Background())
		if r.Header.Get("X-API-Key") != apiKey {
			// Respuesta real de Syncthing sin API key.
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "Forbidden\n")
			return
		}
		f.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL+"/", apiKey)
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

// plain imita http.Error de Syncthing: text/plain y salto de línea final.
func plain(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	io.WriteString(w, body+"\n")
}

func jsonBody(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, body)
}

// Copia reducida de la respuesta real de db/file para "nuevo.txt".
const fileJSON = `{
  "availability": null,
  "global": {"deleted": false, "ignored": false, "invalid": false, "modified": "2026-10-18T00:24:46.361255153-06:00",
             "mustRescan": false, "name": "nuevo.txt", "sequence": 5, "size": 6, "type": "FILE_INFO_TYPE_FILE",
             "version": ["ICY7WEU:1792304686"]},
  "local":  {"deleted": false, "ignored": false, "invalid": false, "modified": "2026-10-18T00:24:46.361255153-06:00",
             "mustRescan": false, "name": "nuevo.txt", "sequence": 5, "size": 6, "type": "FILE_INFO_TYPE_FILE",
             "version": ["ICY7WEU:1792304686"]}
}`

func TestFolders(t *testing.T) {
	c, f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, `[{"id":"fgyww-edjwa","label":"pruebas","path":"~/capataz-dev/sync/pruebas",
		              "type":"sendreceive","paused":false,"filesystemType":"basic","devices":[]}]`)
	})
	got, err := c.Folders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.last.Method != "GET" || f.last.URL.Path != "/rest/config/folders" {
		t.Errorf("petición %s %s", f.last.Method, f.last.URL.Path)
	}
	want := Folder{ID: "fgyww-edjwa", Label: "pruebas", Path: "~/capataz-dev/sync/pruebas", Type: "sendreceive", FilesystemType: "basic"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("Folders = %+v", got)
	}
}

func TestFile(t *testing.T) {
	c, f := newFake(t, func(w http.ResponseWriter, r *http.Request) { jsonBody(w, fileJSON) })
	got, err := c.File(context.Background(), "fgyww-edjwa", "nuevo.txt")
	if err != nil {
		t.Fatal(err)
	}
	if f.last.URL.Path != "/rest/db/file" {
		t.Errorf("ruta %s", f.last.URL.Path)
	}
	wantMod := time.Date(2026, 10, 18, 6, 24, 46, 361255153, time.UTC)
	if got.Global.Name != "nuevo.txt" || got.Global.Size != 6 || !got.Global.Modified.Equal(wantMod) || got.Local.Sequence != 5 {
		t.Errorf("File = %+v", got)
	}
}

func TestFileEncodesPath(t *testing.T) {
	c, f := newFake(t, func(w http.ResponseWriter, r *http.Request) { jsonBody(w, fileJSON) })
	name := "expedientes 2026/año & ñ/a+b?.pdf"
	if _, err := c.File(context.Background(), "fgyww-edjwa", name); err != nil {
		t.Fatal(err)
	}
	if got := f.last.URL.Query().Get("file"); got != name {
		t.Errorf("Syncthing recibió file=%q; quiero %q", got, name)
	}
	// Valor crudo de file= en la consulta: "/" de subcarpetas, espacios, "&",
	// "+", "?" y "ñ" deben ir codificados.
	var raw string
	for kv := range strings.SplitSeq(f.last.URL.RawQuery, "&") {
		if v, ok := strings.CutPrefix(kv, "file="); ok {
			raw = v
		}
	}
	if raw == "" || strings.ContainsAny(raw, " /&?ñ") || strings.Contains(raw, "a+b") {
		t.Errorf("file= sin codificar: %q", raw)
	}
}

// Respuesta real (curl -i) de Syncthing v2.1.6 para un archivo fuera del
// índice: HTTP 404, Content-Type text/plain, cuerpo
// "No such object in the index\n" (28 bytes).
func TestFileNotIndexed(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		plain(w, http.StatusNotFound, "No such object in the index")
	})
	_, err := c.File(context.Background(), "fgyww-edjwa", "recien-creado.txt")
	if !errors.Is(err, ErrNotIndexed) {
		t.Errorf("error = %v; quiero ErrNotIndexed", err)
	}
}

// Se exigen código y texto: otro 404 no es "no indexado".
func TestFileOther404(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		plain(w, http.StatusNotFound, "404 page not found")
	})
	_, err := c.File(context.Background(), "fgyww-edjwa", "x.txt")
	if err == nil || errors.Is(err, ErrNotIndexed) || errors.Is(err, ErrUnavailable) {
		t.Errorf("error = %v; quiero un error genérico", err)
	}
}

// El mismo texto con 200 o en JSON tampoco es "no indexado".
func TestFileNotIndexedNeedsStatus(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "No such object in the index\n")
	})
	if _, err := c.File(context.Background(), "f", "x"); errors.Is(err, ErrNotIndexed) {
		t.Error("sin text/plain no debe reconocerse como no indexado")
	}
}

func TestFileDeleted(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, `{"global":{"name":"borrado.txt","deleted":true,"size":0},"local":{"name":"borrado.txt","deleted":true}}`)
	})
	got, err := c.File(context.Background(), "fgyww-edjwa", "borrado.txt")
	if err != nil {
		t.Fatalf("un archivo borrado no es error del cliente: %v", err)
	}
	if !got.Global.Deleted || !got.Local.Deleted {
		t.Errorf("Deleted no se decodificó: %+v", got)
	}
}

func TestScan(t *testing.T) {
	c, f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if err := c.Scan(context.Background(), "fgyww-edjwa", "docs/a b.txt"); err != nil {
		t.Fatal(err)
	}
	q := f.last.URL.Query()
	if f.last.Method != "POST" || f.last.URL.Path != "/rest/db/scan" || q.Get("folder") != "fgyww-edjwa" || q.Get("sub") != "docs/a b.txt" {
		t.Errorf("petición %s %s?%s", f.last.Method, f.last.URL.Path, f.last.URL.RawQuery)
	}

	if err := c.Scan(context.Background(), "fgyww-edjwa", ""); err != nil {
		t.Fatal(err)
	}
	if f.last.URL.Query().Has("sub") {
		t.Error("sub vacío no debe enviarse")
	}
}

func TestNoSuchFolder(t *testing.T) {
	// Respuestas reales: db/need da 404 y db/scan da 500, ambas "no such folder".
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusNotFound
		if r.URL.Path == "/rest/db/scan" {
			code = http.StatusInternalServerError
		}
		plain(w, code, "no such folder")
	})
	if _, err := c.Need(context.Background(), "no-existe"); !errors.Is(err, ErrNoSuchFolder) {
		t.Errorf("Need: %v", err)
	}
	if err := c.Scan(context.Background(), "no-existe", ""); !errors.Is(err, ErrNoSuchFolder) {
		t.Errorf("Scan: %v", err)
	}
}

func TestNeed(t *testing.T) {
	c, f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		jsonBody(w, `{"page":1,"perpage":65536,"progress":[{"name":"grande.iso","size":100}],"queued":[],"rest":[{"name":"b.txt"}]}`)
	})
	got, err := c.Need(context.Background(), "fgyww-edjwa")
	if err != nil {
		t.Fatal(err)
	}
	if f.last.URL.Path != "/rest/db/need" || f.last.URL.Query().Get("folder") != "fgyww-edjwa" {
		t.Errorf("petición %s?%s", f.last.URL.Path, f.last.URL.RawQuery)
	}
	if len(got.Progress) != 1 || got.Progress[0].Name != "grande.iso" || len(got.Rest) != 1 || got.PerPage != 65536 {
		t.Errorf("Need = %+v", got)
	}
}

func TestUnavailable(t *testing.T) {
	t.Run("api key rechazado", func(t *testing.T) {
		c, _ := newFake(t, nil)
		c.apiKey = "otra-clave-incorrecta"
		_, err := c.Folders(context.Background())
		if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "403") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("error 500", func(t *testing.T) {
		c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) { plain(w, 500, "boom") })
		if _, err := c.Folders(context.Background()); !errors.Is(err, ErrUnavailable) {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("sin conexión", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close() // puerto cerrado
		c, _ := New(srv.URL, apiKey)
		_, err := c.File(context.Background(), "fgyww-edjwa", "expediente-confidencial.pdf")
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
		msg := err.Error()
		for _, leak := range []string{"expediente-confidencial", apiKey, "folder=", srv.URL} {
			if strings.Contains(msg, leak) {
				t.Errorf("el error filtra %q: %s", leak, msg)
			}
		}
	})
	t.Run("plazo vencido", func(t *testing.T) {
		c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := c.Folders(ctx); !errors.Is(err, ErrUnavailable) {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("redirección rechazada", func(t *testing.T) {
		c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://otro-host.invalid/robar", http.StatusFound)
		})
		if _, err := c.Folders(context.Background()); err == nil {
			t.Error("una redirección no debe seguirse")
		}
	})
}

func TestBadJSON(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) { jsonBody(w, "{no es json") })
	if _, err := c.Folders(context.Background()); err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("error = %v", err)
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, u := range []string{"", "127.0.0.1:8384", "ftp://x", "http://"} {
		if _, err := New(u, apiKey); err == nil {
			t.Errorf("New(%q) debería fallar", u)
		}
	}
}
