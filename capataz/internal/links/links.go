// Package links gestiona los enlaces temporales: los acepta (POST /links),
// prepara su copia congelada y su paquete CPK1 (prepare), los sube a Capataz
// Nube (nube) y lleva su estado hasta que vencen o se revocan.
//
// Estados: preparing → uploading → active, y de cualquiera de ellos a
// failed, expired o revoked. El estado vive en CAPATAZ_STATE_DIR/links; tras
// un reinicio, una subida a medias se reanuda y una preparación a medias se
// da por fallida.
//
// La bitácora solo lleva linkId, estados, contadores, tamaños y códigos:
// nunca rutas, nombres, la etiqueta, el token ni la clave.
package links

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/resuelveonline/capataz/capataz/internal/nube"
	"github.com/resuelveonline/capataz/capataz/internal/prepare"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
)

// Estados de Link.status.
const (
	StatusPreparing = "preparing"
	StatusUploading = "uploading"
	StatusActive    = "active"
	StatusFailed    = "failed"
	StatusExpired   = "expired"
	StatusRevoked   = "revoked"
)

// Statuses son los valores válidos de Link.status.
var Statuses = []string{StatusPreparing, StatusUploading, StatusActive, StatusFailed, StatusExpired, StatusRevoked}

// Límites de LinkRequest.
const (
	MinTTL        = time.Hour
	MaxTTL        = 30 * 24 * time.Hour
	DefaultTTL    = 24 * time.Hour
	MaxItems      = 100
	MaxLabelRunes = 120
)

// Códigos de Error.code del contrato que produce este paquete (además de
// los de stage).
const (
	CodeInvalidRequest  = "invalid_request"
	CodeNotFound        = "not_found"
	CodeRateLimited     = "rate_limited"
	CodeCloudUnreach    = "cloud_unreachable"
	CodeCloudQuota      = "cloud_quota_exceeded"
	CodePayloadTooLarge = "payload_too_large"
	CodeInternal        = "internal_error"
)

// Error es un rechazo con código del contrato. El mensaje nunca incluye
// rutas ni nombres.
type Error struct {
	Code    string
	Message string
	Err     error
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func (e *Error) Unwrap() error { return e.Err }

func newError(code, msg string) *Error { return &Error{Code: code, Message: msg} }

// Item es un elemento seleccionado.
type Item struct {
	Folder string `json:"folder"`
	Path   string `json:"path"`
}

// Request es LinkRequest ya decodificado.
type Request struct {
	Items        []Item
	TTL          time.Duration // 0 = DefaultTTL
	MaxDownloads *int
	Label        string
	Encryption   string // "" o "client"
}

// Link es el esquema Link del contrato.
type Link struct {
	ID            string         `json:"id"`
	URL           *string        `json:"url"`
	Label         *string        `json:"label"`
	Status        string         `json:"status"`
	Progress      float64        `json:"progress"`
	FailureCode   *string        `json:"failureCode"`
	CreatedAt     time.Time      `json:"createdAt"`
	ExpiresAt     time.Time      `json:"expiresAt"`
	Downloads     int            `json:"downloads"`
	MaxDownloads  *int           `json:"maxDownloads"`
	Items         []Item         `json:"items"`
	PayloadSize   *int64         `json:"payloadSize"`
	PayloadSha256 *string        `json:"payloadSha256"`
	PackageSize   *int64         `json:"packageSize"`
	PackageSha256 *string        `json:"packageSha256"`
	Skipped       *stage.Skipped `json:"skipped"`
	Encryption    string         `json:"encryption"`
}

// record es lo que se persiste: el Link (sin url, que se arma al vuelo) y
// los datos para reanudar. Token y Key son secretos y se borran en cuanto
// el enlace deja de estar vigente.
type record struct {
	Link
	Owner string                  `json:"owner"`           // deviceId de quien lo creó
	TTL   int64                   `json:"ttlSeconds"`      // vigencia pedida
	Token string                  `json:"token,omitempty"` // base64url, 43 caracteres
	Key   string                  `json:"key,omitempty"`   // clave K en base64url
	Open  *nube.OpenUploadRequest `json:"open,omitempty"`  // cuerpo exacto de la apertura
}

func (r *record) terminal() bool {
	return r.Status == StatusFailed || r.Status == StatusExpired || r.Status == StatusRevoked
}

// Options configura un Manager.
type Options struct {
	StateDir   string
	StagingDir string
	Stager     *stage.Stager
	Nube       *nube.Client // nil: enlaces desactivados (cloud_unreachable)
	Logger     *slog.Logger
	Now        func() time.Time // por defecto time.Now

	MaxActive    int           // preparaciones y subidas a la vez (por defecto 2)
	Retry        nube.Retry    // reintentos por petición a la nube
	SweepEvery   time.Duration // vencimiento local (por defecto 30 s)
	RefreshEvery time.Duration // conciliación con la nube (por defecto 5 min)
}

// Manager es el dueño de todos los enlaces de la instancia.
type Manager struct {
	o     Options
	log   *slog.Logger
	store *store

	mu   sync.Mutex
	recs map[string]*record
	jobs map[string]context.CancelFunc

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

const linkDirPrefix = "link-"

// Open carga los enlaces guardados y recupera los que quedaron a medias:
// preparing pasa a failed y uploading queda para reanudarse en Start.
func Open(o Options) (*Manager, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxActive <= 0 {
		o.MaxActive = 2
	}
	if o.Retry.Attempts == 0 {
		o.Retry = nube.Retry{Attempts: 8, Base: 2 * time.Second, Max: time.Minute}
	}
	o.SweepEvery = cmp.Or(o.SweepEvery, 30*time.Second)
	o.RefreshEvery = cmp.Or(o.RefreshEvery, 5*time.Minute)

	st, err := openStore(o.StateDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{o: o, log: o.Logger, store: st, recs: map[string]*record{}, jobs: map[string]context.CancelFunc{}}
	m.ctx, m.cancel = context.WithCancel(context.Background())

	recs, damaged, err := st.loadAll()
	if err != nil {
		return nil, err
	}
	if damaged > 0 {
		m.log.Warn("registros de enlace dañados", "cantidad", damaged)
	}
	for _, r := range recs {
		m.recs[r.ID] = r
		if r.Status == StatusPreparing {
			// La copia y la clave estaban solo en memoria y en un staging a
			// medias: no hay nada que reanudar.
			m.finish(r, StatusFailed, CodeInternal)
		}
	}
	m.removeOrphanDirs()
	return m, nil
}

// Start lanza las tareas de fondo: reanuda subidas, vence enlaces y
// concilia con la nube. Close las detiene.
func (m *Manager) Start() {
	m.mu.Lock()
	for _, r := range m.recs {
		if r.Status == StatusUploading {
			m.startJob(r.ID, m.upload)
		}
	}
	m.mu.Unlock()

	m.wg.Go(func() {
		sweep := time.NewTicker(m.o.SweepEvery)
		refresh := time.NewTicker(m.o.RefreshEvery)
		defer sweep.Stop()
		defer refresh.Stop()
		m.Sweep()
		for {
			select {
			case <-m.ctx.Done():
				return
			case <-sweep.C:
				m.Sweep()
			case <-refresh.C:
				m.Refresh(m.ctx)
			}
		}
	})
}

// Close cancela las tareas en curso y espera a que terminen. Una subida
// cancelada sigue en uploading y se reanuda en el próximo arranque.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

// Enabled indica si hay nube configurada.
func (m *Manager) Enabled() bool { return m.o.Nube != nil }

// Create valida la petición, guarda el enlace en preparing y lanza su
// preparación. Solo rechaza aquí lo que se puede saber sin copiar.
func (m *Manager) Create(ctx context.Context, owner string, req Request) (Link, error) {
	if err := validate(&req); err != nil {
		return Link{}, err
	}
	if m.o.Nube == nil {
		return Link{}, newError(CodeCloudUnreach, "Capataz Nube no está configurada")
	}
	items := make([]stage.Item, len(req.Items))
	for i, it := range req.Items {
		items[i] = stage.Item{Folder: it.Folder, Path: it.Path}
	}
	if err := m.o.Stager.Check(ctx, items); err != nil {
		return Link{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	busy := 0
	for _, r := range m.recs {
		if r.Status == StatusPreparing || r.Status == StatusUploading {
			busy++
		}
	}
	if busy >= m.o.MaxActive {
		return Link{}, newError(CodeRateLimited, "hay demasiados enlaces en preparación")
	}

	now := m.o.Now().UTC().Truncate(time.Millisecond)
	r := &record{
		Link: Link{
			ID: prepare.NewLinkID(), Status: StatusPreparing, CreatedAt: now,
			// Provisional: la vigencia definitiva empieza al abrir la subida.
			ExpiresAt:    now.Add(req.TTL),
			MaxDownloads: req.MaxDownloads, Items: req.Items, Encryption: "client",
		},
		Owner: owner,
		TTL:   int64(req.TTL / time.Second),
	}
	if req.Label != "" {
		r.Label = &req.Label
	}
	if err := m.store.save(r); err != nil {
		return Link{}, &Error{Code: CodeInternal, Message: "no se pudo guardar el enlace", Err: err}
	}
	m.recs[r.ID] = r
	m.log.Info("enlace solicitado", "linkId", r.ID, "owner", owner, "elementos", len(r.Items),
		"ttlSeconds", r.TTL, "maxDownloads", r.MaxDownloads != nil)
	m.startJob(r.ID, m.run)
	return m.view(r), nil
}

func validate(req *Request) error {
	switch {
	case len(req.Items) == 0:
		return newError(CodeInvalidRequest, "items no puede estar vacío")
	case len(req.Items) > MaxItems:
		return newError(CodeInvalidRequest, "demasiados elementos")
	case req.Encryption != "" && req.Encryption != "client":
		return newError(CodeInvalidRequest, "solo se admite encryption=client")
	case req.MaxDownloads != nil && *req.MaxDownloads < 1:
		return newError(CodeInvalidRequest, "maxDownloads debe ser al menos 1")
	case !utf8.ValidString(req.Label) || utf8.RuneCountInString(req.Label) > MaxLabelRunes:
		return newError(CodeInvalidRequest, "label demasiado largo o inválido")
	}
	if req.TTL == 0 {
		req.TTL = DefaultTTL
	}
	if req.TTL < MinTTL || req.TTL > MaxTTL || req.TTL%time.Second != 0 {
		return newError(CodeInvalidRequest, "ttlSeconds fuera de rango")
	}
	seen := map[Item]bool{}
	for _, it := range req.Items {
		if it.Folder == "" {
			return newError(CodeInvalidRequest, "falta folder")
		}
		if seen[it] {
			return newError(CodeInvalidRequest, "elementos repetidos")
		}
		seen[it] = true
	}
	return nil
}

// Get devuelve un enlace.
func (m *Manager) Get(id string) (Link, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[id]
	if !ok {
		return Link{}, false
	}
	return m.view(r), true
}

// List devuelve los enlaces (con status, solo los de ese estado), del más
// reciente al más antiguo.
func (m *Manager) List(status string) []Link {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Link{}
	for _, r := range m.recs {
		if status == "" || r.Status == status {
			out = append(out, m.view(r))
		}
	}
	slices.SortFunc(out, func(a, b Link) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	return out
}

// ErrNotFound: no existe un enlace con ese id.
var ErrNotFound = newError(CodeNotFound, "no existe")

// Revoke revoca un enlace. preparing se cancela en local; uploading y
// active se revocan primero en la nube, y si no responde el enlace queda
// como estaba. Los ya terminados no cambian (idempotente).
func (m *Manager) Revoke(ctx context.Context, id string) error {
	m.mu.Lock()
	r, ok := m.recs[id]
	if !ok {
		m.mu.Unlock()
		return ErrNotFound
	}
	if r.terminal() {
		m.mu.Unlock()
		return nil
	}
	// Solo un enlace activo o con la subida ya abierta existe en la nube.
	if r.Status == StatusPreparing || r.Status == StatusUploading && r.Open == nil {
		m.finish(r, StatusRevoked, "")
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	// Fuera del candado: la nube puede tardar.
	err := m.o.Retry.Do(ctx, func() error { return m.o.Nube.Revoke(ctx, id) })
	if err != nil && !nube.IsCode(err, "not_found") {
		m.log.Warn("no se pudo revocar en la nube", "linkId", id, "error", err)
		return &Error{Code: CodeCloudUnreach, Message: "no se pudo revocar en Capataz Nube", Err: err}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !r.terminal() {
		m.finish(r, StatusRevoked, "")
	}
	return nil
}

// Sweep vence los enlaces cuya hora pasó. No necesita la nube: ella vence
// los suyos por su cuenta.
func (m *Manager) Sweep() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.o.Now()
	for _, r := range m.recs {
		if !r.terminal() && r.Status != StatusPreparing && !now.Before(r.ExpiresAt) {
			m.finish(r, StatusExpired, "")
		}
	}
}

// Refresh concilia los enlaces activos con la nube: descargas y estados
// que cambiaron allí (agotado, vencido, revocado). Los fallos solo se
// anotan; se reintenta en la próxima pasada.
func (m *Manager) Refresh(ctx context.Context) {
	if m.o.Nube == nil {
		return
	}
	m.mu.Lock()
	active := 0
	for _, r := range m.recs {
		if r.Status == StatusActive {
			active++
		}
	}
	m.mu.Unlock()
	if active == 0 {
		return
	}
	cursor := ""
	for page := 0; page < 100; page++ {
		list, err := m.o.Nube.Links(ctx, "", cursor, 200)
		if err != nil {
			m.log.Warn("no se pudo conciliar con la nube", "error", err)
			return
		}
		m.mu.Lock()
		for _, cl := range list.Items {
			if r, ok := m.recs[cl.LinkID]; ok && r.Status == StatusActive {
				m.reconcile(r, cl)
			}
		}
		m.mu.Unlock()
		if list.NextCursor == nil || *list.NextCursor == "" {
			return
		}
		cursor = *list.NextCursor
	}
}

// reconcile aplica el estado de la nube a un enlace activo (con m.mu).
func (m *Manager) reconcile(r *record, cl nube.Link) {
	if cl.Downloads > r.Downloads {
		m.log.Info("enlace descargado", "linkId", r.ID, "descargas", cl.Downloads)
		r.Downloads = cl.Downloads
		m.persist(r)
	}
	switch cl.State {
	case nube.StateExpired, nube.StateExhausted:
		m.finish(r, StatusExpired, "")
	case nube.StateRevoked:
		m.finish(r, StatusRevoked, "")
	case nube.StateFailed:
		m.finish(r, StatusFailed, cmp.Or(deref(cl.FailureCode), CodeInternal))
	}
}

// --- tareas ---

// startJob lanza fn para el enlace (con m.mu tomado).
func (m *Manager) startJob(id string, fn func(ctx context.Context, id string) error) {
	ctx, cancel := context.WithCancel(m.ctx)
	m.jobs[id] = cancel
	m.wg.Go(func() {
		defer cancel()
		err := fn(ctx, id)
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.jobs, id)
		r := m.recs[id]
		switch {
		case r.terminal():
			// finish pudo borrar el directorio mientras la tarea aún
			// escribía en él.
			os.RemoveAll(m.linkDir(id))
		case err == nil:
		case m.ctx.Err() != nil:
			// Apagado: una subida se reanuda al arrancar; una preparación
			// se da por fallida en Open.
		default:
			code := failureCode(err)
			m.log.Warn("enlace fallido", "linkId", id, "code", code, "error", err)
			m.finish(r, StatusFailed, code)
		}
	})
}

// run prepara el paquete y sigue con la subida en la misma tarea.
func (m *Manager) run(ctx context.Context, id string) error {
	if err := m.prepare(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	done := m.recs[id].terminal()
	m.mu.Unlock()
	if done {
		return nil
	}
	return m.upload(ctx, id)
}

func (m *Manager) prepare(ctx context.Context, id string) error {
	m.mu.Lock()
	r := m.recs[id]
	items := make([]stage.Item, len(r.Items))
	for i, it := range r.Items {
		items[i] = stage.Item{Folder: it.Folder, Path: it.Path}
	}
	label := deref(r.Label)
	m.mu.Unlock()

	dir := m.linkDir(id)
	res, err := prepare.Run(ctx, prepare.Options{
		Stager: m.o.Stager, Items: items, OutDir: dir, Label: label, LinkID: id, Logger: m.log,
	})
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	meta, err := os.ReadFile(filepath.Join(dir, prepare.MetaFile))
	if err != nil {
		return err
	}
	token, err := newToken()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(token))
	mf := res.Manifest

	m.mu.Lock()
	defer m.mu.Unlock()
	if r.terminal() {
		return nil // revocado mientras se preparaba
	}
	r.Token, r.Key = token, res.Key.Base64URL()
	r.PayloadSize, r.PayloadSha256 = &mf.PayloadSize, &mf.PayloadSha256
	r.PackageSize, r.PackageSha256 = &mf.PackageSize, &mf.PackageSha256
	skipped := mf.Skipped
	r.Skipped = &skipped
	r.ExpiresAt = m.o.Now().UTC().Truncate(time.Second).Add(time.Duration(r.TTL) * time.Second)
	r.Open = &nube.OpenUploadRequest{
		LinkID: id, TokenHash: base64.RawURLEncoding.EncodeToString(sum[:]),
		ExpiresAt: r.ExpiresAt, MaxDownloads: r.MaxDownloads, CipherVersion: mf.CipherVersion,
		EncryptedSize: mf.PackageSize, PartSize: nube.PartSize, MetaBlob: string(meta),
	}
	r.Status = StatusUploading
	// Guardar antes de abrir la subida: tras un reinicio se reabre con el
	// mismo cuerpo y la nube la reconoce (idempotente por linkId).
	if err := m.store.save(r); err != nil {
		return err
	}
	m.log.Info("enlace preparado", "linkId", id, "kind", mf.Kind, "packageBytes", mf.PackageSize)
	return nil
}
