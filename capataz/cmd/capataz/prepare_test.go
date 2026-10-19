package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/resuelveonline/capataz/capataz/internal/cpk"
	"github.com/resuelveonline/capataz/capataz/internal/prepare"
	"github.com/resuelveonline/capataz/capataz/internal/stage/stagetest"
)

const (
	apiKey      = "clave-api-syncthing-de-prueba"
	devToken    = "token-de-desarrollo-muy-largo-0123456789"
	secretName  = "expediente-confidencial-garcia.pdf"
	secretDir   = "carpeta-cliente-garcia"
	secretText  = "CONTENIDO-SECRETO-DEL-EXPEDIENTE"
	secretLabel = "Etiqueta-privada-del-titular"
)

type cliEnv struct {
	t       *testing.T
	base    string
	dir     string
	staging string
	env     map[string]string
	logs    *bytes.Buffer
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	base := t.TempDir()
	e := &cliEnv{t: t, base: base, dir: filepath.Join(base, "pruebas"), staging: filepath.Join(base, "staging"), logs: &bytes.Buffer{}}
	for rel, c := range map[string]string{
		secretName:                            secretText,
		secretDir + "/" + secretName:          secretText + " (2)",
		secretDir + "/sub/notas-privadas.txt": secretText + " notas",
	} {
		p := filepath.Join(e.dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(c), 0o644)
	}
	os.Symlink(secretName, filepath.Join(e.dir, "enlace-"+secretName))
	srv := stagetest.Server(t, stagetest.New(t, e.dir, "fgyww-edjwa", "pruebas"), apiKey)
	e.env = map[string]string{
		"SYNCTHING_URL":       srv.URL,
		"SYNCTHING_API_KEY":   apiKey,
		"CAPATAZ_DEV_TOKEN":   devToken,
		"CAPATAZ_STATE_DIR":   filepath.Join(base, "estado"),
		"CAPATAZ_STAGING_DIR": e.staging,
	}
	return e
}

func (e *cliEnv) run(out string, extra ...string) (string, error) {
	args := append([]string{"--folder", "fgyww-edjwa", "--out", out}, extra...)
	var stdout bytes.Buffer
	log := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	err := runPrepare(context.Background(), log, args, func(k string) string { return e.env[k] }, &stdout)
	return stdout.String(), err
}

// readTree concatena el contenido de todos los archivos bajo dir.
func readTree(t *testing.T, dir string) string {
	var b strings.Builder
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			c, _ := os.ReadFile(p)
			b.Write(c)
			b.WriteString(p)
		}
		return nil
	})
	return b.String()
}

// Prueba de privacidad de extremo a extremo de la Fase 2a: la clave K, los
// nombres, las rutas, la etiqueta y el contenido no aparecen en la
// bitácora, en los mensajes de error ni en los archivos de salida (salvo
// cifrados), y la clave solo sale por stdout con --print-key.
func TestPrepareNeverLeaks(t *testing.T) {
	e := newCLIEnv(t)

	// 1. Éxito con --print-key: stdout es exactamente la clave.
	out1 := filepath.Join(e.base, "salida-1")
	stdout, err := e.run(out1, "--path", secretName, "--path", secretDir, "--label", secretLabel, "--print-key")
	if err != nil {
		t.Fatal(err)
	}
	keyText := strings.TrimSuffix(stdout, "\n")
	key, err := cpk.ParseKey(keyText)
	if err != nil || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("stdout debe ser solo la clave: %q", stdout)
	}
	// La clave descifra los metadatos: es la correcta.
	b64, _ := os.ReadFile(filepath.Join(out1, prepare.MetaFile))
	blob, _ := base64.RawURLEncoding.DecodeString(string(b64))
	meta, err := cpk.DecryptMeta(key, blob)
	if err != nil || !strings.Contains(string(meta), secretLabel) {
		t.Fatalf("los metadatos no descifran con la clave impresa: %v", err)
	}

	// 2. Éxito sin --print-key: stdout vacío.
	if stdout, err := e.run(filepath.Join(e.base, "salida-2"), "--path", secretName); err != nil || stdout != "" {
		t.Fatalf("sin --print-key stdout debe estar vacío: %q, %v", stdout, err)
	}

	// 3. Errores con nombres sensibles en la petición.
	var errMsgs []string
	for _, p := range []string{
		"no-existe-" + secretName,
		"../" + secretName,
		"enlace-" + secretName,
		secretDir + "/../" + secretName,
		secretName + "/x",
	} {
		_, err := e.run(filepath.Join(e.base, "salida-error"), "--path", p)
		if err == nil {
			t.Fatalf("%q: se esperaba error", p)
		}
		errMsgs = append(errMsgs, err.Error())
	}
	_, err = e.run(filepath.Join(e.dir, "salida-dentro"), "--path", secretName)
	if err == nil {
		t.Fatal("una salida dentro de la carpeta sincronizada debe rechazarse")
	}
	errMsgs = append(errMsgs, err.Error())

	sensitive := []string{
		keyText, secretName, "expediente", "garcia", secretDir, "notas-privadas",
		secretText, "CONTENIDO-SECRETO", secretLabel, e.dir, apiKey, devToken,
	}
	check := func(where, text string, allow ...string) {
		t.Helper()
		for _, s := range sensitive {
			if strings.Contains(text, s) && !contains(allow, s) {
				t.Errorf("%s contiene %q", where, s)
			}
		}
	}
	check("la bitácora", e.logs.String())
	check("los mensajes de error", strings.Join(errMsgs, "\n"))
	// En la salida, el paquete y el metaBlob van cifrados; el manifiesto
	// no lleva nombres. Las rutas de los archivos de salida contienen la
	// base temporal, que no es sensible.
	check("los archivos de salida", readTree(t, out1), e.dir)
	check("el staging", readTree(t, e.staging))
	if left, _ := os.ReadDir(e.staging); len(left) != 0 {
		t.Errorf("el staging no quedó vacío: %d entradas", len(left))
	}
	if !strings.Contains(e.logs.String(), "paquete preparado") {
		t.Error("la bitácora debe registrar la preparación (sin datos sensibles)")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestPrepareUsage(t *testing.T) {
	e := newCLIEnv(t)
	for _, args := range [][]string{nil, {"--folder", "x"}, {"--folder", "x", "--path", "a"}} {
		err := runPrepare(context.Background(), slog.New(slog.NewTextHandler(e.logs, nil)), args, func(k string) string { return e.env[k] }, &bytes.Buffer{})
		if !errors.Is(err, flag.ErrHelp) {
			t.Errorf("%v: %v; quiero ErrHelp", args, err)
		}
	}
}
