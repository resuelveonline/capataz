package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

const token = "token-de-desarrollo-muy-largo-0123456789"

func TestDevTokenAuthenticate(t *testing.T) {
	a := NewDevToken(token)
	tests := []struct {
		name   string
		header string // "" = sin header Authorization
		ok     bool
	}{
		{"correcto", "Bearer " + token, true},
		{"esquema en minúsculas", "bearer " + token, true},
		{"espacios sobrantes", "Bearer   " + token + "  ", true},
		{"sin header", "", false},
		{"solo esquema", "Bearer", false},
		{"esquema sin token", "Bearer ", false},
		{"esquema incorrecto", "Basic " + token, false},
		{"sin esquema", token, false},
		{"token incorrecto", "Bearer " + token[:len(token)-1] + "X", false},
		{"prefijo del token", "Bearer " + token[:10], false},
		{"token más largo", "Bearer " + token + "0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/folders", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			p, err := a.Authenticate(r)
			if tt.ok {
				if err != nil {
					t.Fatalf("error inesperado: %v", err)
				}
				if p.Role != RoleTitular || p.DeviceID != "dev" {
					t.Errorf("principal = %+v", p)
				}
				return
			}
			if !errors.Is(err, ErrUnauthorized) {
				t.Errorf("error = %v; quiero ErrUnauthorized", err)
			}
			if p != (Principal{}) {
				t.Errorf("un fallo no debe devolver identidad: %+v", p)
			}
		})
	}
}

func TestDevTokenStoresOnlyHash(t *testing.T) {
	a := NewDevToken(token)
	if string(a.sum[:]) == token {
		t.Fatal("el token se guarda en claro")
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Error("un contexto vacío no debe tener Principal")
	}
	want := Principal{DeviceID: "dev", Role: RoleTitular}
	got, ok := FromContext(WithPrincipal(context.Background(), want))
	if !ok || got != want {
		t.Errorf("FromContext = %+v, %v", got, ok)
	}
}

// La interfaz es la costura para el emparejamiento real.
var _ Authenticator = (*DevToken)(nil)
