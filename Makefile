IMAGE  = rubn1987/sesame-bot
TAG    = latest

.PHONY: build push release

## Compila la imagen para linux/amd64 (compatible con servidores Linux desde Mac ARM)
build:
	docker buildx build --platform linux/amd64 -t $(IMAGE):$(TAG) --load .

## Sube la imagen a Docker Hub
push:
	docker buildx build --platform linux/amd64 -t $(IMAGE):$(TAG) --push .

## Build + push en un solo paso
release:
	docker buildx build --platform linux/amd64 -t $(IMAGE):$(TAG) --push .
	@echo "✅ Imagen publicada: $(IMAGE):$(TAG)"
