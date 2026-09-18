IMAGE  = rubn1987/sesame-bot
VERSION = $(shell cat VERSION)
TAG    = v$(VERSION)

.PHONY: build push release version

## Compila la imagen para linux/amd64 (compatible con servidores Linux desde Mac ARM)
build:
	docker buildx build --platform linux/amd64 -t $(IMAGE):$(TAG) --load .

## Sube la imagen versionada a Docker Hub
push:
	docker buildx build --platform linux/amd64 -t $(IMAGE):$(TAG) --push .

## Build + push en un solo paso
release:
	docker buildx build --platform linux/amd64 -t $(IMAGE):$(TAG) --push .
	@echo "✅ Imagen publicada: $(IMAGE):$(TAG)"

## Muestra la versión actual
version:
	@echo $(TAG)
