// Command seeduser crea/actualiza un usuario de prueba en la base de datos con
// credenciales de Sesame cifradas y un day override para el día actual, para
// poder probar el flujo de fichaje (p. ej. con DRY_RUN=true) sin esperar a la
// hora programada real.
//
// Uso:
//
//	go run ./cmd/seeduser -email usuario@ejemplo.com -password 'contraseña' [-in HH:MM] [-out HH:MM]
//
// Las horas por defecto son ahora+2min (IN) y ahora+5min (OUT).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/jackc/pgx/v5"

	appdb "sesame-bot/internal/db"
	"sesame-bot/internal/crypto"
	"sesame-bot/internal/models"
)

func main() {
	email := flag.String("email", "", "Email del usuario de prueba (cuenta web)")
	password := flag.String("password", "", "Contraseña del usuario de prueba (cuenta web)")
	sesameEmail := flag.String("sesame-email", "", "Email de Sesame (por defecto: igual que -email)")
	sesamePassword := flag.String("sesame-password", "", "Contraseña de Sesame (por defecto: igual que -password)")
	inTime := flag.String("in", "", "Hora de entrada HH:MM (por defecto: ahora+2min)")
	outTime := flag.String("out", "", "Hora de salida HH:MM (por defecto: ahora+5min)")
	officeLoc := flag.String("office", "", "Coordenadas de oficina lat,lon (por defecto: LOCATION_OFFICE del .env)")
	homeLoc := flag.String("home", "", "Coordenadas de casa lat,lon (por defecto: LOCATION_HOME del .env)")
	officeDays := flag.String("office-days", "", "Días de oficina, ej: Tuesday,Thursday (por defecto: vacío = siempre casa)")
	flag.Parse()

	if *email == "" || *password == "" {
		log.Fatal("faltan -email y/o -password")
	}
	if *sesameEmail == "" {
		*sesameEmail = *email
	}
	if *sesamePassword == "" {
		*sesamePassword = *password
	}

	ctx := context.Background()

	pool, err := appdb.Connect(ctx)
	if err != nil {
		log.Fatalf("conectando a la BD: %v", err)
	}
	defer pool.Close()

	if err := appdb.RunMigrations(ctx, pool, "migrations"); err != nil {
		log.Fatalf("migraciones: %v", err)
	}

	encKey, err := crypto.LoadKey()
	if err != nil {
		log.Fatalf("cargando ENCRYPTION_KEY: %v", err)
	}
	if len(encKey) == 0 {
		log.Fatal("ENCRYPTION_KEY no definida: no se puede cifrar la contraseña de Sesame")
	}

	// ── Usuario (cuenta web) ────────────────────────────────────────────────
	hash, err := bcrypt.GenerateFromPassword([]byte(*password), 12)
	if err != nil {
		log.Fatalf("hasheando contraseña: %v", err)
	}

	user, err := appdb.GetUserByEmail(ctx, pool, *email)
	created := false
	switch {
	case err == pgx.ErrNoRows:
		user, err = appdb.CreateUser(ctx, pool, *email, string(hash), false, true)
		if err != nil {
			log.Fatalf("creando usuario: %v", err)
		}
		created = true
	case err != nil:
		log.Fatalf("buscando usuario: %v", err)
	default:
		_, err = pool.Exec(ctx,
			`UPDATE users SET password_hash=$1, is_active=TRUE, is_approved=TRUE, updated_at=NOW() WHERE id=$2`,
			string(hash), user.ID)
		if err != nil {
			log.Fatalf("actualizando usuario: %v", err)
		}
	}

	// ── Config de Sesame (credenciales cifradas + geolocalización) ──────────
	off, err := resolveLocation(*officeLoc, "LOCATION_OFFICE")
	if err != nil {
		log.Fatalf("coordenadas oficina: %v", err)
	}
	hom, err := resolveLocation(*homeLoc, "LOCATION_HOME")
	if err != nil {
		log.Fatalf("coordenadas casa: %v", err)
	}
	encPw, err := crypto.Encrypt(encKey, *sesamePassword)
	if err != nil {
		log.Fatalf("cifrando contraseña de Sesame: %v", err)
	}
	cfg := &models.UserConfig{
		UserID:            user.ID,
		SesameEmail:       *sesameEmail,
		SesamePasswordEnc: encPw,
		LocationOfficeLat: off.lat,
		LocationOfficeLon: off.lon,
		LocationHomeLat:   hom.lat,
		LocationHomeLon:   hom.lon,
		OfficeDays:        *officeDays,
		WhatsappNumber:    "", // sin WhatsApp: evita notificaciones durante las pruebas
	}
	if err := appdb.UpsertUserConfig(ctx, pool, cfg); err != nil {
		log.Fatalf("guardando config: %v", err)
	}

	// ── Day override de hoy ─────────────────────────────────────────────────
	inH, inM, err := resolveTime(*inTime, 2*time.Minute)
	if err != nil {
		log.Fatalf("hora -in: %v", err)
	}
	outH, outM, err := resolveTime(*outTime, 5*time.Minute)
	if err != nil {
		log.Fatalf("hora -out: %v", err)
	}
	ov := &models.DayOverride{
		UserID:        user.ID,
		Weekday:       int(time.Now().Weekday()),
		HoursIn:       fmt.Sprintf("%02d:%02d", inH, inM),
		HoursOut:      fmt.Sprintf("%02d:%02d", outH, outM),
		JitterMinutes: 0,
	}
	if err := appdb.UpsertDayOverride(ctx, pool, ov); err != nil {
		log.Fatalf("guardando day override: %v", err)
	}

	action := "creado"
	if !created {
		action = "actualizado"
	}
	fmt.Printf(`
✔ Usuario %s: %s (id=%s)
  Sesame email: %s
  Geolocalización: oficina %.6f,%.6f · casa %.6f,%.6f · días oficina: %q
  Override de HOY (weekday=%d): IN %s · OUT %s · jitter 0
  DRY_RUN=%s → el click se omitirá si está a "true"
`, action, user.Email, user.ID, *sesameEmail,
		off.lat, off.lon, hom.lat, hom.lon, *officeDays,
		ov.Weekday, ov.HoursIn, ov.HoursOut, envOr("DRY_RUN", "false"))
}

func resolveTime(raw string, offset time.Duration) (int, int, error) {
	if raw != "" {
		var h, m int
		if _, err := fmt.Sscanf(raw, "%02d:%02d", &h, &m); err != nil {
			return 0, 0, fmt.Errorf("formato esperado HH:MM: %w", err)
		}
		if h < 0 || h > 23 || m < 0 || m > 59 {
			return 0, 0, fmt.Errorf("hora fuera de rango: %s", raw)
		}
		return h, m, nil
	}
	t := time.Now().Add(offset)
	h, m := t.Hour(), t.Minute()
	if h > 23 {
		h, m = 23, 58
	}
	return h, m, nil
}

type coords struct {
	lat, lon float64
}

// resolveLocation acepta "lat,lon" o, si está vacío, toma la variable de entorno
// indicada. Vacío en ambos = coordenadas 0 (sin override de GPS).
func resolveLocation(raw, envKey string) (coords, error) {
	if raw == "" {
		raw = os.Getenv(envKey)
	}
	if raw == "" {
		return coords{}, nil
	}
	parts := strings.SplitN(raw, ",", 2)
	if len(parts) != 2 {
		return coords{}, fmt.Errorf("formato esperado lat,lon, obtenido %q", raw)
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return coords{}, fmt.Errorf("latitud inválida en %q: %w", raw, err)
	}
	lon, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return coords{}, fmt.Errorf("longitud inválida en %q: %w", raw, err)
	}
	return coords{lat: lat, lon: lon}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
