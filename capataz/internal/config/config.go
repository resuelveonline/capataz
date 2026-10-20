// Package config carga la configuración de Capataz desde variables de entorno.
//
// Los mensajes de error nombran la variable con problema, nunca su valor:
// varias de ellas son secretos.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// MinDevTokenLen es la longitud mínima del token de desarrollo.
const MinDevTokenLen = 32

const (
	defaultListen       = "127.0.0.1:8080"
	defaultInstanceName = "Capataz"
	defaultStagingDir   = "~/capataz-host/staging"
)

// Config es la configuración ya validada.
type Config struct {
	SyncthingURL    string // URL base de la REST API de Syncthing, sin "/" final
	SyncthingAPIKey string // secreto
	Listen          string // dirección host:puerto del servidor HTTP
	DevToken        string // secreto: token temporal de desarrollo
	StateDir        string // directorio del estado de la instancia (ruta absoluta)
	StagingDir      string // copias congeladas temporales (ruta absoluta)
	InstanceName    string // nombre público que devuelve GET /instance
}

// LogValue hace que slog nunca imprima los secretos aunque alguien registre
// la Config completa por error.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("syncthingURL", c.SyncthingURL),
		slog.String("listen", c.Listen),
		slog.String("stateDir", c.StateDir),
		slog.String("stagingDir", c.StagingDir),
		slog.String("instanceName", c.InstanceName),
	)
}

// FromEnv lee y valida la configuración. getenv suele ser os.Getenv; las
// pruebas pasan su propia función. Devuelve todos los problemas juntos para
// no obligar a arrancar varias veces.
func FromEnv(getenv func(string) string) (Config, error) {
	var problems []string
	required := func(name string) string {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			problems = append(problems, "falta "+name)
		}
		return v
	}

	c := Config{
		SyncthingURL:    strings.TrimRight(required("SYNCTHING_URL"), "/"),
		SyncthingAPIKey: required("SYNCTHING_API_KEY"),
		DevToken:        required("CAPATAZ_DEV_TOKEN"),
		StateDir:        required("CAPATAZ_STATE_DIR"),
		Listen:          strings.TrimSpace(getenv("CAPATAZ_LISTEN")),
		InstanceName:    strings.TrimSpace(getenv("CAPATAZ_INSTANCE_NAME")),
		StagingDir:      strings.TrimSpace(getenv("CAPATAZ_STAGING_DIR")),
	}
	if c.StagingDir == "" {
		c.StagingDir = defaultStagingDir
	}
	if c.Listen == "" {
		c.Listen = defaultListen
	}
	if c.InstanceName == "" {
		c.InstanceName = defaultInstanceName
	}

	if c.SyncthingURL != "" {
		u, err := url.Parse(c.SyncthingURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			problems = append(problems, "SYNCTHING_URL debe ser una URL http(s) sin credenciales")
		}
	}
	if c.DevToken != "" && len(c.DevToken) < MinDevTokenLen {
		problems = append(problems, fmt.Sprintf("CAPATAZ_DEV_TOKEN debe tener al menos %d caracteres", MinDevTokenLen))
	}
	if c.StateDir != "" {
		dir, err := absDir(c.StateDir)
		if err != nil {
			problems = append(problems, "CAPATAZ_STATE_DIR no es una ruta válida")
		}
		c.StateDir = dir
	}
	if dir, err := absDir(c.StagingDir); err != nil {
		problems = append(problems, "CAPATAZ_STAGING_DIR no es una ruta válida")
	} else {
		c.StagingDir = dir
	}

	if len(problems) > 0 {
		return Config{}, errors.New("configuración inválida: " + strings.Join(problems, "; "))
	}
	return c, nil
}

func absDir(p string) (string, error) {
	dir, err := ExpandHome(p)
	if err != nil {
		return "", err
	}
	return filepath.Abs(dir)
}

// ExpandHome sustituye un "~" inicial por el directorio del usuario.
// Syncthing también guarda rutas de carpeta con "~".
func ExpandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, p[1:]), nil
}
