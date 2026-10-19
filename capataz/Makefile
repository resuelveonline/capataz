# Capataz: tareas de desarrollo.
# La versión se inyecta en main.version con -ldflags (git describe o "dev").

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)
ENV_FILE ?= ../.env

.PHONY: run test vet build build-arm64 clean

## run: ejecuta Capataz cargando el .env de desarrollo
run:
	go run -ldflags "-X main.version=$(VERSION)" ./cmd/capataz -env-file $(ENV_FILE)

## test: pruebas con el detector de carreras
test:
	go test -race ./...

## vet: análisis estático de la librería estándar
vet:
	go vet ./...

## build: binario para esta máquina en bin/capataz
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/capataz ./cmd/capataz

## build-arm64: binario para la Raspberry Pi (Linux arm64, sin cgo)
build-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/capataz-linux-arm64 ./cmd/capataz

clean:
	rm -rf bin
