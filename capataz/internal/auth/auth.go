// Package auth identifica quién hace cada petición.
//
// Authenticator es la costura para cambiar de mecanismo: en la Fase 1 lo
// implementa DevToken; después lo implementará el emparejamiento de
// dispositivos sin tocar los handlers.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Role es el rol de quien llama, verificado en el servidor.
type Role string

const (
	RoleTitular Role = "titular"
	RoleDevice  Role = "device"
)

// Principal es la identidad autenticada de una petición.
type Principal struct {
	DeviceID string
	Role     Role
}

// ErrUnauthorized: credenciales ausentes o inválidas. No distingue entre
// ambos casos a propósito.
var ErrUnauthorized = errors.New("no autorizado")

// Authenticator valida las credenciales de una petición.
type Authenticator interface {
	Authenticate(r *http.Request) (Principal, error)
}

// DevToken acepta un único token fijo de desarrollo y lo trata como la
// cuenta titular. Solo para la Fase 1.
type DevToken struct {
	sum [sha256.Size]byte
}

// NewDevToken guarda solo el hash del token.
func NewDevToken(token string) *DevToken {
	return &DevToken{sum: sha256.Sum256([]byte(token))}
}

// Authenticate compara en tiempo constante. Se comparan los SHA-256 y no
// los tokens: ConstantTimeCompare sale antes si las longitudes difieren, y
// los hashes siempre miden 32 bytes, así tampoco se filtra la longitud.
func (d *DevToken) Authenticate(r *http.Request) (Principal, error) {
	tok, ok := bearer(r)
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	sum := sha256.Sum256([]byte(tok))
	if subtle.ConstantTimeCompare(sum[:], d.sum[:]) != 1 {
		return Principal{}, ErrUnauthorized
	}
	return Principal{DeviceID: "dev", Role: RoleTitular}, nil
}

// bearer extrae el token de "Authorization: Bearer <token>". El esquema no
// distingue mayúsculas (RFC 9110).
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	scheme, tok, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	tok = strings.TrimSpace(tok)
	return tok, tok != ""
}

type ctxKey struct{}

// WithPrincipal devuelve un contexto que lleva el Principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext recupera el Principal guardado por el middleware.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
