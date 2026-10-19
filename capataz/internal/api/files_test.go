package api

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

const folderID = "fgyww-edjwa"

var fixedTime = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// filesEnv crea una carpeta sincronizada de prueba:
//
//	base/fuera/secreto.txt
//	base/pruebas/
//	  Zeta.txt (5 B)  alfa.txt (4 B)  página.html  expediente-confidencial.pdf
//	  docs/  docs/b.txt  Carpeta/
//	  .stfolder/  .stversions/v.txt  .stignore  .syncthing.c.tmp  ~syncthing~d.tmp
//	  enlace-dentro -> alfa.txt      enlace-fuera -> ../fuera/secreto.txt
//	  enlace-dir-fuera -> ../fuera   fifo
func filesEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "pruebas")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"fuera", "pruebas/docs", "pruebas/Carpeta", "pruebas/.stfolder", "pruebas/.stversions"} {
		must(os.MkdirAll(filepath.Join(base, d), 0o755))
	}
	for name, content := range map[string]string{
		"fuera/secreto.txt":                   "secreto",
		"pruebas/Zeta.txt":                    "zzzzz",
		"pruebas/alfa.txt":                    "alfa",
		"pruebas/página.html":                 "<script>alert(1)</script>",
		"pruebas/expediente-confidencial.pdf": "%PDF",
		"pruebas/docs/b.txt":                  "b",
		"pruebas/.stversions/v.txt":           "v",
		"pruebas/.stignore":                   "*.bak",
		"pruebas/.syncthing.c.tmp":            "parcial",
		"pruebas/~syncthing~d.tmp":            "parcial",
	} {
		p := filepath.Join(base, name)
		must(os.WriteFile(p, []byte(content), 0o644))
		must(os.Chtimes(p, fixedTime, fixedTime))
	}
	for name, target := range map[string]string{
		"enlace-dentro":    "alfa.txt",
		"enlace-fuera":     "../fuera/secreto.txt",
		"enlace-dir-fuera": "../fuera",
	} {
		must(os.Symlink(target, filepath.Join(dir, name)))
	}
	must(syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))

	return newTestEnv(t, syncthing.Folder{ID: folderID, Label: "pruebas", Path: dir}), dir
}

// filesURL arma la URL con la consulta codificada como lo haría un cliente.
func filesURL(endpoint, folder, p string) string {
	return endpoint + "?" + url.Values{"folder": {folder}, "path": {p}}.Encode()
}

func listNames(t *testing.T, w *httptest.ResponseRecorder) []entryDTO {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("estado %d: %s", w.Code, w.Body.String())
	}
	var out []entryDTO
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFilesListRoot(t *testing.T) {
	e, _ := filesEnv(t)
	got := listNames(t, e.do("GET", "/files?folder="+folderID, true))

	var names []string
	for _, d := range got {
		names = append(names, d.Name)
	}
	// Carpetas primero, luego por nombre sin distinguir mayúsculas. Sin
	// ocultos, sin enlaces hacia fuera y sin el FIFO.
	want := "Carpeta,docs,alfa.txt,enlace-dentro,expediente-confidencial.pdf,página.html,Zeta.txt"
	if strings.Join(names, ",") != want {
		t.Errorf("listado %v\n         quiero %s", names, want)
	}

	byName := map[string]entryDTO{}
	for _, d := range got {
		byName[d.Name] = d
	}
	if d := byName["Zeta.txt"]; d.Path != "Zeta.txt" || d.IsDir || d.Size != 5 || !d.Modified.Equal(fixedTime) {
		t.Errorf("Zeta.txt = %+v", d)
	}
	if d := byName["docs"]; !d.IsDir || d.Size != 0 || d.Path != "docs" {
		t.Errorf("docs = %+v", d)
	}
	if d := byName["enlace-dentro"]; d.IsDir || d.Size != 4 {
		t.Errorf("enlace-dentro debe resolverse al archivo: %+v", d)
	}
}

func TestFilesListJSONShape(t *testing.T) {
	e, _ := filesEnv(t)
	w := e.do("GET", filesURL("/files", folderID, "docs"), true)
	want := `[{"name":"b.txt","path":"docs/b.txt","isDir":false,"size":1,"modified":"2026-10-18T12:00:00Z"}]`
	if got := strings.TrimSpace(w.Body.String()); got != want {
		t.Errorf("cuerpo %s\n quiero %s", got, want)
	}
}

func TestFilesListEmptyDirIsArray(t *testing.T) {
	e, _ := filesEnv(t)
	w := e.do("GET", filesURL("/files", folderID, "Carpeta"), true)
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("cuerpo %q; quiero []", got)
	}
}

func TestFilesErrors(t *testing.T) {
	e, _ := filesEnv(t)
	tests := []struct {
		name   string
		target string // URL cruda
		status int
		code   string
	}{
		{"sin folder", "/files?path=docs", 400, codeInvalidRequest},
		{"carpeta desconocida", "/files?folder=no-existe", 404, codeNotFound},
		{"archivo en vez de carpeta", filesURL("/files", folderID, "alfa.txt"), 400, codeInvalidRequest},
		{"no existe", filesURL("/files", folderID, "no-existe"), 404, codeNotFound},
		{"oculto .stversions", filesURL("/files", folderID, ".stversions"), 404, codeNotFound},
		{"oculto en mayúsculas", filesURL("/files", folderID, ".STVERSIONS"), 404, codeNotFound},
		{"oculto .stfolder", filesURL("/files", folderID, ".stfolder"), 404, codeNotFound},
		{"..", filesURL("/files", folderID, ".."), 400, codePathNotAllowed},
		{"../fuera", filesURL("/files", folderID, "../fuera"), 400, codePathNotAllowed},
		{"docs/../..", filesURL("/files", folderID, "docs/../.."), 400, codePathNotAllowed},
		{"absoluta", filesURL("/files", folderID, "/etc"), 400, codePathNotAllowed},
		{"%2e%2e%2f codificado", "/files?folder=" + folderID + "&path=%2e%2e%2ffuera", 400, codePathNotAllowed},
		{"%2E%2E mayúsculas", "/files?folder=" + folderID + "&path=%2E%2E", 400, codePathNotAllowed},
		{"byte nulo %00", "/files?folder=" + folderID + "&path=docs%00", 400, codePathNotAllowed},
		{"barra invertida %5C", "/files?folder=" + folderID + "&path=..%5Cfuera", 400, codePathNotAllowed},
		{"enlace a carpeta externa", filesURL("/files", folderID, "enlace-dir-fuera"), 400, codePathNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, e.do("GET", tt.target, true), tt.status, tt.code)
		})
	}
}

func TestFilesRequiresAuth(t *testing.T) {
	e, _ := filesEnv(t)
	wantError(t, e.do("GET", "/files?folder="+folderID, false), 401, codeUnauthorized)
	wantError(t, e.do("GET", filesURL("/files/download", folderID, "alfa.txt"), false), 401, codeUnauthorized)
}

func TestFilesSyncthingDown(t *testing.T) {
	e, _ := filesEnv(t)
	e.st.err = fmt.Errorf("syncthing config/folders: %w", syncthing.ErrUnavailable)
	wantError(t, e.do("GET", "/files?folder="+folderID, true), 502, codeSyncthingUnavailable)
	wantError(t, e.do("GET", filesURL("/files/download", folderID, "alfa.txt"), true), 502, codeSyncthingUnavailable)
}

func TestFilesFolderMissingOnDisk(t *testing.T) {
	e := newTestEnv(t, syncthing.Folder{ID: folderID, Label: "pruebas", Path: filepath.Join(t.TempDir(), "borrada")})
	wantError(t, e.do("GET", "/files?folder="+folderID, true), 500, codeInternal)
}

func TestDownload(t *testing.T) {
	e, _ := filesEnv(t)
	w := e.do("GET", filesURL("/files/download", folderID, "Zeta.txt"), true)
	if w.Code != 200 || w.Body.String() != "zzzzz" {
		t.Fatalf("estado %d, cuerpo %q", w.Code, w.Body.String())
	}
	for k, want := range map[string]string{
		"Content-Type":           "application/octet-stream",
		"Content-Length":         "5",
		"Accept-Ranges":          "bytes",
		"Content-Disposition":    `attachment; filename=Zeta.txt`,
		"X-Content-Type-Options": "nosniff",
		"Last-Modified":          fixedTime.Format(http.TimeFormat),
	} {
		if got := w.Header().Get(k); got != want {
			t.Errorf("%s = %q; quiero %q", k, got, want)
		}
	}
}

func TestDownloadHTMLIsNotRendered(t *testing.T) {
	e, _ := filesEnv(t)
	w := e.do("GET", filesURL("/files/download", folderID, "página.html"), true)
	if ct := w.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type %q: un .html no debe servirse como HTML", ct)
	}
	// Nombre no ASCII codificado según RFC 2231/6266.
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename*=utf-8''p%C3%A1gina.html` {
		t.Errorf("Content-Disposition %q", cd)
	}
}

func TestDownloadRange(t *testing.T) {
	e, _ := filesEnv(t)
	target := filesURL("/files/download", folderID, "Zeta.txt")

	w := e.do("GET", target, true, "Range", "bytes=1-3")
	if w.Code != 206 || w.Body.String() != "zzz" || w.Header().Get("Content-Range") != "bytes 1-3/5" {
		t.Errorf("Range: estado %d, cuerpo %q, Content-Range %q", w.Code, w.Body.String(), w.Header().Get("Content-Range"))
	}

	// Reanudar: If-Range con la fecha vigente respeta el Range...
	w = e.do("GET", target, true, "Range", "bytes=3-", "If-Range", fixedTime.Format(http.TimeFormat))
	if w.Code != 206 || w.Body.String() != "zz" {
		t.Errorf("If-Range vigente: estado %d, cuerpo %q", w.Code, w.Body.String())
	}
	// ...y si el archivo cambió desde entonces, se envía completo.
	w = e.do("GET", target, true, "Range", "bytes=3-", "If-Range", fixedTime.Add(-time.Hour).Format(http.TimeFormat))
	if w.Code != 200 || w.Body.String() != "zzzzz" {
		t.Errorf("If-Range caducado: estado %d, cuerpo %q", w.Code, w.Body.String())
	}
}

// Range fuera del archivo: 416 con Content-Range "bytes */<tamaño>" y el
// JSON del contrato (invalid_request), no el texto plano de ServeContent.
func TestDownloadRangeNotSatisfiable(t *testing.T) {
	e, _ := filesEnv(t)
	w := e.do("GET", filesURL("/files/download", folderID, "Zeta.txt"), true, "Range", "bytes=100-200")
	wantError(t, w, 416, codeInvalidRequest)
	if cr := w.Header().Get("Content-Range"); cr != "bytes */5" {
		t.Errorf("Content-Range %q; quiero \"bytes */5\"", cr)
	}
	if strings.Contains(w.Body.String(), "invalid range") {
		t.Error("se coló el cuerpo de texto de ServeContent")
	}
}

func TestDownloadHead(t *testing.T) {
	e, _ := filesEnv(t)
	w := e.do("HEAD", filesURL("/files/download", folderID, "Zeta.txt"), true)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "5" {
		t.Errorf("HEAD: estado %d, cuerpo %d bytes, Content-Length %q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
}

func TestDownloadErrors(t *testing.T) {
	e, _ := filesEnv(t)
	tests := []struct {
		name   string
		target string
		status int
		code   string
	}{
		{"sin path", "/files/download?folder=" + folderID, 400, codeInvalidRequest},
		{"sin folder", "/files/download?path=alfa.txt", 400, codeInvalidRequest},
		{"carpeta", filesURL("/files/download", folderID, "docs"), 400, codeInvalidRequest},
		{"raíz", filesURL("/files/download", folderID, "."), 400, codeInvalidRequest},
		{"carpeta desconocida", filesURL("/files/download", "no-existe", "alfa.txt"), 404, codeNotFound},
		{"no existe", filesURL("/files/download", folderID, "no.txt"), 404, codeNotFound},
		{"fifo", filesURL("/files/download", folderID, "fifo"), 404, codeNotFound},
		{"temporal .syncthing", filesURL("/files/download", folderID, ".syncthing.c.tmp"), 404, codeNotFound},
		{"temporal ~syncthing~", filesURL("/files/download", folderID, "~syncthing~d.tmp"), 404, codeNotFound},
		{".stignore", filesURL("/files/download", folderID, ".stignore"), 404, codeNotFound},
		{".stversions/v.txt", filesURL("/files/download", folderID, ".stversions/v.txt"), 404, codeNotFound},
		{".stfolder", filesURL("/files/download", folderID, ".stfolder"), 404, codeNotFound},
		{"enlace fuera", filesURL("/files/download", folderID, "enlace-fuera"), 400, codePathNotAllowed},
		{"por carpeta enlazada fuera", filesURL("/files/download", folderID, "enlace-dir-fuera/secreto.txt"), 400, codePathNotAllowed},
		{"../", filesURL("/files/download", folderID, "../fuera/secreto.txt"), 400, codePathNotAllowed},
		{"codificado", "/files/download?folder=" + folderID + "&path=..%2Ffuera%2Fsecreto.txt", 400, codePathNotAllowed},
		{"absoluta", filesURL("/files/download", folderID, "/etc/hosts"), 400, codePathNotAllowed},
		{"byte nulo", "/files/download?folder=" + folderID + "&path=alfa.txt%00.pdf", 400, codePathNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, e.do("GET", tt.target, true), tt.status, tt.code)
		})
	}
	// Por un enlace interno sí se puede descargar.
	if w := e.do("GET", filesURL("/files/download", folderID, "enlace-dentro"), true); w.Code != 200 || w.Body.String() != "alfa" {
		t.Errorf("enlace-dentro: %d %q", w.Code, w.Body.String())
	}
}

// La bitácora registra el patrón de ruta, estado y duración, pero nunca la
// consulta, el valor de path, nombres de archivo ni el token.
func TestLogsOmitSensitiveData(t *testing.T) {
	e, _ := filesEnv(t)
	e.do("GET", filesURL("/files", folderID, "docs"), true)
	e.do("GET", filesURL("/files/download", folderID, "expediente-confidencial.pdf"), true)
	e.do("GET", filesURL("/files/download", folderID, "expediente-confidencial.pdf"), true, "Range", "bytes=1-2")
	e.do("GET", filesURL("/files/download", folderID, "expediente-confidencial.pdf"), true, "Range", "bytes=999-")
	e.do("GET", filesURL("/files/download", folderID, "../expediente-confidencial.pdf"), true)
	e.do("GET", filesURL("/files/download", folderID, "no-existe/expediente-confidencial.pdf"), true)
	e.do("GET", "/expediente-confidencial.pdf", true) // ruta inexistente
	e.st.err = fmt.Errorf("syncthing config/folders: %w (x)", syncthing.ErrUnavailable)
	e.do("GET", filesURL("/files/download", folderID, "expediente-confidencial.pdf"), true)

	logs := e.logs.String()
	for _, want := range []string{
		`route="GET /files/download"`, `route="GET /files"`, `route="(sin ruta)"`,
		"status=200", "status=206", "status=416", "status=400", "status=404", "status=502", "duration=",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("la bitácora no contiene %q", want)
		}
	}
	for _, leak := range []string{"expediente", "confidencial", "docs", "path=", "folder=", "?", testToken, "Bearer"} {
		if strings.Contains(logs, leak) {
			t.Errorf("la bitácora filtra %q:\n%s", leak, logs)
		}
	}
}

// Con un servidor HTTP real (no Recorder), ResponseController sí aplica
// plazos: la descarga grande y la reanudación deben funcionar igual.
func TestDownloadRealServer(t *testing.T) {
	e, dir := filesEnv(t)
	big := make([]byte, 8<<20)
	rand.Read(big)
	if err := os.WriteFile(filepath.Join(dir, "grande.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(e.srv)
	defer srv.Close()

	get := func(rangeHdr string) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", srv.URL+filesURL("/files/download", folderID, "grande.bin"), nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		if rangeHdr != "" {
			req.Header.Set("Range", rangeHdr)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, body
	}

	resp, body := get("")
	if resp.StatusCode != 200 || !bytes.Equal(body, big) {
		t.Fatalf("completa: estado %d, %d bytes", resp.StatusCode, len(body))
	}
	resp, body = get(fmt.Sprintf("bytes=%d-", 5<<20))
	if resp.StatusCode != 206 || !bytes.Equal(body, big[5<<20:]) {
		t.Fatalf("reanudación: estado %d, %d bytes", resp.StatusCode, len(body))
	}
}
