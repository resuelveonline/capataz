package safepath

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestClean(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" cuando se espera ErrNotAllowed
	}{
		// Válidas
		{"", "."},
		{".", "."},
		{"a.txt", "a.txt"},
		{"docs/a.txt", "docs/a.txt"},
		{"./docs//a.txt", "docs/a.txt"},
		{"docs/", "docs"},
		{"docs/./a", "docs/a"},
		{"..a/b..", "..a/b.."}, // ".." solo es peligroso como segmento completo
		{"a/...", "a/..."},
		{"ñandú/año 2026.pdf", "ñandú/año 2026.pdf"},
		{"%2e%2e/x", "%2e%2e/x"}, // ya decodificada: es un nombre literal
		{"~/x", "~/x"},           // "~" no se expande: nombre literal

		// Rechazadas
		{"..", ""},
		{"../x", ""},
		{"a/../b", ""}, // aunque se quede dentro, se rechaza
		{"a/../../x", ""},
		{"a/..", ""},
		{"/etc/passwd", ""},
		{"/", ""},
		{"a\x00b", ""},
		{"a.txt\x00.pdf", ""},
		{`..\x`, ""},
		{`a\b`, ""},
		{"\xff\xfe", ""}, // UTF-8 inválido
	}
	for _, tt := range tests {
		got, err := Clean(tt.in)
		if tt.want == "" {
			if !errors.Is(err, ErrNotAllowed) {
				t.Errorf("Clean(%q) = %q, %v; quiero ErrNotAllowed", tt.in, got, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("Clean(%q) = %q, %v; quiero %q", tt.in, got, err, tt.want)
		}
	}
}

// Rutas tal como llegan en la cadena de consulta, antes de que net/http las
// decodifique. Lo que importa es el resultado tras decodificar.
func TestCleanEncoded(t *testing.T) {
	tests := []struct {
		raw     string
		allowed bool
	}{
		{"..%2Fx", false},
		{"%2e%2e%2fetc%2fpasswd", false},
		{"%2E%2E/x", false},
		{"%2Fetc%2Fpasswd", false},
		{"a%00b", false},
		{"a%5C..%5Cb", false}, // %5C = "\"
		{"docs%2Fa.txt", true},
		{"a%20b.txt", true},
		{"%252e%252e%252fx", true}, // doble codificación: queda el nombre literal "%2e%2e%2fx"
	}
	for _, tt := range tests {
		dec, err := url.QueryUnescape(tt.raw)
		if err != nil {
			t.Fatalf("%q: %v", tt.raw, err)
		}
		_, err = Clean(dec)
		if allowed := err == nil; allowed != tt.allowed {
			t.Errorf("%q (decodificada %q): permitida=%v, quiero %v", tt.raw, dec, allowed, tt.allowed)
		}
	}
}

func TestIsHidden(t *testing.T) {
	hidden := []string{
		".stfolder", ".stversions", ".stignore", ".STFOLDER", ".StVersions",
		".syncthing.informe.pdf.tmp", ".syncthing.a.TMP", "~syncthing~informe.pdf.tmp",
	}
	visible := []string{
		"stfolder", ".stfolder2", ".stignore.bak", ".syncthing.pdf", "syncthing.a.tmp",
		"informe.tmp", ".gitignore", "a.txt",
	}
	for _, n := range hidden {
		if !IsHidden(n) {
			t.Errorf("IsHidden(%q) = false", n)
		}
	}
	for _, n := range visible {
		if IsHidden(n) {
			t.Errorf("IsHidden(%q) = true", n)
		}
	}
	if !HasHidden(".stversions/a.txt") || !HasHidden("docs/.syncthing.x.tmp") || HasHidden("docs/a.txt") {
		t.Error("HasHidden no revisa todos los segmentos")
	}
}

// fixture crea:
//
//	base/
//	  fuera/secreto.txt              (fuera de la carpeta sincronizada)
//	  carpeta/
//	    a.txt
//	    docs/b.txt
//	    .stfolder/  .stversions/v.txt  .syncthing.c.txt.tmp
//	    enlace-dentro      -> a.txt
//	    enlace-dir-dentro  -> docs
//	    enlace-fuera       -> ../fuera/secreto.txt
//	    enlace-dir-fuera   -> ../fuera
//	    enlace-absoluto    -> /etc/hosts
//	    enlace-roto        -> no-existe
//	    bucle              -> bucle
//	    fifo               (tubería con nombre)
func fixture(t *testing.T) *Folder {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "carpeta")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"fuera", "carpeta/docs", "carpeta/.stfolder", "carpeta/.stversions"} {
		must(os.MkdirAll(filepath.Join(base, d), 0o755))
	}
	for name, content := range map[string]string{
		"fuera/secreto.txt":            "secreto",
		"carpeta/a.txt":                "hola",
		"carpeta/docs/b.txt":           "b",
		"carpeta/.stversions/v.txt":    "v",
		"carpeta/.syncthing.c.txt.tmp": "parcial",
	} {
		must(os.WriteFile(filepath.Join(base, name), []byte(content), 0o644))
	}
	for name, target := range map[string]string{
		"enlace-dentro":     "a.txt",
		"enlace-dir-dentro": "docs",
		"enlace-fuera":      "../fuera/secreto.txt",
		"enlace-dir-fuera":  "../fuera",
		"enlace-absoluto":   "/etc/hosts",
		"enlace-roto":       "no-existe",
		"bucle":             "bucle",
	} {
		must(os.Symlink(target, filepath.Join(dir, name)))
	}
	must(syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))

	f, err := OpenFolder(dir)
	must(err)
	t.Cleanup(func() { f.Close() })
	return f
}

func TestFolderOpenFile(t *testing.T) {
	f := fixture(t)
	tests := []struct {
		rel     string
		want    error
		content string
	}{
		{"a.txt", nil, "hola"},
		{"docs/b.txt", nil, "b"},
		{"./docs//b.txt", nil, "b"},
		{"enlace-dentro", nil, "hola"},
		{"enlace-dir-dentro/b.txt", nil, "b"},

		{"enlace-fuera", ErrNotAllowed, ""},
		{"enlace-dir-fuera/secreto.txt", ErrNotAllowed, ""},
		{"enlace-absoluto", ErrNotAllowed, ""},
		{"../fuera/secreto.txt", ErrNotAllowed, ""},
		{"/etc/hosts", ErrNotAllowed, ""},
		{"a.txt\x00", ErrNotAllowed, ""},

		{"no-existe.txt", ErrNotFound, ""},
		{"enlace-roto", ErrNotFound, ""},
		{"a.txt/x", ErrNotFound, ""},
		{"bucle", ErrNotFound, ""},

		{"docs", ErrIsDir, ""},
		{".", ErrIsDir, ""},
		{"fifo", ErrNotRegular, ""}, // no debe bloquearse esperando un escritor
	}
	for _, tt := range tests {
		t.Run(tt.rel, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				file, fi, err := f.OpenFile(tt.rel)
				if !errors.Is(err, tt.want) {
					t.Errorf("OpenFile(%q) error = %v; quiero %v", tt.rel, err, tt.want)
				}
				if err != nil {
					return
				}
				defer file.Close()
				buf := make([]byte, 64)
				n, _ := file.Read(buf)
				if string(buf[:n]) != tt.content || fi.Size() != int64(len(tt.content)) {
					t.Errorf("contenido %q (tamaño %d); quiero %q", buf[:n], fi.Size(), tt.content)
				}
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("OpenFile se bloqueó")
			}
		})
	}
}

func TestFolderStat(t *testing.T) {
	f := fixture(t)
	if fi, err := f.Stat("enlace-dir-dentro"); err != nil || !fi.IsDir() {
		t.Errorf("enlace a directorio interno: %v, %v", fi, err)
	}
	if _, err := f.Stat("enlace-dir-fuera"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("enlace a directorio externo: %v; quiero ErrNotAllowed", err)
	}
	if _, err := f.Stat("a/../a.txt"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("ruta con ..: %v; quiero ErrNotAllowed", err)
	}
}

func TestFolderReadDir(t *testing.T) {
	f := fixture(t)

	entries, err := f.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	slices.Sort(names)
	// Sin ocultos, sin enlaces hacia fuera, rotos o en bucle, y sin el FIFO.
	want := []string{"a.txt", "docs", "enlace-dentro", "enlace-dir-dentro"}
	if !slices.Equal(names, want) {
		t.Errorf("ReadDir = %v; quiero %v", names, want)
	}
	for _, e := range entries {
		if e.Name == "enlace-dir-dentro" && !e.Info.IsDir() {
			t.Error("enlace-dir-dentro debe resolverse como directorio")
		}
	}

	tests := []struct {
		rel  string
		want error
	}{
		{"docs", nil},
		{"enlace-dir-dentro", nil},
		{"enlace-dir-fuera", ErrNotAllowed},
		{"../fuera", ErrNotAllowed},
		{"a.txt", ErrNotDir},
		{"no-existe", ErrNotFound},
	}
	for _, tt := range tests {
		if _, err := f.ReadDir(tt.rel); !errors.Is(err, tt.want) {
			t.Errorf("ReadDir(%q) = %v; quiero %v", tt.rel, err, tt.want)
		}
	}
}

func TestClassifyStripsPath(t *testing.T) {
	err := classify(&os.PathError{Op: "open", Path: "expediente-confidencial.pdf", Err: syscall.EACCES})
	if err == nil || errors.Is(err, ErrNotAllowed) {
		t.Fatalf("EACCES: %v", err)
	}
	if got := err.Error(); got != "open: permission denied" {
		t.Errorf("mensaje %q: no debe incluir la ruta", got)
	}
}
