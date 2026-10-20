// Comando capataz: servicio de la API de Capataz junto a Syncthing.
//
//	capataz [-env-file F]                    arranca el servidor HTTP
//	capataz prepare [opciones]               prepara un paquete CPK1 (desarrollo)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/api"
	"github.com/resuelveonline/capataz/capataz/internal/auth"
	"github.com/resuelveonline/capataz/capataz/internal/config"
	"github.com/resuelveonline/capataz/capataz/internal/cpk"
	"github.com/resuelveonline/capataz/capataz/internal/links"
	"github.com/resuelveonline/capataz/capataz/internal/nube"
	"github.com/resuelveonline/capataz/capataz/internal/prepare"
	"github.com/resuelveonline/capataz/capataz/internal/stage"
	"github.com/resuelveonline/capataz/capataz/internal/state"
	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// version se inyecta al compilar: -ldflags "-X main.version=...".
var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	args := os.Args[1:]
	var err error
	if len(args) > 0 && args[0] == "prepare" {
		err = runPrepare(context.Background(), logger, args[1:], os.Getenv, os.Stdout)
	} else {
		err = runServe(logger, args)
	}
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(2)
	}
	if err != nil {
		logger.Error("capataz terminó con error", "error", err)
		os.Exit(1)
	}
}

// loadConfig carga el .env (si se pidió) y valida la configuración.
func loadConfig(envFile string, getenv func(string) string) (config.Config, error) {
	if envFile != "" {
		if err := config.LoadDotEnv(envFile); err != nil {
			return config.Config{}, fmt.Errorf("cargando el archivo de entorno: %w", err)
		}
	}
	return config.FromEnv(getenv)
}

func runServe(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("capataz", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "archivo .env a cargar (solo desarrollo)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*envFile, os.Getenv)
	if err != nil {
		return err
	}
	inst, err := state.LoadOrCreate(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("estado de la instancia: %w", err)
	}
	st, err := syncthing.New(cfg.SyncthingURL, cfg.SyncthingAPIKey)
	if err != nil {
		return err
	}
	stager, err := stage.New(stage.Options{
		Syncthing: st, StagingDir: cfg.StagingDir, MaxPayload: cpk.MaxPayload, Logger: log,
	})
	if err != nil {
		return err
	}
	var cloud *nube.Client
	if cfg.NubeKey != nil {
		if cloud, err = nube.New(cfg.NubeURL, inst.InstanceID, cfg.NubeKey); err != nil {
			return err
		}
	} else {
		log.Warn("CAPATAZ_NUBE_KEY vacía: los enlaces temporales están desactivados")
	}
	mgr, err := links.Open(links.Options{
		StateDir: cfg.StateDir, StagingDir: cfg.StagingDir, Stager: stager, Nube: cloud, Logger: log,
	})
	if err != nil {
		return fmt.Errorf("enlaces: %w", err)
	}

	handler := api.New(api.Options{
		Auth:      auth.NewDevToken(cfg.DevToken),
		Syncthing: st,
		Instance: api.Instance{
			InstanceID: inst.InstanceID,
			Name:       cfg.InstanceName,
			Version:    version,
			PublicMode: "local",
		},
		Links:  mgr,
		Logger: log,
	})

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: handler,
		// Sin WriteTimeout ni ReadTimeout globales: cortarían descargas (y
		// futuras subidas) grandes. Las respuestas normales tienen un plazo
		// por petición y las descargas uno por inactividad (paquete api).
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	if !isLoopback(cfg.Listen) {
		log.Warn("Capataz escucha fuera de localhost sin TLS; úsalo solo en redes de confianza", "listen", cfg.Listen)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Comprobación no bloqueante: avisar pronto si Syncthing no responde.
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	if folders, err := st.Folders(checkCtx); err != nil {
		log.Warn("Syncthing no responde todavía", "error", err)
	} else {
		log.Info("Syncthing conectado", "carpetas", len(folders))
	}
	cancel()
	if cloud != nil {
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if ci, err := cloud.Instance(checkCtx); err != nil {
			log.Warn("Capataz Nube no responde todavía", "error", err)
		} else {
			log.Info("Capataz Nube conectada", "enlacesActivos", ci.Usage.ActiveLinks)
		}
		cancel()
	}
	mgr.Start()
	defer mgr.Close() // tras Shutdown: una subida cortada se reanuda al arrancar

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Info("Capataz escuchando", "listen", cfg.Listen, "instanceId", inst.InstanceID, "version", version, "syncthing", cfg.SyncthingURL)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	log.Info("apagando")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// paths acumula --path repetidos.
type paths []string

func (p *paths) String() string     { return fmt.Sprint(len(*p), " rutas") } // sin las rutas
func (p *paths) Set(v string) error { *p = append(*p, v); return nil }

// runPrepare implementa "capataz prepare". La clave solo sale por stdout,
// una vez, y solo con --print-key; nunca por la bitácora (stderr).
func runPrepare(ctx context.Context, log *slog.Logger, args []string, getenv func(string) string, stdout io.Writer) error {
	fs := flag.NewFlagSet("capataz prepare", flag.ContinueOnError)
	envFile := fs.String("env-file", "", "archivo .env a cargar (solo desarrollo)")
	folder := fs.String("folder", "", "id de la carpeta de Syncthing (obligatorio)")
	var ps paths
	fs.Var(&ps, "path", "ruta dentro de la carpeta; repetible (obligatorio)")
	out := fs.String("out", "", "directorio de salida, fuera de las carpetas sincronizadas (obligatorio)")
	label := fs.String("label", "", "etiqueta opcional del enlace")
	printKey := fs.Bool("print-key", false, "imprime la clave en stdout (solo pruebas)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "uso: capataz prepare --folder ID --path RUTA [--path RUTA...] --out DIR [--label TEXTO] [--print-key] [-env-file .env]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *folder == "" || len(ps) == 0 || *out == "" || fs.NArg() > 0 {
		fs.Usage()
		return flag.ErrHelp
	}

	cfg, err := loadConfig(*envFile, getenv)
	if err != nil {
		return err
	}
	st, err := syncthing.New(cfg.SyncthingURL, cfg.SyncthingAPIKey)
	if err != nil {
		return err
	}
	stager, err := stage.New(stage.Options{
		Syncthing: st, StagingDir: cfg.StagingDir, MaxPayload: cpk.MaxPayload, Logger: log,
	})
	if err != nil {
		return err
	}
	items := make([]stage.Item, len(ps))
	for i, p := range ps {
		items[i] = stage.Item{Folder: *folder, Path: p}
	}

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	res, err := prepare.Run(ctx, prepare.Options{
		Stager: stager, Items: items, OutDir: *out, Label: strings.TrimSpace(*label), Logger: log,
	})
	if err != nil {
		return err
	}
	m := res.Manifest
	sk := m.Skipped
	log.Info("listo", "linkId", m.LinkID, "kind", m.Kind, "payloadBytes", m.PayloadSize,
		"packageBytes", m.PackageSize, "omitidos", fmt.Sprintf("ignored=%d hidden=%d symlink=%d special=%d", sk.Ignored, sk.Hidden, sk.Symlink, sk.Special))
	if *printKey {
		fmt.Fprintln(stdout, res.Key.Base64URL())
	}
	return nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
