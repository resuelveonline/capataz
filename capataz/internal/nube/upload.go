package nube

import (
	"context"
	"errors"
	"io"
	"time"
)

// PartSize es el tamaño de parte que usa Capataz (la nube acepta de 1 a
// 8 MiB). Es independiente del tamaño de bloque CPK1.
const PartSize = 8 << 20

// Retry configura los reintentos de errores transitorios.
type Retry struct {
	Attempts int           // intentos por operación (por defecto 5)
	Base     time.Duration // primera espera; se duplica (por defecto 1 s)
	Max      time.Duration // espera máxima (por defecto 30 s)
}

func (r Retry) withDefaults() Retry {
	if r.Attempts <= 0 {
		r.Attempts = 5
	}
	if r.Base <= 0 {
		r.Base = time.Second
	}
	if r.Max <= 0 {
		r.Max = 30 * time.Second
	}
	return r
}

// Transient indica si vale la pena reintentar: la nube no respondió, falló
// con 5xx o pidió esperar (429).
func Transient(err error) bool {
	var e *Error
	return errors.Is(err, ErrUnreachable) || errors.As(err, &e) && e.Status == 429
}

// Do ejecuta op y la reintenta mientras el error sea transitorio.
func (r Retry) Do(ctx context.Context, op func() error) error {
	r = r.withDefaults()
	wait := r.Base
	var err error
	for attempt := 1; ; attempt++ {
		if err = op(); err == nil || !Transient(err) || attempt >= r.Attempts {
			return err
		}
		d := wait
		var e *Error
		if errors.As(err, &e) && e.RetryAfter > d {
			d = min(e.RetryAfter, r.Max)
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		wait = min(wait*2, r.Max)
	}
}

// Upload sube las partes del paquete que la nube aún no tiene (received
// son las que ya tiene, de un OpenUpload anterior) y llama a progress con
// los bytes ya presentes en la nube tras cada parte. Usa un solo búfer de
// partSize bytes: la memoria no crece con el paquete.
func (c *Client) Upload(ctx context.Context, linkID string, pkg io.ReaderAt, size int64, partSize int,
	received []int, retry Retry, progress func(done int64)) error {
	if partSize <= 0 || size <= 0 {
		return errors.New("nube: tamaño de parte o de paquete inválido")
	}
	count := int((size + int64(partSize) - 1) / int64(partSize))
	have := make(map[int]bool, len(received))
	var done int64
	for _, n := range received {
		if n >= 1 && n <= count && !have[n] {
			have[n] = true
			done += partLen(n, count, size, partSize)
		}
	}
	if progress != nil {
		progress(done)
	}
	buf := make([]byte, partSize)
	for n := 1; n <= count; n++ {
		if have[n] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		part := buf[:partLen(n, count, size, partSize)]
		// ReadAt puede devolver io.EOF junto con la parte completa.
		if got, err := pkg.ReadAt(part, int64(n-1)*int64(partSize)); got != len(part) || err != nil && !errors.Is(err, io.EOF) {
			return errors.New("nube: no se pudo leer el paquete")
		}
		if err := retry.Do(ctx, func() error {
			_, err := c.UploadPart(ctx, linkID, n, part)
			return err
		}); err != nil {
			return err
		}
		done += int64(len(part))
		if progress != nil {
			progress(done)
		}
	}
	return nil
}

func partLen(n, count int, size int64, partSize int) int64 {
	if n < count {
		return int64(partSize)
	}
	return size - int64(count-1)*int64(partSize)
}
