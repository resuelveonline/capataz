package links

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// store guarda un archivo JSON por enlace en CAPATAZ_STATE_DIR/links (0700),
// cada uno con permisos 0600 y escrito de forma atómica (temporal + rename).
// Los registros llevan rutas, el token y la clave K: este directorio no se
// copia a la bitácora ni sale del host.
type store struct {
	dir string
}

func openStore(stateDir string) (*store, error) {
	dir := filepath.Join(stateDir, "links")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, errors.New("no se pudo crear el directorio de enlaces")
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("el directorio de enlaces no debe ser accesible por otros usuarios (permisos 0700)")
	}
	return &store{dir: dir}, nil
}

func (s *store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *store) save(r *record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".link-*.tmp") // 0600
	if err != nil {
		return errors.New("no se pudo guardar el enlace")
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(data, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return errors.New("no se pudo guardar el enlace")
	}
	if err := os.Rename(tmp.Name(), s.path(r.ID)); err != nil {
		return errors.New("no se pudo guardar el enlace")
	}
	return nil
}

// loadAll lee todos los registros. Un archivo dañado no impide arrancar: se
// cuenta y se deja en su sitio para revisarlo a mano.
func (s *store) loadAll() (recs []*record, damaged int, err error) {
	des, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, 0, fmt.Errorf("leyendo el directorio de enlaces: %w", err)
	}
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".link-") {
			os.Remove(filepath.Join(s.dir, name)) // temporal de una escritura cortada
			continue
		}
		if !strings.HasSuffix(name, ".json") || !de.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			damaged++
			continue
		}
		var r record
		if json.Unmarshal(data, &r) != nil || r.ID+".json" != name {
			damaged++
			continue
		}
		recs = append(recs, &r)
	}
	return recs, damaged, nil
}
