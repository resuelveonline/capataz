// Package state guarda la identidad de la instancia en CAPATAZ_STATE_DIR.
package state

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const fileName = "instance.json"

// Instance es la identidad persistente de esta instalación de Capataz.
type Instance struct {
	InstanceID string    `json:"instanceId"`
	CreatedAt  time.Time `json:"createdAt"`
}

// LoadOrCreate lee dir/instance.json o, si no existe, lo crea con un
// instanceId aleatorio nuevo. El directorio se crea con permisos 0700 y el
// archivo con 0600.
func LoadOrCreate(dir string) (Instance, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Instance{}, fmt.Errorf("creando el directorio de estado: %w", err)
	}
	path := filepath.Join(dir, fileName)

	inst, err := load(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return inst, err
	}

	inst, err = newInstance()
	if err != nil {
		return Instance{}, err
	}
	if err := writeOnce(path, inst); err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Otro proceso lo creó primero: su identidad es la buena.
			return load(path)
		}
		return Instance{}, err
	}
	return inst, nil
}

func load(path string) (Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Instance{}, err
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return Instance{}, fmt.Errorf("%s está dañado: %w", fileName, err)
	}
	if inst.InstanceID == "" {
		return Instance{}, fmt.Errorf("%s no tiene instanceId", fileName)
	}
	return inst, nil
}

func newInstance() (Instance, error) {
	b := make([]byte, 16) // 128 bits
	if _, err := rand.Read(b); err != nil {
		return Instance{}, err
	}
	id := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return Instance{InstanceID: id, CreatedAt: time.Now().UTC().Truncate(time.Second)}, nil
}

// writeOnce escribe en un temporal y lo publica con os.Link, que falla si el
// destino ya existe. Así el archivo nunca queda a medias y dos arranques
// simultáneos no se pisan la identidad (os.Rename sí sobrescribiría).
func writeOnce(path string, inst Instance) error {
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".instance-*.tmp") // se crea con 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}
