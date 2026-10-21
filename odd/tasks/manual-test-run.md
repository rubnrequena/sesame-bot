# Feature: Ejecución manual de fichaje y dry-run de diagnóstico

## Estado: implementado y verificado (v0.3.4) · reporting de errores (v0.3.5)

### v0.3.5 — el panel nunca se queda sin explicación

Un fichaje manual en producción realizó el fichaje pero falló al cerrar sesión, y el panel
mostró "Error de red al ejecutar el fichaje": el cliente no recibió JSON y el `catch` del
`fetch` descartaba el log del run, que sí estaba en el servidor. Correcciones:

- `scheduler.ErrLogoutFailed`: el fichaje hecho con el logout fallido se reporta como
  **ok con aviso**, no como error. El fallo del cierre de sesión no invalida el registro de
  horas. Afecta al run programado y al bajo demanda (helper `statusFor`).
- `web.go`: `recover()` en los dos handlers → un panic devuelve JSON 500 legible en vez de
  tumbar la conexión (lo que el navegador muestra como "error de red"). Se registra el stack.
- `web.go`: `watchClientGone` distingue "el panel se fue" de "el servidor se cayó" (si el
  servidor muere no queda log; una desconexión sí se registra).
- `web.go`: se registra el error al escribir la respuesta JSON (cliente desconectado).
- `scheduler.tailLogs`: el log devuelto se acota a 300 líneas / 512KB, conservando las más
  recientes.
- Plantillas: el `fetch` lee el cuerpo como texto y parsea el JSON con tolerancia; si no es
  JSON válido muestra el cuerpo y el estado HTTP. El aviso de red dice explícitamente que
  **el fichaje puede haberse registrado igualmente** y enlaza a `/logs`. El toggle con las
  líneas de log se muestra también en estado error.
- `internal/scheduler/status_test.go`: test de regresión del mapeo de estado.

- T1-T5 completadas: migración `006`, propagación de `dryRun`/`source`/`logWriter`,
  `Scheduler.RunForUser`, endpoints `POST /test-run` y `POST /checkin/manual`, UI en
  dashboard/config/logs y sección del README.
- Verificación end-to-end contra la BD local (`.env` → `localhost:5432/test`):
  - `POST /test-run` (admin, sobre sí mismo) → `ok:true`, 12.7s, log con
    `[SIMULACRO] ... click omitido` y botón "Salir" localizado. Sin fichaje real.
  - `POST /test-run` con `user_id` de otro usuario (admin) → ejecuta el flujo de ese
    usuario; sin ser admin → **403** ("Sólo un administrador puede probar otro usuario").
  - `POST /checkin/manual` → ejecuta el flujo real; registrado en `checkin_logs` con
    `source='manual'`.
  - `checkin_logs` muestra `source`: `scheduled` / `manual` / `test`, y `action='TEST'`
    para los dry-run.
- Hallazgo durante la verificación: **si el usuario no tiene coordenadas configuradas,
  la SPA de Sesame no muestra el botón de fichar**. El error ahora lo dice y el log
  avisa ("Sin coordenadas configuradas").
- Desviación aceptada: el dry-run está exento de la guardia de duplicados (una
  simulación no puede duplicar un fichaje real y debe poder ejecutarse aunque hoy ya
  haya fichado).

## Objetivo

Poder ejecutar el flujo del bot a demanda, sin esperar al horario programado:

1. **Dry-run de diagnóstico** (login en modo simulación, sin marcar fichaje).
2. **Fichaje manual real** de entrada o salida, con notificación por WhatsApp.

## Problema

El único disparador del flujo de navegador es el scheduler, que ejecuta cada acción en el
minut exacto configurado. Consecuencias:

- Un fallo de login solo se descubre cuando salta el fichaje programado, y **consume el
  fichaje real** de ese día.
- Diagnosticar exige esperar hasta la siguiente ejecución programada.
- El 20/10/2026 se perdieron dos ejecuciones (15:07 y 17:59) por el mismo bloqueo del
  renderer, sin ninguna forma de reproducir el fallo a demanda.

## Decisiones tomadas

| Decisión | Valor |
|---|---|
| Botón de dry-run | En el **dashboard** |
| Dry-run con usuario específico | **Solo el super admin** (`users.is_admin`, el primer usuario registrado). Un usuario normal solo puede probarse a sí mismo |
| Fichaje manual | En la **sección de configuración de cada usuario** (`/config`): 2 botones, entrada y salida |
| Notificación WhatsApp | **Sí**, en el fichaje manual. El dry-run **no** notifica (es una simulación; avisar sería engañoso) |
| Ubicación del fichaje manual | La ya configurada para el día: oficina si hoy es día de oficina, casa si no. Es el mismo criterio que usa el flujo programado (`getLocationForDay`) |
| Tipo de ejecución | dry-run obligatorio en el endpoint de diagnóstico; fichaje real solo en el endpoint manual |

## Alcance

### A. Dry-run de diagnóstico

- `POST /test-run` (`requireAuth`):
  - Body opcional: `user_id` (destino) y `action` (`IN`/`OUT`, por defecto `IN`).
  - Si `user_id` está vacío → el usuario autenticado.
  - Si `user_id` es otro usuario → **solo admin**; si no, `403`.
  - Ejecuta el flujo completo con **dry-run forzado en el código** (nunca fichaje real).
  - Respuesta JSON: `{ok, action, location, error, duration_ms, logs[]}`.
- Registra en `checkin_logs` con `action='TEST'`, `source='test'`.
- Botón "Probar login (simulación)" en `templates/dashboard.html`; para el admin, un
  `<select>` con los usuarios (por defecto, él mismo).

### B. Fichaje manual

- `POST /checkin/manual` (`requireAuth`):
  - Body: `action` (`IN`/`OUT`) y opcional `force=1`.
  - Ejecuta el flujo **real** (no dry-run) con la ubicación configurada para hoy.
  - Guarda el log con `action` real y `source='manual'`.
  - Envía notificación por WhatsApp (mismo formato que el scheduler).
  - Guarda en `checkin_logs` y devuelve JSON con el resultado.
- 2 botones en `templates/config.html`: "Fichar entrada ahora" y "Fichar salida ahora",
  con confirmación (`confirm()`) antes de dispara.

## Migración de base de datos

`checkin_logs` hoy solo admite `action IN ('IN','OUT')` y no distingue el origen de la
ejecución. Nueva migración `migrations/006_manual_and_source.sql`:

```sql
ALTER TABLE checkin_logs ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'scheduled';
ALTER TABLE checkin_logs DROP CONSTRAINT IF EXISTS checkin_logs_action_check;
ALTER TABLE checkin_logs ADD CONSTRAINT checkin_logs_action_check CHECK (action IN ('IN','OUT','TEST'));
```

Valores de `source`: `scheduled` (programado), `manual` (fichaje manual), `test` (dry-run).

## Tareas

### T1 — Propagar `dryRun` y `source` por llamada

- `scheduler.RunActionFn` (`internal/scheduler/scheduler.go:46`): añadir `dryRun bool`.
- `Scheduler`: campo `dryRun bool` (inicializado con `DRY_RUN` del entorno) para las
  ejecuciones programadas.
- `runActionBridge` (`main.go:128`): firma acorde; usa el `dryRun` recibido en lugar de
  leer el entorno.
- `db.InsertCheckinLog` (`internal/db/logs.go:11`): añadir el parámetro `source`.
- `models.CheckinLog` (`internal/models/models.go`): campo `Source`.
- Las llamadas existentes pasan `source="scheduled"`.

### T2 — `Scheduler.RunAction` (ejecución bajo demanda)

En `internal/scheduler/scheduler.go`, un único punto de entrada para las tres vías
(programada, manual, dry-run):

```go
type RunOptions struct {
    Action   string // "IN" | "OUT"
    DryRun   bool
    Source   string // "scheduled" | "manual" | "test"
    Notify   bool   // enviar WhatsApp
    Force    bool   // permitir duplicado de la misma acción el mismo día
}

type RunResult struct {
    Action   string
    Location string
    Status   string
    Message  string
    Duration time.Duration
    Logs     []string
}

func (s *Scheduler) RunForUser(ctx context.Context, userID string, opts RunOptions) (RunResult, error)
```

- Carga el usuario con `appdb.LoadUserWithConfig` y resuelve la contraseña con
  `resolvePassword` (memoria o ciphertext de la BD).
- Sin credenciales de Sesame → error explícito, no un fallo genérico del navegador.
- Ejecuta `runAction` con el `dryRun` de las opciones.
- Inserta el log con el `source` correspondiente.
- Envía WhatsApp solo si `opts.Notify` es true.
- **Un solo run en curso por usuario**: mutex por userID; si ya hay uno, error
  "ya hay una acción en curso".
- Guarda de duplicados: si la misma acción ya se ejecutó con éxito hoy y `Force` es
  false → error "ya hay una entrada registrada hoy" (el propio estado de Sesame ya
  protege, pero el mensaje es más claro).

### T3 — Handlers web

`web.go`:

- `mux.HandleFunc("/test-run", requireAuth(pool, handleTestRun(pool, sched)))`
  - valida `action` ∈ {IN, OUT} y el `user_id` (admin-only para otro usuario).
  - contexto propio de 120s (el cliente puede desconectarse sin cortar el navegador).
  - responde JSON con el resultado y las líneas de log.
- `mux.HandleFunc("/checkin/manual", requireAuth(pool, handleManualCheckin(pool, sched)))`
  - valida `action` ∈ {IN, OUT}; el usuario es siempre el autenticado.
  - dry-run forzado a false, `Notify=true`, `Source="manual"`.

### T4 — UI

- `templates/dashboard.html`: botón "Probar login (simulación)" + selector de usuario
  (solo admin) + render del resultado.
- `templates/config.html`: 2 botones "Fichar entrada ahora" / "Fichar salida ahora" con
  `confirm()`.
- `templates/logs.html`: mostrar el origen (`scheduled`/`manual`/`test`).

### T5 — Documentación

- `README.md`: sección "Fichaje manual y dry-run de diagnóstico".
- `odd/tasks/manual-test-run.md`: este documento (actualizado con las decisiones).

## Criterios de aceptación

- `POST /test-run` sin `user_id` ejecuta el flujo del usuario autenticado en simulación y
  devuelve `{ok:true}` en 10-30s **sin marcar fichaje**.
- `POST /test-run` con `user_id` de otro usuario devuelve 403 si quien llama no es admin;
  si es admin, ejecuta el dry-run de ese usuario.
- `POST /checkin/manual` con `action=IN` marca la entrada real, usa la ubicación
  configurada para hoy y envía la notificación de WhatsApp.
- Un segundo fichaje manual de la misma acción el mismo día se rechaza con un mensaje
  claro (salvo `force=1`).
- El log muestra el origen de cada ejecución (programado / manual / test).
- `go build ./...` y `go vet ./...` sin errores; `gofmt -l` limpio.

## Comprobaciones

- `go build ./... && go vet ./... && gofmt -l .`
- Migración aplicada sobre la BD local: `psql $DATABASE_URL -c '\d checkin_logs'`.
- Prueba end-to-end contra la BD local (`.env` apunta a `localhost:5432/test`):
  `curl -b cookie -X POST -d 'action=IN' localhost:8080/test-run` → `ok:true` y el log
  muestra `[SIMULACRO]`.
- Prueba del fichaje manual **con `DRY_RUN=true`** para no marcar entrada real durante
  las pruebas: el botón manual debe respetar el dry-run del entorno? → **no**: el
  fichaje manual es real por definición; para probarlo sin efectos se usa el endpoint de
  dry-run.

## Riesgos

| Riesgo | Mitigación |
|---|---|
| El endpoint lanza un Chromium: consumo de CPU | Un run en curso por usuario; el segundo recibe "ya hay una acción en curso" |
| El cliente desconecta a mitad del test | El handler usa contexto propio con timeout, no el de la request |
| Que el dry-run marque un fichaje real | El dry-run se fuerza en el código, no por entorno ni por parámetro |
| Doble fichaje por pulsar dos veces | Guardia de duplicados por día y acción + `confirm()` en el botón |

## Proyección

~350 líneas entre 10 ficheros (migración, modelo, db, scheduler, main, web, 3 plantillas,
README). Dentro del presupuesto de 400 líneas, sin cambios de esquema incompatibles
(solo una columna nueva y una restricción ampliada).
