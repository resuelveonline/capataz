// Package nubetest es una Capataz Nube falsa en memoria para pruebas. Cumple
// api/capataz-nube-api.yaml en lo que usa Capataz: verifica la firma HMAC en
// el orden del contrato, guarda partes, comprueba el SHA-256 al completar,
// hace cumplir el vencimiento y sirve /s/{token}/meta y /s/{token}/blob para
// que las pruebas descarguen y descifren de extremo a extremo.
package nubetest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/nube"
)

// Límites por defecto, los del contrato.
var DefaultLimits = nube.Limits{
	MaxEncryptedBytes: 524296032,
	MinPartSize:       1 << 20,
	MaxPartSize:       8 << 20,
	MinTTLSeconds:     3600,
	MaxTTLSeconds:     2592000,
	MaxActiveLinks:    100,
	MaxStoredBytes:    10 << 30,
}

// Server es la nube falsa. Los campos exportados se pueden cambiar entre
// peticiones (bajo Lock cuando la prueba corre en paralelo con Capataz).
type Server struct {
	*httptest.Server
	InstanceID string
	Key        []byte

	mu     sync.Mutex
	Limits nube.Limits
	// Now es el reloj de la nube (por defecto time.Now).
	Now func() time.Time
	// Fail, si no es nil, se consulta antes de atender cada petición de
	// /v1/...; si devuelve un estado distinto de 0, se responde con ese
	// estado y código sin tocar nada. Sirve para inyectar fallos.
	Fail func(r *http.Request) (status int, code string)

	nonces   map[string]time.Time
	links    map[string]*link
	byToken  map[string]string // tokenHash → linkId
	order    []string          // linkIds por orden de creación
	requests map[string]int    // "MÉTODO patrón" → número de peticiones
}

type link struct {
	req       nube.OpenUploadRequest
	reqBody   []byte // cuerpo canónico para la idempotencia
	state     string
	createdAt time.Time
	activated *time.Time
	failure   *string
	parts     map[int][]byte
	blob      []byte
	downloads int
}

// New arranca la nube falsa con una clave aleatoria fija para la prueba.
func New(instanceID string, key []byte) *Server {
	s := &Server{
		InstanceID: instanceID,
		Key:        bytes.Clone(key),
		Limits:     DefaultLimits,
		Now:        time.Now,
		nonces:     map[string]time.Time{},
		links:      map[string]*link{},
		byToken:    map[string]string{},
		requests:   map[string]int{},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Lock y Unlock protegen los campos exportados.
func (s *Server) Lock()   { s.mu.Lock() }
func (s *Server) Unlock() { s.mu.Unlock() }

// LinkView es lo que una prueba puede ver de un enlace.
type LinkView struct {
	State     string
	Parts     int
	Blob      []byte
	Downloads int
	TokenHash string
	ExpiresAt time.Time
}

// Link devuelve el estado de un enlace y si existe.
func (s *Server) Link(id string) (LinkView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.links[id]
	if !ok {
		return LinkView{}, false
	}
	s.expire(l)
	return LinkView{State: l.state, Parts: len(l.parts), Blob: bytes.Clone(l.blob),
		Downloads: l.downloads, TokenHash: l.req.TokenHash, ExpiresAt: l.req.ExpiresAt}, true
}

// Requests cuenta las peticiones atendidas por "MÉTODO patrón", p. ej.
// "PUT /v1/links/{id}/parts/{n}".
func (s *Server) Requests(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[key]
}

var (
	reLinkID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	reB64_43 = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	reNonce  = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	reHex64  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")

	if len(parts) >= 2 && parts[0] == "s" {
		s.public(w, r, parts[1:])
		return
	}
	if len(parts) < 2 || parts[0] != "v1" {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 9<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !s.authenticate(w, r, body) {
		return
	}
	if s.Fail != nil {
		if st, code := s.Fail(r); st != 0 {
			writeErr(w, st, code)
			return
		}
	}

	switch {
	case len(parts) == 2 && parts[1] == "instance" && r.Method == http.MethodGet:
		s.count("GET /v1/instance")
		s.instance(w)
	case len(parts) == 2 && parts[1] == "links" && r.Method == http.MethodPost:
		s.count("POST /v1/links")
		s.open(w, body)
	case len(parts) == 2 && parts[1] == "links" && r.Method == http.MethodGet:
		s.count("GET /v1/links")
		s.list(w, r)
	case len(parts) == 3 && parts[1] == "links" && r.Method == http.MethodGet:
		s.count("GET /v1/links/{id}")
		if l := s.find(w, parts[2]); l != nil {
			writeJSON(w, http.StatusOK, s.view(parts[2], l))
		}
	case len(parts) == 3 && parts[1] == "links" && r.Method == http.MethodDelete:
		s.count("DELETE /v1/links/{id}")
		if l := s.find(w, parts[2]); l != nil {
			if l.state == nube.StateUploading || l.state == nube.StateActive {
				l.state = nube.StateRevoked
				l.parts, l.blob = nil, nil
			}
			w.WriteHeader(http.StatusNoContent)
		}
	case len(parts) == 5 && parts[1] == "links" && parts[3] == "parts" && r.Method == http.MethodPut:
		s.count("PUT /v1/links/{id}/parts/{n}")
		s.part(w, parts[2], parts[4], body)
	case len(parts) == 4 && parts[1] == "links" && parts[3] == "complete" && r.Method == http.MethodPost:
		s.count("POST /v1/links/{id}/complete")
		s.complete(w, parts[2], body)
	default:
		writeErr(w, http.StatusNotFound, "not_found")
	}
}

func (s *Server) count(k string) { s.requests[k]++ }

// authenticate aplica las reglas del contrato en su orden: instancia,
// ventana de tiempo, hash del cuerpo, firma y nonce.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, body []byte) bool {
	h := r.Header
	if h.Get("X-Capataz-Instance") != s.InstanceID {
		writeErr(w, http.StatusUnauthorized, "unknown_instance")
		return false
	}
	now := s.Now()
	ts, err := strconv.ParseInt(h.Get("X-Capataz-Timestamp"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	if d := now.Unix() - ts; d > 300 || d < -300 {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"code": "timestamp_out_of_window", "message": "reloj", "serverTime": now.Unix()})
		return false
	}
	sum := sha256.Sum256(body)
	bodyHex := hex.EncodeToString(sum[:])
	if h.Get("X-Capataz-Content-SHA256") != bodyHex {
		writeErr(w, http.StatusBadRequest, "checksum_mismatch")
		return false
	}
	nonce := h.Get("X-Capataz-Nonce")
	if !reNonce.MatchString(nonce) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	m := hmac.New(sha256.New, s.Key)
	io.WriteString(m, nube.CanonicalString(r.Method, r.URL.RequestURI(), h.Get("X-Capataz-Timestamp"), nonce, bodyHex))
	want := base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(want), []byte(h.Get("X-Capataz-Signature"))) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid_signature")
		return false
	}
	for n, t := range s.nonces {
		if now.Sub(t) > 10*time.Minute {
			delete(s.nonces, n)
		}
	}
	if _, used := s.nonces[nonce]; used {
		writeErr(w, http.StatusUnauthorized, "replayed_nonce")
		return false
	}
	s.nonces[nonce] = now
	return true
}

func (s *Server) instance(w http.ResponseWriter) {
	var u nube.Usage
	for _, l := range s.links {
		s.expire(l)
		if l.state == nube.StateActive || l.state == nube.StateUploading {
			if l.state == nube.StateActive {
				u.ActiveLinks++
			}
			u.StoredBytes += l.req.EncryptedSize
		}
	}
	writeJSON(w, http.StatusOK, nube.Instance{InstanceID: s.InstanceID, ServerTime: s.Now().Unix(), Limits: s.Limits, Usage: u})
}

func (s *Server) open(w http.ResponseWriter, body []byte) {
	var req nube.OpenUploadRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil || !reLinkID.MatchString(req.LinkID) || !reB64_43.MatchString(req.TokenHash) ||
		req.CipherVersion != "cpk1" || req.EncryptedSize < 48 || req.MetaBlob == "" ||
		(req.MaxDownloads != nil && *req.MaxDownloads < 1) {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	meta, err := base64.RawURLEncoding.DecodeString(req.MetaBlob)
	if err != nil || len(meta) > 192<<10 {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	canon, _ := json.Marshal(req)
	if l, ok := s.links[req.LinkID]; ok {
		if !bytes.Equal(l.reqBody, canon) {
			writeErr(w, http.StatusConflict, "conflict")
			return
		}
		s.expire(l)
		writeJSON(w, http.StatusOK, s.view(req.LinkID, l))
		return
	}
	if _, dup := s.byToken[req.TokenHash]; dup {
		writeErr(w, http.StatusConflict, "conflict")
		return
	}
	now := s.Now()
	ttl := req.ExpiresAt.Sub(now)
	if ttl < time.Duration(s.Limits.MinTTLSeconds-300)*time.Second || ttl > time.Duration(s.Limits.MaxTTLSeconds+300)*time.Second ||
		req.PartSize < s.Limits.MinPartSize || req.PartSize > s.Limits.MaxPartSize {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if req.EncryptedSize > s.Limits.MaxEncryptedBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "payload_too_large")
		return
	}
	var active int
	var stored int64
	for _, l := range s.links {
		s.expire(l)
		if l.state == nube.StateActive || l.state == nube.StateUploading {
			active++
			stored += l.req.EncryptedSize
		}
	}
	if active >= s.Limits.MaxActiveLinks || stored+req.EncryptedSize > s.Limits.MaxStoredBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "quota_exceeded")
		return
	}
	l := &link{req: req, reqBody: canon, state: nube.StateUploading, createdAt: now, parts: map[int][]byte{}}
	s.links[req.LinkID] = l
	s.byToken[req.TokenHash] = req.LinkID
	s.order = append(s.order, req.LinkID)
	writeJSON(w, http.StatusCreated, s.view(req.LinkID, l))
}

func (s *Server) part(w http.ResponseWriter, id, num string, body []byte) {
	l := s.find(w, id)
	if l == nil {
		return
	}
	if !s.gone(w, l) {
		return
	}
	if l.state != nube.StateUploading {
		writeErr(w, http.StatusConflict, "conflict")
		return
	}
	n, err := strconv.Atoi(num)
	count := partCount(l.req)
	if err != nil || n < 1 || n > count {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	want := int64(l.req.PartSize)
	if n == count {
		want = l.req.EncryptedSize - int64(count-1)*int64(l.req.PartSize)
	}
	if int64(len(body)) != want {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	l.parts[n] = bytes.Clone(body)
	sum := sha256.Sum256(body)
	writeJSON(w, http.StatusOK, nube.PartReceipt{PartNumber: n, Size: len(body), SHA256: hex.EncodeToString(sum[:])})
}

func (s *Server) complete(w http.ResponseWriter, id string, body []byte) {
	l := s.find(w, id)
	if l == nil {
		return
	}
	var req struct {
		CiphertextSha256 string `json:"ciphertextSha256"`
	}
	if json.Unmarshal(body, &req) != nil || !reHex64.MatchString(req.CiphertextSha256) {
		writeErr(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !s.gone(w, l) {
		return
	}
	if l.state == nube.StateActive {
		sum := sha256.Sum256(l.blob)
		if hex.EncodeToString(sum[:]) == req.CiphertextSha256 {
			writeJSON(w, http.StatusOK, s.view(id, l))
		} else {
			writeErr(w, http.StatusConflict, "conflict")
		}
		return
	}
	if l.state != nube.StateUploading {
		writeErr(w, http.StatusConflict, "conflict")
		return
	}
	var missing []int
	for n := 1; n <= partCount(l.req); n++ {
		if _, ok := l.parts[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"code": "parts_missing", "message": "faltan partes", "missingParts": missing})
		return
	}
	var blob []byte
	for n := 1; n <= partCount(l.req); n++ {
		blob = append(blob, l.parts[n]...)
	}
	sum := sha256.Sum256(blob)
	if hex.EncodeToString(sum[:]) != req.CiphertextSha256 {
		code := "checksum_mismatch"
		l.state, l.failure, l.parts = nube.StateFailed, &code, nil
		writeErr(w, http.StatusUnprocessableEntity, "checksum_mismatch")
		return
	}
	now := s.Now()
	l.state, l.activated, l.blob, l.parts = nube.StateActive, &now, blob, map[int][]byte{}
	writeJSON(w, http.StatusOK, s.view(id, l))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeErr(w, http.StatusBadRequest, "invalid_request")
			return
		}
		limit = n
	}
	start := 0
	if c := q.Get("cursor"); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_request")
			return
		}
		start = n
	}
	ids := slices.Clone(s.order)
	slices.Reverse(ids) // del más reciente al más antiguo
	var out nube.LinkList
	out.Items = []nube.Link{}
	i := 0
	for _, id := range ids {
		l := s.links[id]
		s.expire(l)
		if st := q.Get("state"); st != "" && l.state != st {
			continue
		}
		if i >= start && len(out.Items) < limit {
			out.Items = append(out.Items, s.view(id, l))
		}
		i++
	}
	if start+len(out.Items) < i {
		next := strconv.Itoa(start + len(out.Items))
		out.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, out)
}

// public atiende /s/{token}/meta y /s/{token}/blob (sin firma).
func (s *Server) public(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) != 2 || r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	sum := sha256.Sum256([]byte(parts[0]))
	id, ok := s.byToken[base64.RawURLEncoding.EncodeToString(sum[:])]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}
	l := s.links[id]
	if !s.gone(w, l) {
		return
	}
	if l.state != nube.StateActive {
		writeErr(w, http.StatusConflict, "link_not_ready")
		return
	}
	switch parts[1] {
	case "meta":
		s.count("GET /s/{token}/meta")
		var remaining *int
		if l.req.MaxDownloads != nil {
			n := *l.req.MaxDownloads - l.downloads
			remaining = &n
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": "active", "expiresAt": l.req.ExpiresAt,
			"encryptedSize": l.req.EncryptedSize, "cipherVersion": "cpk1", "metaBlob": l.req.MetaBlob,
			"downloadsRemaining": remaining})
	case "blob":
		s.count("GET /s/{token}/blob")
		// Sin Range: cuenta como descarga (el contrato también cuenta
		// bytes=0-...; la falsa no implementa Range).
		if l.req.MaxDownloads != nil && l.downloads >= *l.req.MaxDownloads {
			writeErr(w, http.StatusGone, "downloads_exhausted")
			return
		}
		l.downloads++
		if l.req.MaxDownloads != nil && l.downloads >= *l.req.MaxDownloads {
			// Se sirve esta descarga; las siguientes ya no.
			defer func() { l.state = nube.StateExhausted }()
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(l.blob)
	default:
		writeErr(w, http.StatusNotFound, "not_found")
	}
}

func (s *Server) find(w http.ResponseWriter, id string) *link {
	l, ok := s.links[id]
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found")
		return nil
	}
	s.expire(l)
	return l
}

// gone responde 410 si el enlace venció, se revocó o se agotó.
func (s *Server) gone(w http.ResponseWriter, l *link) bool {
	switch l.state {
	case nube.StateExpired:
		writeErr(w, http.StatusGone, "link_expired")
	case nube.StateRevoked:
		writeErr(w, http.StatusGone, "link_revoked")
	case nube.StateExhausted:
		writeErr(w, http.StatusGone, "downloads_exhausted")
	default:
		return true
	}
	return false
}

func (s *Server) expire(l *link) {
	if (l.state == nube.StateUploading || l.state == nube.StateActive) && !s.Now().Before(l.req.ExpiresAt) {
		l.state = nube.StateExpired
		l.parts, l.blob = nil, nil
	}
}

func (s *Server) view(id string, l *link) nube.Link {
	received := []int{}
	for n := range l.parts {
		received = append(received, n)
	}
	slices.Sort(received)
	return nube.Link{LinkID: id, State: l.state, CreatedAt: l.createdAt, ActivatedAt: l.activated,
		ExpiresAt: l.req.ExpiresAt, EncryptedSize: l.req.EncryptedSize, PartSize: l.req.PartSize,
		PartCount: partCount(l.req), ReceivedParts: received, Downloads: l.downloads,
		MaxDownloads: l.req.MaxDownloads, FailureCode: l.failure}
}

func partCount(r nube.OpenUploadRequest) int {
	return int((r.EncryptedSize + int64(r.PartSize) - 1) / int64(r.PartSize))
}

func writeErr(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"code": code, "message": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
