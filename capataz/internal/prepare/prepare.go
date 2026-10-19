// Package prepare orquesta la Fase 2a de un enlace, todo en local:
// copia congelada (stage) → carga útil (bundle) → paquete CPK1 (cpk).
// Escribe en el directorio de salida el paquete, el metaBlob y un
// manifiesto sin nombres ni clave.
package prepare

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/bundle"
	"github.com/resuelveonline/capataz/capataz/internal/cpk"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
)

// Archivos que escribe Run en el directorio de salida.
const (
	PackageFile  = "package.cpk1"
	MetaFile     = "meta.b64" // metaBlob en base64url, sin salto final
	ManifestFile = "manifest.json"
)

// Options configura Run.
type Options struct {
	Stager *stage.Stager
	Items  []stage.Item
	OutDir string
	Label  string
	Now    time.Time // por defecto time.Now()
	Logger *slog.Logger
}

// Manifest describe el paquete sin revelar nombres, rutas ni la clave.
type Manifest struct {
	LinkID        string        `json:"linkId"`
	Kind          string        `json:"kind"`
	CipherVersion string        `json:"cipherVersion"`
	ChunkSizeLog2 int           `json:"chunkSizeLog2"`
	PayloadSize   int64         `json:"payloadSize"`
	PayloadSha256 string        `json:"payloadSha256"`
	PackageSize   int64         `json:"packageSize"`
	PackageSha256 string        `json:"packageSha256"`
	MetaBlobSize  int           `json:"metaBlobSize"`
	CreatedAt     string        `json:"createdAt"`
	Skipped       stage.Skipped `json:"skipped"`
}

// Result es lo que devuelve Run. Key solo vive en memoria: Run no la
// escribe en ningún archivo ni en la bitácora.
type Result struct {
	Manifest Manifest
	Key      cpk.Key
}

// Run prepara el paquete. Si falla, no deja nada en el staging ni archivos
// a medias en OutDir.
func Run(ctx context.Context, o Options) (_ *Result, err error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	start := time.Now()

	if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
		return nil, errors.New("no se pudo crear el directorio de salida")
	}
	if err := o.Stager.CheckOutside(ctx, o.OutDir, "la salida"); err != nil {
		return nil, err
	}
	outputs := []string{
		filepath.Join(o.OutDir, PackageFile),
		filepath.Join(o.OutDir, MetaFile),
		filepath.Join(o.OutDir, ManifestFile),
	}
	for _, p := range outputs {
		if _, err := os.Lstat(p); err == nil {
			return nil, errors.New("el directorio de salida ya contiene un paquete")
		}
	}
	defer func() {
		if err != nil {
			for _, p := range outputs {
				os.Remove(p)
			}
		}
	}()

	sn, err := o.Stager.Begin(ctx, o.Items)
	if err != nil {
		return nil, err
	}
	defer sn.Close()

	payload, err := bundle.Build(ctx, sn, bundle.Options{Label: o.Label, Now: o.Now})
	if err != nil {
		return nil, err
	}
	key, err := cpk.NewKey()
	if err != nil {
		return nil, err
	}

	pkgSize, pkgSum, err := encryptPayload(outputs[0], payload, key)
	if err != nil {
		return nil, err
	}
	// La carga útil en claro ya no hace falta: liberar disco antes de seguir.
	os.Remove(payload.Path)

	metaJSON, err := payload.MetaJSON()
	if err != nil {
		return nil, err
	}
	blob, err := cpk.EncryptMeta(key, metaJSON)
	if err != nil {
		return nil, err
	}
	if err := writeNew(outputs[1], []byte(base64.RawURLEncoding.EncodeToString(blob))); err != nil {
		return nil, err
	}

	m := Manifest{
		LinkID:        newLinkID(),
		Kind:          payload.Kind,
		CipherVersion: "cpk1",
		ChunkSizeLog2: cpk.DefaultChunkSizeLog2,
		PayloadSize:   payload.Size,
		PayloadSha256: hex.EncodeToString(payload.SHA256[:]),
		PackageSize:   pkgSize,
		PackageSha256: hex.EncodeToString(pkgSum[:]),
		MetaBlobSize:  len(blob),
		CreatedAt:     sn.CreatedAt.Format(time.RFC3339),
		Skipped:       sn.Skipped,
	}
	mj, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeNew(outputs[2], append(mj, '\n')); err != nil {
		return nil, err
	}

	o.Logger.Info("paquete preparado", "linkId", m.LinkID, "kind", m.Kind,
		"payloadBytes", m.PayloadSize, "packageBytes", m.PackageSize,
		"duration", time.Since(start).Round(time.Millisecond))
	return &Result{Manifest: m, Key: key}, nil
}

func encryptPayload(dst string, p *bundle.Payload, key cpk.Key) (int64, [32]byte, error) {
	var sum [32]byte
	src, err := os.Open(p.Path)
	if err != nil {
		return 0, sum, err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, sum, errors.New("no se pudo crear el paquete")
	}
	h := sha256.New()
	n, err := cpk.Encrypt(io.MultiWriter(out, h), src, p.Size, key)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, sum, err
	}
	if want := cpk.PackageSize(p.Size, cpk.DefaultChunkSizeLog2); n != want {
		return 0, sum, errors.New("el paquete no mide lo esperado")
	}
	copy(sum[:], h.Sum(nil))
	return n, sum, nil
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("no se pudo escribir la salida")
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// newLinkID: 16 bytes aleatorios en base64url (22 caracteres), compatible
// con el patrón de linkId del contrato de Nube.
func newLinkID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
