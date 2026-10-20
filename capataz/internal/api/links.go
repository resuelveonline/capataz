package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/resuelveonline/capataz/capataz/internal/auth"
	"github.com/resuelveonline/capataz/capataz/internal/links"
)

// Links es lo que la API necesita del gestor de enlaces.
type Links interface {
	Create(ctx context.Context, owner string, req links.Request) (links.Link, error)
	Get(id string) (links.Link, bool)
	List(status string) []links.Link
	Revoke(ctx context.Context, id string) error
}

// maxLinkRequest es el tope del cuerpo de POST /links.
const maxLinkRequest = 64 << 10

// linkRequest es el esquema LinkRequest del contrato.
type linkRequest struct {
	Items        []links.Item `json:"items"`
	TTLSeconds   *int64       `json:"ttlSeconds"`
	MaxDownloads *int         `json:"maxDownloads"`
	Label        string       `json:"label"`
	Encryption   string       `json:"encryption"`
}

// titular exige la cuenta titular (x-roles [titular] del contrato).
func (s *Server) titular(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.FromContext(r.Context()); !ok || p.Role != auth.RoleTitular {
			writeError(w, http.StatusForbidden, codeForbidden, "solo la cuenta titular")
			return
		}
		if s.links == nil {
			writeError(w, http.StatusBadGateway, codeCloudUnreachable, "los enlaces no están disponibles")
			return
		}
		h(w, r)
	}
}

// handleCreateLink atiende POST /links: valida y responde 202 con el
// enlace en preparing; el resto ocurre en segundo plano.
func (s *Server) handleCreateLink(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLinkRequest))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, codePayloadTooLarge, "petición demasiado grande")
			return
		}
		s.fail(w, r, badRequest("no se pudo leer la petición"))
		return
	}
	var lr linkRequest
	if err := decodeStrict(body, &lr); err != nil {
		s.fail(w, r, badRequest("JSON inválido"))
		return
	}
	req := links.Request{Items: lr.Items, TTLSeconds: lr.TTLSeconds, MaxDownloads: lr.MaxDownloads,
		Label: lr.Label, Encryption: lr.Encryption}
	p, _ := auth.FromContext(r.Context())
	l, err := s.links.Create(r.Context(), p.DeviceID, req)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/links/"+l.ID)
	writeJSON(w, http.StatusAccepted, l)
}

// handleListLinks atiende GET /links?status=.
func (s *Server) handleListLinks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "" && !slices.Contains(links.Statuses, status) {
		s.fail(w, r, badRequest("status inválido"))
		return
	}
	writeJSON(w, http.StatusOK, s.links.List(status))
}

// handleGetLink atiende GET /links/{id}.
func (s *Server) handleGetLink(w http.ResponseWriter, r *http.Request) {
	l, ok := s.links.Get(r.PathValue("id"))
	if !ok {
		s.fail(w, r, errNotFound)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// handleRevokeLink atiende DELETE /links/{id}.
func (s *Server) handleRevokeLink(w http.ResponseWriter, r *http.Request) {
	if err := s.links.Revoke(r.Context(), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func decodeStrict(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("datos sobrantes")
	}
	return nil
}
