// Comando capataz: servicio de la API de Capataz junto a Syncthing.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/resuelveonline/capataz/capataz/internal/api"
	"github.com/resuelveonline/capataz/capataz/internal/auth"
	"github.com/resuelveonline/capataz/capataz/internal/config"
	"github.com/resuelveonline/capataz/capataz/internal/state"
	"github.com/resuelveonline/capataz/capataz/internal/syncthing"
)

// version se inyecta al compilar: -ldflags "-X main.version=...".
var version = "dev"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("capataz no pudo arrancar", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	envFile := flag.String("env-file", "", "archivo .env a cargar (solo desarrollo)")
	flag.Parse()

	if *envFile != "" {
		if err := config.LoadDotEnv(*envFile); err != nil {
			return fmt.Errorf("cargando %s: %w", *envFile, err)
		}
	}
	cfg, err := config.FromEnv(os.Getenv)
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

	handler := api.New(api.Options{
		Auth:      auth.NewDevToken(cfg.DevToken),
		Syncthing: st,
		Instance: api.Instance{
			InstanceID: inst.InstanceID,
			Name:       cfg.InstanceName,
			Version:    version,
			PublicMode: "local",
		},
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
