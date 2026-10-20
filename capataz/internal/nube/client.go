// Package nube es el cliente de la API de instancia de Capataz Nube
// (api/capataz-nube-api.yaml): peticiones firmadas con HMAC-SHA256 para
// abrir, subir por partes, completar, consultar y revocar enlaces.
//
// Capataz siempre inicia la conexión; la nube nunca llama al host. Ningún
// error de este paquete incluye la clave de instancia, firmas, tokens ni el
// cuerpo de las peticiones, para que nada de eso llegue a la bitácora.
package nube

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ErrUnreachable: la nube no responde, corta la conexión o falla con 5xx.
// Es transitorio: quien llama puede reintentar.
var ErrUnreachable = errors.New("capataz nube no disponible")

var errInvalidLinkID = errors.New("nube: linkId inválido")

// ValidLinkID comprueba el patrón ^[A-Za-z0-9_-]{8,64}$ del contrato. Así
// el linkId nunca necesita escaparse en la ruta.
func ValidLinkID(id string) bool {
	if len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// KeySize es el largo de la clave de instancia (CAPATAZ_NUBE_KEY).
const KeySize = 32

const (
	defaultTimeout = 30 * time.Second
	partTimeout    = 5 * time.Minute // una parte de 8 MiB por un enlace lento
	maxBody        = 4 << 20         // tope de lectura de respuestas JSON
)

// Error es una respuesta de error de la nube (esquema CloudError). No
// guarda el mensaje del servidor: basta el código estable.
type Error struct {
	Status       int
	Code         string
	ServerTime   int64         // solo con timestamp_out_of_window
	MissingParts []int         // solo con parts_missing
	RetryAfter   time.Duration // solo con 429
}

func (e *Error) Error() string {
	return fmt.Sprintf("capataz nube: %d %s", e.Status, e.Code)
}

// IsCode indica si err es un *Error de la nube con ese código.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Client habla con Capataz Nube en nombre de una instancia.
type Client struct {
	base       *url.URL
	instanceID string
	key        []byte
	http       *http.Client
	now        func() time.Time
	skew       atomic.Int64 // segundos que la nube va por delante del host
}

// New crea un cliente para baseURL (p. ej. https://sync.softmx.net). Exige
// https salvo en loopback, que solo se usa en desarrollo y pruebas.
func New(baseURL, instanceID string, key []byte) (*Client, error) {
	u, err := parseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if len(key) != KeySize {
		return nil, errors.New("nube: la clave de instancia debe medir 32 bytes")
	}
	if instanceID == "" {
		return nil, errors.New("nube: falta instanceId")
	}
	return &Client{
		base:       u,
		instanceID: instanceID,
		key:        bytes.Clone(key),
		http: &http.Client{
			// Sin redirecciones: una firma vale solo para su destino.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		now: time.Now,
	}, nil
}

// CheckBaseURL valida una URL base con las mismas reglas que New.
func CheckBaseURL(baseURL string) error {
	_, err := parseBaseURL(baseURL)
	return err
}

func parseBaseURL(baseURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("nube: URL base inválida")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, errors.New("nube: la URL base debe ser https")
	}
	return u, nil
}

// BaseURL devuelve la URL base, sin "/" final. Con ella se arma la URL
// pública del enlace: BaseURL + "/s/" + token + "#" + clave.
func (c *Client) BaseURL() string { return c.base.String() }

// ParseKey decodifica CAPATAZ_NUBE_KEY: 32 bytes en base64url sin relleno
// (43 caracteres). Acepta también base64 estándar con relleno.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == KeySize {
			return b, nil
		}
	}
	return nil, errors.New("la clave de instancia debe ser de 32 bytes en base64url")
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Limits y Usage son CloudLimits y CloudUsage.
type Limits struct {
	MaxEncryptedBytes int64 `json:"maxEncryptedBytes"`
	MinPartSize       int   `json:"minPartSize"`
	MaxPartSize       int   `json:"maxPartSize"`
	MinTTLSeconds     int   `json:"minTtlSeconds"`
	MaxTTLSeconds     int   `json:"maxTtlSeconds"`
	MaxActiveLinks    int   `json:"maxActiveLinks"`
	MaxStoredBytes    int64 `json:"maxStoredBytes"`
}

type Usage struct {
	ActiveLinks int   `json:"activeLinks"`
	StoredBytes int64 `json:"storedBytes"`
}

// Instance es CloudInstance.
type Instance struct {
	InstanceID string `json:"instanceId"`
	ServerTime int64  `json:"serverTime"`
	Limits     Limits `json:"limits"`
	Usage      Usage  `json:"usage"`
}

// Estados de CloudLinkState.
const (
	StateUploading = "uploading"
	StateActive    = "active"
	StateExpired   = "expired"
	StateExhausted = "exhausted"
	StateRevoked   = "revoked"
	StateFailed    = "failed"
)

// Link es CloudLink.
type Link struct {
	LinkID        string     `json:"linkId"`
	State         string     `json:"state"`
	CreatedAt     time.Time  `json:"createdAt"`
	ActivatedAt   *time.Time `json:"activatedAt"`
	ExpiresAt     time.Time  `json:"expiresAt"`
	EncryptedSize int64      `json:"encryptedSize"`
	PartSize      int        `json:"partSize"`
	PartCount     int        `json:"partCount"`
	ReceivedParts []int      `json:"receivedParts"`
	Downloads     int        `json:"downloads"`
	MaxDownloads  *int       `json:"maxDownloads"`
	FailureCode   *string    `json:"failureCode"`
}

// LinkList es CloudLinkList.
type LinkList struct {
	Items      []Link  `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// OpenUploadRequest es el cuerpo de POST /v1/links.
type OpenUploadRequest struct {
	LinkID        string    `json:"linkId"`
	TokenHash     string    `json:"tokenHash"` // base64url(SHA-256(token))
	ExpiresAt     time.Time `json:"expiresAt"`
	MaxDownloads  *int      `json:"maxDownloads"`
	CipherVersion string    `json:"cipherVersion"`
	EncryptedSize int64     `json:"encryptedSize"`
	PartSize      int       `json:"partSize"`
	MetaBlob      string    `json:"metaBlob"` // base64url
}

// PartReceipt es la respuesta de PUT /v1/links/{id}/parts/{n}.
type PartReceipt struct {
	PartNumber int    `json:"partNumber"`
	Size       int    `json:"size"`
	SHA256     string `json:"sha256"`
}

// Instance consulta GET /v1/instance (credenciales, reloj, límites y uso).
func (c *Client) Instance(ctx context.Context) (Instance, error) {
	var out Instance
	return out, c.call(ctx, http.MethodGet, "/v1/instance", nil, nil, &out)
}

// OpenUpload abre (o reanuda, si ya existe con el mismo cuerpo) la subida.
func (c *Client) OpenUpload(ctx context.Context, req OpenUploadRequest) (Link, error) {
	var out Link
	if !ValidLinkID(req.LinkID) {
		return out, errInvalidLinkID
	}
	req.ExpiresAt = req.ExpiresAt.UTC().Truncate(time.Second)
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	return out, c.call(ctx, http.MethodPost, "/v1/links", nil, body, &out)
}

// UploadPart sube la parte n (desde 1). La parte entera va en memoria
// porque su SHA-256 viaja firmado en un header antes del cuerpo; mide 8 MiB
// como máximo.
func (c *Client) UploadPart(ctx context.Context, linkID string, n int, part []byte) (PartReceipt, error) {
	var out PartReceipt
	if !ValidLinkID(linkID) || n < 1 {
		return out, errors.New("nube: parte inválida")
	}
	ctx, cancel := withDefaultTimeout(ctx, partTimeout)
	defer cancel()
	p := "/v1/links/" + linkID + "/parts/" + strconv.Itoa(n)
	if err := c.call(ctx, http.MethodPut, p, nil, part, &out); err != nil {
		return out, err
	}
	sum := sha256.Sum256(part)
	if out.PartNumber != n || out.Size != len(part) || out.SHA256 != hex.EncodeToString(sum[:]) {
		return out, errors.New("nube: el recibo de la parte no coincide con lo enviado")
	}
	return out, nil
}

// Complete cierra la subida con el SHA-256 (hex) del paquete completo.
func (c *Client) Complete(ctx context.Context, linkID, packageSHA256 string) (Link, error) {
	var out Link
	if !ValidLinkID(linkID) {
		return out, errInvalidLinkID
	}
	body, err := json.Marshal(map[string]string{"ciphertextSha256": packageSHA256})
	if err != nil {
		return out, err
	}
	return out, c.call(ctx, http.MethodPost, "/v1/links/"+linkID+"/complete", nil, body, &out)
}

// Link consulta GET /v1/links/{id}.
func (c *Client) Link(ctx context.Context, linkID string) (Link, error) {
	var out Link
	if !ValidLinkID(linkID) {
		return out, errInvalidLinkID
	}
	return out, c.call(ctx, http.MethodGet, "/v1/links/"+linkID, nil, nil, &out)
}

// Links lista una página de enlaces (state y cursor opcionales; limit 0
// usa el valor por defecto de la nube).
func (c *Client) Links(ctx context.Context, state, cursor string, limit int) (LinkList, error) {
	var out LinkList
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return out, c.call(ctx, http.MethodGet, "/v1/links", q, nil, &out)
}

// Revoke revoca el enlace y borra sus bytes. Es idempotente en la nube; un
// enlace que nunca existió devuelve *Error con code not_found.
func (c *Client) Revoke(ctx context.Context, linkID string) error {
	if !ValidLinkID(linkID) {
		return errInvalidLinkID
	}
	return c.call(ctx, http.MethodDelete, "/v1/links/"+linkID, nil, nil, nil)
}

// call firma y envía la petición. Si la nube responde
// timestamp_out_of_window, corrige el desfase con su serverTime y reintenta
// una vez (con nonce nuevo).
func (c *Client) call(ctx context.Context, method, path string, q url.Values, body []byte, out any) error {
	ctx, cancel := withDefaultTimeout(ctx, defaultTimeout)
	defer cancel()
	err := c.once(ctx, method, path, q, body, out)
	var e *Error
	if errors.As(err, &e) && e.Code == "timestamp_out_of_window" && e.ServerTime > 0 {
		c.skew.Store(e.ServerTime - c.now().Unix())
		err = c.once(ctx, method, path, q, body, out)
	}
	return err
}

func (c *Client) once(ctx context.Context, method, path string, q url.Values, body []byte, out any) error {
	u := *c.base
	u.Path = c.base.Path + path
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("nube: petición inválida")
	}
	if body == nil {
		req.Body, req.GetBody, req.ContentLength = http.NoBody, nil, 0
	}
	if err := c.sign(req, body); err != nil {
		return err
	}
	if len(body) > 0 {
		ct := "application/json"
		if method == http.MethodPut {
			ct = "application/octet-stream"
		}
		req.Header.Set("Content-Type", ct)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// El error de net/http trae la URL; basta con decir qué pasó.
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", ErrUnreachable, ctx.Err())
		}
		return ErrUnreachable
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return ErrUnreachable
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil || resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return errors.New("nube: respuesta JSON inválida")
		}
		return nil
	}
	return responseError(resp, data)
}

func responseError(resp *http.Response, data []byte) error {
	var body struct {
		Code         string `json:"code"`
		ServerTime   int64  `json:"serverTime"`
		MissingParts []int  `json:"missingParts"`
	}
	if json.Unmarshal(data, &body) != nil || body.Code == "" {
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			return fmt.Errorf("%w: estado %d", ErrUnreachable, resp.StatusCode)
		}
		return &Error{Status: resp.StatusCode, Code: "invalid_response"}
	}
	e := &Error{Status: resp.StatusCode, Code: body.Code, ServerTime: body.ServerTime, MissingParts: body.MissingParts}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: %w", ErrUnreachable, e)
	}
	return e
}

// sign pone los cinco headers de autenticación. La cadena canónica es
// MÉTODO, destino (ruta y consulta tal como viajan), timestamp, nonce y
// SHA-256 hex del cuerpo, separados por "\n" y sin salto final.
func (c *Client) sign(req *http.Request, body []byte) error {
	var nb [16]byte
	if _, err := rand.Read(nb[:]); err != nil {
		return err
	}
	nonce := base64.RawURLEncoding.EncodeToString(nb[:])
	ts := strconv.FormatInt(c.now().Unix()+c.skew.Load(), 10)
	sum := sha256.Sum256(body)
	bodyHex := hex.EncodeToString(sum[:])

	m := hmac.New(sha256.New, c.key)
	io.WriteString(m, CanonicalString(req.Method, req.URL.RequestURI(), ts, nonce, bodyHex))

	h := req.Header
	h.Set("X-Capataz-Instance", c.instanceID)
	h.Set("X-Capataz-Timestamp", ts)
	h.Set("X-Capataz-Nonce", nonce)
	h.Set("X-Capataz-Content-SHA256", bodyHex)
	h.Set("X-Capataz-Signature", base64.RawURLEncoding.EncodeToString(m.Sum(nil)))
	return nil
}

// CanonicalString arma la cadena que se firma. Se exporta para que la nube
// falsa de las pruebas verifique con la misma definición del contrato.
func CanonicalString(method, target, timestamp, nonce, bodySHA256Hex string) string {
	return strings.Join([]string{method, target, timestamp, nonce, bodySHA256Hex}, "\n")
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}
