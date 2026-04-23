# 🕰️ Sesame Time Bot

Bot que automatiza el fichaje de entrada y salida en la aplicación Sesame Time. Funciona como un scheduler en segundo plano que comprueba la hora actual contra los horarios configurados y ejecuta las acciones necesarias mediante automatización de navegador.

Incluye una **interfaz web de administración** para modificar la configuración en caliente, sin necesidad de reiniciar el servicio.

## 🚀 Funcionalidades

- **Scheduling automático:** Ejecuta acciones únicamente a las horas configuradas (`HH:MM`).
- **Horarios flexibles:** Soporta horario genérico semanal y overrides por día específico.
- **Control de fin de semana:** Configurable para ejecutar solo en días laborables.
- **Geolocalización:** Simula ubicación GPS diferente según si el día es de oficina o teletrabajo.
- **UI de administración:** Formulario web protegido por contraseña para editar la configuración sin tocar el `.env`.

## ⚙️ Configuración

Copia `.env.example` como `.env` y rellena los valores:

```bash
cp .env.example .env
```

### Variables requeridas

| Variable | Descripción | Ejemplo |
| :--- | :--- | :--- |
| `SESAME_EMAIL` | Email de acceso a Sesame Time | `user@empresa.com` |
| `SESAME_PASSWORD` | Contraseña de Sesame Time | `tu_contraseña` |
| `HOURS_IN` | Horas de entrada separadas por coma (`HH:MM`) | `09:00` o `09:00,14:00` |
| `HOURS_OUT` | Horas de salida separadas por coma (`HH:MM`) | `18:00` o `13:00,18:00` |

### Variables opcionales — comportamiento

| Variable | Descripción | Por defecto |
| :--- | :--- | :--- |
| `HEADLESS` | `false` para ver el navegador (debug) | `true` |
| `WEEKEND` | `true` para ejecutar también en sábado y domingo | `false` |
| `MONDAY_IN`, `FRIDAY_OUT`, etc. | Override de horario para un día concreto. Formato: `DAYNAME_IN` / `DAYNAME_OUT`. Días disponibles: `SUNDAY`, `MONDAY`, `TUESDAY`, `WEDNESDAY`, `THURSDAY`, `FRIDAY`, `SATURDAY` | — |

### Variables opcionales — geolocalización

Permiten simular la ubicación GPS del dispositivo al fichar, útil si Sesame Time registra la posición.

| Variable | Descripción | Ejemplo |
| :--- | :--- | :--- |
| `LOCATION_OFFICE` | Coordenadas de la oficina (`latitud,longitud`) | `40.4162,-3.7038` |
| `LOCATION_HOME` | Coordenadas de casa (`latitud,longitud`) | `40.1234,-3.4567` |
| `OFFICE_DAYS` | Días en que se aplica `LOCATION_OFFICE`. El resto usa `LOCATION_HOME`. Separados por coma. | `Tuesday,Thursday` |

### Variables opcionales — UI de administración

| Variable | Descripción | Por defecto |
| :--- | :--- | :--- |
| `ADMIN_PORT` | Puerto del servidor web de administración | `8080` |
| `ADMIN_PASSWORD` | Contraseña para acceder a la UI web | — (requerida para usar la UI) |

## 🖥️ Interfaz web de administración

El bot expone un servidor HTTP que permite modificar la configuración sin editar el `.env` ni reiniciar el servicio.

### Acceso

Con el bot en ejecución, abre en el navegador:

```
http://localhost:8080/login
```

Introduce el valor de `ADMIN_PASSWORD` para acceder.

### Qué se puede configurar

| Campo | Variable |
| :--- | :--- |
| Horas de entrada | `HOURS_IN` |
| Horas de salida | `HOURS_OUT` |
| Ejecutar en fin de semana | `WEEKEND` |
| Coordenadas de oficina | `LOCATION_OFFICE` |
| Coordenadas de casa | `LOCATION_HOME` |
| Días de oficina | `OFFICE_DAYS` |

Al guardar, los cambios se aplican **inmediatamente** al scheduler en memoria y se persisten en el archivo `.env`, por lo que sobreviven a un reinicio del servicio.

> **Nota Docker:** Si usas `--env-file` sin montar el `.env` como volumen, los cambios se aplican en memoria pero no persisten al reiniciar el contenedor. Monta el archivo para persistencia:
> ```bash
> docker run -v $(pwd)/.env:/app/.env --env-file .env ...
> ```

## 🐳 Docker

```bash
# Construir imagen
make build

# Ejecutar
docker run -v $(pwd)/.env:/app/.env --env-file .env -p 8080:8080 rubn1987/sesame-bot:latest
```

## 🛠️ Cómo funciona

1. **Arranque:** Lee y parsea credenciales y horarios desde el entorno.
2. **Scheduler:** Bucle infinito que comprueba la hora cada 30 segundos.
3. **Comprobación:** Compara hora actual con el horario del día (aplicando overrides si existen).
4. **Ejecución:** Al coincidir la hora, lanza `runAction`:
   - Abre Chrome (headless por defecto)
   - Aplica geolocalización GPS si está configurada
   - Navega a `https://app.sesametime.com/login` y hace login
   - Pulsa `Entrar` o `Salir` según corresponda
   - Espera 5 segundos y cierra sesión

## 🐛 Troubleshooting

1. **Credenciales:** Verifica `SESAME_EMAIL` y `SESAME_PASSWORD` en el `.env`.
2. **Selectores CSS:** Si Sesame Time actualiza su interfaz, los selectores en `main.go` (`#btn-next-login`, `.headerProfileName`, etc.) pueden necesitar actualización.
3. **Horario no ejecuta:** Asegúrate de que `HOURS_IN` y `HOURS_OUT` están definidos. El bot comprueba cada 30 segundos, por lo que puede tardar hasta 30 s en reaccionar.
4. **UI no accesible:** Verifica que `ADMIN_PASSWORD` está definido y que el puerto `ADMIN_PORT` no está bloqueado por el firewall.
