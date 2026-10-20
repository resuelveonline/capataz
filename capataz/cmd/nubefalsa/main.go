// Comando nubefalsa: Capataz Nube falsa en memoria (nubetest) para la
// prueba de humo de la Fase 2b. Solo desarrollo; no se instala en el host.
//
//	nubefalsa --listen 127.0.0.1:18090 --instance ID --key-file ARCHIVO
//
// La clave de instancia se lee de un archivo (32 bytes en base64url), nunca
// de los argumentos. Escucha solo en loopback.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/resuelveonline/capataz/capataz/internal/nube"
	"github.com/resuelveonline/capataz/capataz/internal/nube/nubetest"
)

func main() {
	if err := run(); err != nil {
		slog.Error("nubefalsa terminó con error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:18090", "dirección de loopback")
	instance := flag.String("instance", "", "instanceId de Capataz (obligatorio)")
	keyFile := flag.String("key-file", "", "archivo con la clave de instancia (obligatorio)")
	flag.Parse()
	if *instance == "" || *keyFile == "" {
		flag.Usage()
		return errors.New("faltan --instance o --key-file")
	}
	raw, err := os.ReadFile(*keyFile)
	if err != nil {
		return errors.New("no se pudo leer la clave")
	}
	key, err := nube.ParseKey(string(raw))
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(*listen)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("--listen debe ser una IP de loopback")
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := nubetest.NewUnstarted(*instance, key)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()
	fmt.Fprintln(os.Stderr, "nubefalsa escuchando en", srv.URL)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return nil
}
