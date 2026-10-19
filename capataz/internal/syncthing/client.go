// Package syncthing es el cliente interno de la REST API local de Syncthing.
//
// El API key solo viaja en el header X-API-Key hacia Syncthing. Ningún error
// de este paquete incluye el API key, la URL completa ni nombres de archivo
// (van en la cadena de consulta), para que nada de eso llegue a la bitácora.
package syncthing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrUnavailable: Syncthing no responde, falla o rechaza el API key.
	ErrUnavailable = errors.New("syncthing no disponible")
	// ErrNotIndexed: db/file no conoce el archivo (p. ej. recién creado y
	// todavía sin escanear).
	ErrNotIndexed = errors.New("archivo no indexado por syncthing")
	// ErrNoSuchFolder: Syncthing no tiene una carpeta con ese id.
	ErrNoSuchFolder = errors.New("carpeta desconocida para syncthing")
)

// Respuestas de texto plano de Syncthing v2.1.6, verificadas con curl -i
// (Fase 1). db/file responde notIndexedBody con 404 tanto para un archivo
// fuera del índice como para una carpeta inexistente: por eso Capataz valida
// la carpeta contra config/folders antes de llamar.
const (
	notIndexedBody   = "No such object in the index"
	noSuchFolderBody = "no such folder"
)

const (
	defaultTimeout = 10 * time.Second
	scanTimeout    = 2 * time.Minute // db/scan espera a que termine el escaneo
	maxBody        = 32 << 20        // tope de lectura de respuestas JSON
)

// Client habla con una instancia de Syncthing.
type Client struct {
	base   *url.URL
	apiKey string
	http   *http.Client
}

// New crea un cliente para baseURL (p. ej. http://127.0.0.1:8384).
func New(baseURL, apiKey string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("syncthing: URL base inválida")
	}
	return &Client{
		base:   u,
		apiKey: apiKey,
		http: &http.Client{
			// Sin redirecciones: no hay motivo legítimo y evitaría reenviar
			// el API key a otro destino.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Folder es una carpeta tal como la configura Syncthing.
type Folder struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Path           string `json:"path"` // puede empezar con "~"
	Type           string `json:"type"`
	Paused         bool   `json:"paused"`
	FilesystemType string `json:"filesystemType"`
}

// FileInfo es la vista de un archivo en el índice (campos que usa Capataz).
type FileInfo struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	Modified   time.Time `json:"modified"`
	Type       string    `json:"type"` // FILE_INFO_TYPE_FILE, FILE_INFO_TYPE_DIRECTORY...
	Deleted    bool      `json:"deleted"`
	Ignored    bool      `json:"ignored"`
	Invalid    bool      `json:"invalid"`
	MustRescan bool      `json:"mustRescan"`
	Sequence   int64     `json:"sequence"`
	Version    []string  `json:"version"`
}

// FileStatus es la respuesta de db/file: versión global (la del clúster) y
// local (la de este host).
type FileStatus struct {
	Global FileInfo `json:"global"`
	Local  FileInfo `json:"local"`
}

// Need es la respuesta de db/need: lo que este host aún debe descargar.
type Need struct {
	Progress []FileInfo `json:"progress"` // descargándose ahora
	Queued   []FileInfo `json:"queued"`
	Rest     []FileInfo `json:"rest"`
	Page     int        `json:"page"`
	PerPage  int        `json:"perpage"`
}

// Folders consulta GET /rest/config/folders.
func (c *Client) Folders(ctx context.Context) ([]Folder, error) {
	var out []Folder
	return out, c.getJSON(ctx, "config/folders", nil, &out)
}

// File consulta GET /rest/db/file. Devuelve ErrNotIndexed si Syncthing no
// conoce el archivo. Un archivo borrado NO es error: llega con
// Global.Deleted=true y quien llama decide.
func (c *Client) File(ctx context.Context, folder, file string) (FileStatus, error) {
	var out FileStatus
	q := url.Values{"folder": {folder}, "file": {file}}
	return out, c.getJSON(ctx, "db/file", q, &out)
}

// Scan fuerza POST /rest/db/scan?folder=&sub= y espera a que termine. sub
// vacío escanea la carpeta entera.
func (c *Client) Scan(ctx context.Context, folder, sub string) error {
	q := url.Values{"folder": {folder}}
	if sub != "" {
		q.Set("sub", sub)
	}
	ctx, cancel := withDefaultTimeout(ctx, scanTimeout)
	defer cancel()
	resp, err := c.do(ctx, http.MethodPost, "db/scan", q)
	if err != nil {
		return err
	}
	defer drain(resp)
	return checkStatus(resp, "db/scan")
}

// Need consulta GET /rest/db/need (primera página, hasta 65536 elementos).
func (c *Client) Need(ctx context.Context, folder string) (Need, error) {
	var out Need
	return out, c.getJSON(ctx, "db/need", url.Values{"folder": {folder}}, &out)
}

func (c *Client) getJSON(ctx context.Context, endpoint string, q url.Values, out any) error {
	ctx, cancel := withDefaultTimeout(ctx, defaultTimeout)
	defer cancel()
	resp, err := c.do(ctx, http.MethodGet, endpoint, q)
	if err != nil {
		return err
	}
	defer drain(resp)
	if err := checkStatus(resp, endpoint); err != nil {
		return err
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("syncthing %s: respuesta JSON inválida: %w", endpoint, err)
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, endpoint string, q url.Values) (*http.Response, error) {
	u := *c.base
	u.Path += "/rest/" + endpoint
	u.RawQuery = q.Encode() // codifica "/", espacios, "&", "ñ"... (hallazgo de la Fase 0)

	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("syncthing %s: %w", endpoint, ErrUnavailable)
	}
	req.Header.Set("X-API-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// *url.Error incluye la URL con la consulta (nombres de archivo);
		// se conserva solo la causa de red.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("syncthing %s: %w (%v)", endpoint, ErrUnavailable, err)
	}
	return resp, nil
}

// checkStatus traduce el código HTTP. El cuerpo de error de Syncthing es
// texto plano corto; se lee como mucho 1 KiB y nunca se copia al mensaje.
func checkStatus(resp *http.Response, endpoint string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
	body := strings.TrimSpace(string(b))
	isText := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain")

	switch {
	case resp.StatusCode == http.StatusNotFound && isText && body == notIndexedBody:
		return ErrNotIndexed
	case isText && body == noSuchFolderBody: // 404 en db/need, 500 en db/scan
		return ErrNoSuchFolder
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("syncthing %s: %w (HTTP %d: ¿API key incorrecto?)", endpoint, ErrUnavailable, resp.StatusCode)
	case resp.StatusCode >= 500:
		return fmt.Errorf("syncthing %s: %w (HTTP %d)", endpoint, ErrUnavailable, resp.StatusCode)
	default:
		return fmt.Errorf("syncthing %s: respuesta inesperada HTTP %d", endpoint, resp.StatusCode)
	}
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// drain vacía y cierra el cuerpo para que la conexión se reutilice.
func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}
