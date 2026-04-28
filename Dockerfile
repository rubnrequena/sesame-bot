# ─── Stage 1: Build ───────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Dependencias del módulo primero (mejor cache de capas)
COPY go.mod go.sum* ./
RUN go mod download

# Código fuente y recursos embebidos
COPY *.go ./
COPY internal/ ./internal/
COPY migrations/ ./migrations/
COPY templates/ ./templates/

# Compilar binario estático
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o sesame-bot .

# ─── Stage 2: Runtime ─────────────────────────────────────────────────────────
# Imagen oficial de Rod: incluye Chromium + todas las dependencias
# necesarias para correr el navegador en modo headless dentro de un contenedor
FROM ghcr.io/go-rod/rod:latest

WORKDIR /app

# Instalar datos de zona horaria y configurar España (Europa/Madrid)
RUN apt-get update && apt-get install -y --no-install-recommends tzdata \
    && ln -sf /usr/share/zoneinfo/Europe/Madrid /etc/localtime \
    && echo "Europe/Madrid" > /etc/timezone \
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Copiar solo el binario compilado
COPY --from=builder /app/sesame-bot .

# El .env se monta en runtime (ver docker-compose o --env-file)
# No se copia aquí para no exponer credenciales en la imagen

# Copiar también migraciones (aunque estén embebidas, las buscamos en disco)
COPY --from=builder /app/migrations ./migrations

# Forzar headless dentro del contenedor (no hay display disponible)
ENV HEADLESS=true
ENV TZ=Europe/Madrid

# DATABASE_URL y ENCRYPTION_KEY se pasan en runtime (no se incrustan en la imagen)

CMD ["./sesame-bot"]
