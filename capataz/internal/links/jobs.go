package links

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/resuelveonline/capataz/capataz/internal/nube"
	"github.com/resuelveonline/capataz/capataz/internal/prepare"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
)

// upload abre (o reabre) la subida en la nube, sube las partes que falten
// y la completa. El paquete se borra del staging en cuanto queda activo.
func (m *Manager) upload(ctx context.Context, id string) error {
	m.mu.Lock()
	r := m.recs[id]
	if r.Open == nil || r.PackageSha256 == nil {
		m.mu.Unlock()
		return newError(CodeInternal, "registro de subida incompleto")
	}
	open, sha := *r.Open, *r.PackageSha256
	m.mu.Unlock()

	var cl nube.Link
	err := m.o.Retry.Do(ctx, func() (err error) {
		cl, err = m.o.Nube.OpenUpload(ctx, open)
		return err
	})
	if err != nil {
		return err
	}
	if cl.State == nube.StateUploading {
		f, err := os.Open(filepath.Join(m.linkDir(id), prepare.PackageFile))
		if err != nil {
			return newError(CodeInternal, "el paquete ya no está en el staging")
		}
		defer f.Close()
		size := float64(open.EncryptedSize)
		err = m.o.Nube.Upload(ctx, id, f, open.EncryptedSize, open.PartSize, cl.ReceivedParts, m.o.Retry,
			func(done int64) {
				m.mu.Lock()
				r.Progress = min(float64(done)/size, 0.99) // 1 solo cuando la nube lo activa
				m.mu.Unlock()
			})
		if err != nil {
			return err
		}
		err = m.o.Retry.Do(ctx, func() (err error) {
			cl, err = m.o.Nube.Complete(ctx, id, sha)
			return err
		})
		if err != nil {
			return err
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if r.terminal() {
		return nil
	}
	switch cl.State {
	case nube.StateActive:
		r.Status, r.Progress = StatusActive, 1
		r.Open = nil // ya no hace falta reabrir
		m.persist(r)
		os.RemoveAll(m.linkDir(id))
		m.log.Info("enlace activo", "linkId", id, "packageBytes", open.EncryptedSize)
		return nil
	case nube.StateExpired, nube.StateExhausted:
		m.finish(r, StatusExpired, "")
		return nil
	case nube.StateRevoked:
		m.finish(r, StatusRevoked, "")
		return nil
	}
	return newError(CodeCloudUnreach, "estado inesperado en la nube: "+cl.State)
}

// finish lleva el enlace a un estado terminal (con m.mu): cancela su
// tarea, borra el token, la clave y el paquete local, y lo guarda.
func (m *Manager) finish(r *record, status, code string) {
	if cancel, ok := m.jobs[r.ID]; ok {
		cancel()
	}
	opened := r.Open != nil
	r.Status = status
	r.Token, r.Key, r.Open = "", "", nil
	r.FailureCode = nil
	if status == StatusFailed {
		r.FailureCode = &code
		if opened && m.o.Nube != nil {
			// La nube pudo quedarse con partes: liberarlas, sin esperar.
			id := r.ID
			m.wg.Go(func() {
				if err := m.o.Nube.Revoke(m.ctx, id); err != nil && !nube.IsCode(err, "not_found") {
					m.log.Warn("no se pudo liberar la subida fallida en la nube", "linkId", id, "error", err)
				}
			})
		}
	}
	os.RemoveAll(m.linkDir(r.ID))
	m.persist(r)
	attrs := []any{"linkId", r.ID, "status", status}
	if code != "" {
		attrs = append(attrs, "code", code)
	}
	m.log.Info("enlace terminado", attrs...)
}

func (m *Manager) persist(r *record) {
	if err := m.store.save(r); err != nil {
		m.log.Error("no se pudo guardar el enlace", "linkId", r.ID, "error", err)
	}
}

// view arma el Link del contrato (con m.mu). La url solo existe mientras
// el enlace está activo.
func (m *Manager) view(r *record) Link {
	l := r.Link
	l.Items = slices.Clone(r.Items)
	if r.Status == StatusActive && r.Token != "" && r.Key != "" && m.o.Nube != nil {
		u := m.o.Nube.BaseURL() + "/s/" + r.Token + "#" + r.Key
		l.URL = &u
	}
	return l
}

func (m *Manager) linkDir(id string) string {
	return filepath.Join(m.o.StagingDir, linkDirPrefix+id)
}

// removeOrphanDirs borra los paquetes del staging que ningún enlace en
// subida necesita.
func (m *Manager) removeOrphanDirs() {
	des, err := os.ReadDir(m.o.StagingDir)
	if err != nil {
		return
	}
	n := 0
	for _, de := range des {
		id, ok := strings.CutPrefix(de.Name(), linkDirPrefix)
		if !ok || !de.IsDir() {
			continue
		}
		if r, ok := m.recs[id]; ok && r.Status == StatusUploading {
			continue
		}
		if os.RemoveAll(filepath.Join(m.o.StagingDir, de.Name())) == nil {
			n++
		}
	}
	if n > 0 {
		m.log.Info("paquetes huérfanos eliminados", "cantidad", n)
	}
}

// newToken: 32 bytes aleatorios en base64url (43 caracteres). La nube solo
// recibe base64url(SHA-256) de estos 43 caracteres.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// failureCode traduce el error de una tarea al código de Link.failureCode.
func failureCode(err error) string {
	var le *Error
	var se *stage.Error
	var ne *nube.Error
	switch {
	case errors.As(err, &le):
		return le.Code
	case errors.As(err, &se):
		return se.Code
	case errors.Is(err, nube.ErrUnreachable):
		return CodeCloudUnreach
	case errors.As(err, &ne):
		switch ne.Code {
		case "payload_too_large":
			return CodePayloadTooLarge
		case "quota_exceeded":
			return CodeCloudQuota
		}
		return CodeCloudUnreach
	}
	return CodeInternal
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
