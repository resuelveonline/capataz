package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	secretKey   = "clave-secreta-de-syncthing"
	secretToken = "token-de-desarrollo-muy-largo-0123456789"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func validEnv() map[string]string {
	return map[string]string{
		"SYNCTHING_URL":         "http://127.0.0.1:8384/",
		"SYNCTHING_API_KEY":     secretKey,
		"CAPATAZ_DEV_TOKEN":     secretToken,
		"CAPATAZ_STATE_DIR":     "~/capataz-host/state",
		"CAPATAZ_LISTEN":        "",
		"CAPATAZ_INSTANCE_NAME": "",
	}
}

func TestFromEnvValid(t *testing.T) {
	c, err := FromEnv(envOf(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if c.SyncthingURL != "http://127.0.0.1:8384" {
		t.Errorf("SyncthingURL = %q", c.SyncthingURL)
	}
	if c.Listen != defaultListen || c.InstanceName != defaultInstanceName {
		t.Errorf("valores por defecto: %q %q", c.Listen, c.InstanceName)
	}
	if want := filepath.Join(home, "capataz-host/state"); c.StateDir != want {
		t.Errorf("StateDir = %q, quiero %q", c.StateDir, want)
	}
	if want := filepath.Join(home, "capataz-host/staging"); c.StagingDir != want {
		t.Errorf("StagingDir por defecto = %q, quiero %q", c.StagingDir, want)
	}
}

func TestStagingDir(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{
		"":                  filepath.Join(home, "capataz-host/staging"), // vacía: por defecto
		"   ":               filepath.Join(home, "capataz-host/staging"),
		"~/otra/staging":    filepath.Join(home, "otra/staging"),
		"/var/tmp/staging/": "/var/tmp/staging",
	} {
		env := validEnv()
		env["CAPATAZ_STAGING_DIR"] = in
		c, err := FromEnv(envOf(env))
		if err != nil || c.StagingDir != want {
			t.Errorf("CAPATAZ_STAGING_DIR=%q: %q, %v; quiero %q", in, c.StagingDir, err, want)
		}
	}
}

func TestFromEnvErrors(t *testing.T) {
	tests := []struct {
		name   string
		modify func(map[string]string)
		want   []string
	}{
		{"falta todo", func(m map[string]string) { clear(m) },
			[]string{"falta SYNCTHING_URL", "falta SYNCTHING_API_KEY", "falta CAPATAZ_DEV_TOKEN", "falta CAPATAZ_STATE_DIR"}},
		{"api key vacía", func(m map[string]string) { m["SYNCTHING_API_KEY"] = "   " },
			[]string{"falta SYNCTHING_API_KEY"}},
		{"token corto", func(m map[string]string) { m["CAPATAZ_DEV_TOKEN"] = "corto-pero-secreto" },
			[]string{"CAPATAZ_DEV_TOKEN debe tener al menos 32"}},
		{"url sin esquema", func(m map[string]string) { m["SYNCTHING_URL"] = "127.0.0.1:8384" },
			[]string{"SYNCTHING_URL debe ser"}},
		{"url con credenciales", func(m map[string]string) { m["SYNCTHING_URL"] = "http://admin:" + secretKey + "@127.0.0.1:8384" },
			[]string{"SYNCTHING_URL debe ser"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := validEnv()
			tt.modify(env)
			_, err := FromEnv(envOf(env))
			if err == nil {
				t.Fatal("se esperaba error")
			}
			msg := err.Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("mensaje %q no contiene %q", msg, w)
				}
			}
			// El mensaje nunca debe contener valores secretos.
			for _, s := range []string{secretKey, secretToken, "corto-pero-secreto"} {
				if strings.Contains(msg, s) {
					t.Errorf("el mensaje filtra un secreto: %q", msg)
				}
			}
		})
	}
}

func TestLogValueRedactsSecrets(t *testing.T) {
	c, err := FromEnv(envOf(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("config", "cfg", c)
	if out := buf.String(); strings.Contains(out, secretKey) || strings.Contains(out, secretToken) {
		t.Errorf("el log filtra secretos: %s", out)
	}
}

func TestParseDotEnv(t *testing.T) {
	in := `
# comentario
SIMPLE=valor
  ESPACIOS  =   con espacios
COMENTARIO=abc # esto se ignora
ALMOHADILLA=abc#def
DOBLES="hola # no es comentario"   # este sí
ESCAPES="a\"b\\c\nd"
SIMPLES='literal \n # sin escapes'
VACIO=
VACIO_COMILLAS=""
export EXPORTADO=1
URL=http://127.0.0.1:8384
`
	got, err := parseDotEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SIMPLE":         "valor",
		"ESPACIOS":       "con espacios",
		"COMENTARIO":     "abc",
		"ALMOHADILLA":    "abc#def",
		"DOBLES":         "hola # no es comentario",
		"ESCAPES":        "a\"b\\c\nd",
		"SIMPLES":        `literal \n # sin escapes`,
		"VACIO":          "",
		"VACIO_COMILLAS": "",
		"EXPORTADO":      "1",
		"URL":            "http://127.0.0.1:8384",
	}
	if len(got) != len(want) {
		t.Errorf("%d variables, quiero %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, quiero %q", k, got[k], v)
		}
	}
}

func TestParseDotEnvErrors(t *testing.T) {
	for _, in := range []string{
		"SIN_IGUAL",
		"1CLAVE=x",
		`ABIERTA="` + secretKey,
		`ABIERTA='` + secretKey,
		`BASURA="x"` + secretKey,
	} {
		_, err := parseDotEnv(strings.NewReader(in))
		if err == nil {
			t.Errorf("%q: se esperaba error", in)
			continue
		}
		if strings.Contains(err.Error(), secretKey) {
			t.Errorf("el error filtra el valor: %v", err)
		}
	}
}

func TestLoadDotEnvRealEnvWins(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("CAPATAZ_T_A=archivo\nCAPATAZ_T_B=archivo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAPATAZ_T_A", "entorno")
	if err := LoadDotEnv(p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("CAPATAZ_T_B") })
	if v := os.Getenv("CAPATAZ_T_A"); v != "entorno" {
		t.Errorf("A = %q, el entorno real debe ganar", v)
	}
	if v := os.Getenv("CAPATAZ_T_B"); v != "archivo" {
		t.Errorf("B = %q", v)
	}
}
