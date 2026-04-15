package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/joho/godotenv"
)

const (
	loginURL = "https://app.sesametime.com/login"

	pageTimeout   = 30 * time.Second
	actionTimeout = 10 * time.Second
)

type actionType string

const (
	actionIn  actionType = "IN"
	actionOut actionType = "OUT"
)

// scheduledTime guarda una hora HH:MM y el tipo de acción asociada
type scheduledTime struct {
	hour   int
	minute int
	action actionType
}

// daySchedule contiene los horarios de entrada y salida para un día concreto
type daySchedule struct {
	in  []string
	out []string
}

// location contiene coordenadas de geolocalización
type location struct {
	lat float64
	lon float64
}

// dayNames mapea cada día de la semana al prefijo usado en las variables de entorno
var dayNames = map[time.Weekday]string{
	time.Sunday:    "SUNDAY",
	time.Monday:    "MONDAY",
	time.Tuesday:   "TUESDAY",
	time.Wednesday: "WEDNESDAY",
	time.Thursday:  "THURSDAY",
	time.Friday:    "FRIDAY",
	time.Saturday:  "SATURDAY",
}

// ─── Config ──────────────────────────────────────────────────────────────────

type config struct {
	email          string
	password       string
	headless       bool
	weekend        bool                         // si es false, omitir ejecución en sábado y domingo
	hoursIn        []string                     // horario genérico de entrada
	hoursOut       []string                     // horario genérico de salida
	overrides      map[time.Weekday]daySchedule // horarios específicos por día
	locationOffice location                     // coordenadas de la oficina
	locationHome   location                     // coordenadas de casa
	officeDays     map[time.Weekday]bool        // días que se va a la oficina
}

func loadConfig() config {
	// Cargar .env si existe (desarrollo local).
	// En Docker las variables llegan por --env-file, así que no es obligatorio.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Fatal("Error cargando archivo .env: ", err)
	}

	email := os.Getenv("SESAME_EMAIL")
	password := os.Getenv("SESAME_PASSWORD")
	if email == "" || password == "" {
		log.Fatal("SESAME_EMAIL y SESAME_PASSWORD son requeridos en el archivo .env")
	}

	headless := os.Getenv("HEADLESS") != "false"

	// WEEKEND: true por defecto; false = no ejecutar en fin de semana
	weekend := os.Getenv("WEEKEND") != "false"

	hoursIn := splitTimes(os.Getenv("HOURS_IN"))
	hoursOut := splitTimes(os.Getenv("HOURS_OUT"))

	if len(hoursIn) == 0 || len(hoursOut) == 0 {
		log.Fatal("SESAME_EMAIL, SESAME_PASSWORD, HOURS_IN y HOURS_OUT son requeridos. Debes configurar valores en ambos para operar el scheduler.")
	}

	// Leer overrides por día: MONDAY_IN, FRIDAY_OUT, etc.
	overrides := make(map[time.Weekday]daySchedule)
	for weekday, prefix := range dayNames {
		in := splitTimes(os.Getenv(prefix + "_IN"))
		out := splitTimes(os.Getenv(prefix + "_OUT"))
		if len(in) > 0 || len(out) > 0 {
			overrides[weekday] = daySchedule{in: in, out: out}
		}
	}

	// Geolocalización
	locationOffice, err := parseLocation(os.Getenv("LOCATION_OFFICE"))
	if err != nil {
		log.Fatalf("LOCATION_OFFICE inválido: %v", err)
	}
	locationHome, err := parseLocation(os.Getenv("LOCATION_HOME"))
	if err != nil {
		log.Fatalf("LOCATION_HOME inválido: %v", err)
	}

	// Días de oficina: OFFICE_DAYS=Tuesday,Thursday
	officeDays := parseOfficeDays(os.Getenv("OFFICE_DAYS"))

	return config{
		email:          email,
		password:       password,
		headless:       headless,
		weekend:        weekend,
		hoursIn:        hoursIn,
		hoursOut:       hoursOut,
		overrides:      overrides,
		locationOffice: locationOffice,
		locationHome:   locationHome,
		officeDays:     officeDays,
	}
}

// getScheduleForDay devuelve los scheduledTime para el día indicado.
// Aplica el override del día si existe; si no, usa el horario genérico.
// Devuelve nil si es fin de semana y weekend=false.
func getScheduleForDay(cfg config, day time.Weekday) []scheduledTime {
	isWeekend := day == time.Saturday || day == time.Sunday
	if isWeekend && !cfg.weekend {
		return nil
	}

	// Determinar las horas de entrada y salida para este día
	inTimes := cfg.hoursIn
	outTimes := cfg.hoursOut

	if override, ok := cfg.overrides[day]; ok {
		if len(override.in) > 0 {
			inTimes = override.in
		}
		if len(override.out) > 0 {
			outTimes = override.out
		}
	}

	var schedule []scheduledTime

	for _, raw := range inTimes {
		st, err := parseTime(raw, actionIn)
		if err != nil {
			log.Printf("⚠️  Hora IN inválida '%s': %v", raw, err)
			continue
		}
		schedule = append(schedule, st)
	}

	for _, raw := range outTimes {
		st, err := parseTime(raw, actionOut)
		if err != nil {
			log.Printf("⚠️  Hora OUT inválida '%s': %v", raw, err)
			continue
		}
		schedule = append(schedule, st)
	}

	return schedule
}

// parseLocation parsea "latitud,longitud" desde una variable de entorno
func parseLocation(raw string) (location, error) {
	if raw == "" {
		return location{}, nil
	}
	parts := strings.SplitN(raw, ",", 2)
	if len(parts) != 2 {
		return location{}, fmt.Errorf("formato esperado: latitud,longitud (ej: 40.4168,-3.7038)")
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return location{}, fmt.Errorf("latitud inválida: %v", err)
	}
	lon, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return location{}, fmt.Errorf("longitud inválida: %v", err)
	}
	return location{lat: lat, lon: lon}, nil
}

// parseOfficeDays parsea "Tuesday,Thursday" y devuelve un mapa de weekdays
// Acepta coma o = como separador para mayor flexibilidad
func parseOfficeDays(raw string) map[time.Weekday]bool {
	result := make(map[time.Weekday]bool)
	if raw == "" {
		return result
	}
	// Normalizar separadores: reemplazar = por ,
	raw = strings.ReplaceAll(raw, "=", ",")
	nameToWeekday := map[string]time.Weekday{
		"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
		"wednesday": time.Wednesday, "thursday": time.Thursday,
		"friday": time.Friday, "saturday": time.Saturday,
	}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if wd, ok := nameToWeekday[part]; ok {
			result[wd] = true
		} else if part != "" {
			log.Printf("⚠️  Día desconocido en OFFICE_DAYS: '%s'", part)
		}
	}
	return result
}

// getLocationForDay devuelve las coordenadas según si el día es de oficina o de casa
func getLocationForDay(cfg config, day time.Weekday) location {
	if cfg.officeDays[day] {
		return cfg.locationOffice
	}
	return cfg.locationHome
}

func splitTimes(raw string) []string {
	if raw == "" {
		return nil
	}
	var result []string
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s != "" {
			result = append(result, s)
		}
	}
	return result
}

func parseTime(raw string, action actionType) (scheduledTime, error) {
	var h, m int
	if _, err := fmt.Sscanf(raw, "%d:%d", &h, &m); err != nil {
		return scheduledTime{}, fmt.Errorf("formato esperado HH:MM")
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return scheduledTime{}, fmt.Errorf("hora fuera de rango")
	}
	return scheduledTime{hour: h, minute: m, action: action}, nil
}

// ─── Main / Scheduler ─────────────────────────────────────────────────────────

func main() {
	cfg := loadConfig()

	log.Println("Bot de Sesame Time iniciado")
	log.Printf("Modo headless : %v", cfg.headless)
	log.Printf("Ejecutar fines de semana: %v", cfg.weekend)
	log.Printf("Horario genérico IN : %v", cfg.hoursIn)
	log.Printf("Horario genérico OUT: %v", cfg.hoursOut)
	for day, ov := range cfg.overrides {
		log.Printf("Override %s → IN:%v OUT:%v", dayNames[day], ov.in, ov.out)
	}

	executed := map[string]bool{}
	lastDate := ""

	log.Println("Scheduler corriendo. Esperando hora programada...")

	for {
		now := time.Now()
		today := now.Format("2006-01-02")

		// Resetear el registro de ejecuciones al comenzar un nuevo día
		if today != lastDate {
			executed = map[string]bool{}
			lastDate = today
			schedule := getScheduleForDay(cfg, now.Weekday())
			if schedule == nil {
				log.Printf("📅 %s es fin de semana — no se ejecutarán acciones", dayNames[now.Weekday()])
			} else {
				log.Printf("📅 Horario de hoy (%s):", dayNames[now.Weekday()])
				for _, st := range schedule {
					log.Printf("   %02d:%02d → %s", st.hour, st.minute, st.action)
				}
			}
		}

		schedule := getScheduleForDay(cfg, now.Weekday())
		for _, st := range schedule {
			key := fmt.Sprintf("%s-%02d:%02d-%s", today, st.hour, st.minute, st.action)
			if executed[key] {
				continue
			}
			if now.Hour() == st.hour && now.Minute() == st.minute {
				log.Printf("⏰ Ejecutando acción %s a las %02d:%02d", st.action, st.hour, st.minute)
				if err := runAction(cfg, st.action); err != nil {
					log.Printf("❌ Error en acción %s: %v", st.action, err)
				} else {
					log.Printf("✅ Acción %s completada", st.action)
				}
				executed[key] = true
			}
		}

		// Revisar cada 30 segundos para no perder el minuto exacto
		time.Sleep(30 * time.Second)
	}
}

// ─── Acción completa: login → click → esperar → logout ───────────────────────

func runAction(cfg config, action actionType) error {
	u := launcher.New().
		Headless(cfg.headless).
		Set("disable-blink-features", "AutomationControlled").
		Set("no-sandbox", "").
		MustLaunch()

	browser := rod.New().ControlURL(u).MustConnect()
	defer browser.MustClose()

	page := browser.MustPage("").Timeout(pageTimeout)

	// Aplicar geolocalización según el día (oficina o casa)
	loc := getLocationForDay(cfg, time.Now().Weekday())
	if loc.lat != 0 || loc.lon != 0 {
		accuracy := 10.0
		geoCmd := proto.EmulationSetGeolocationOverride{
			Latitude:  &loc.lat,
			Longitude: &loc.lon,
			Accuracy:  &accuracy,
		}
		if err := geoCmd.Call(page); err != nil {
			return fmt.Errorf("establecer geolocalización: %w", err)
		}
		// Conceder permiso de geolocalización automáticamente
		permCmd := proto.BrowserGrantPermissions{
			Permissions: []proto.BrowserPermissionType{
				proto.BrowserPermissionTypeGeolocation,
			},
			Origin: loginURL,
		}
		if err := permCmd.Call(browser); err != nil {
			return fmt.Errorf("conceder permiso de geolocalización: %w", err)
		}
		log.Printf("📍 Geolocalización aplicada: %.6f, %.6f", loc.lat, loc.lon)
	}

	// 1. Login
	log.Println("Navegando al login...")
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navegar a login: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return fmt.Errorf("esperar carga login: %w", err)
	}
	if err := doLogin(page, cfg.email, cfg.password); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	log.Println("Login exitoso")

	// 2. Click según acción
	var buttonText string
	if action == actionIn {
		buttonText = "Entrar"
	} else {
		buttonText = "Salir"
	}

	log.Printf("Buscando botón %q...", buttonText)
	btn, err := waitForElementByText(page, "span", buttonText)
	if err != nil {
		return fmt.Errorf("buscar botón %q: %w", buttonText, err)
	}
	if err := btn.WaitVisible(); err != nil {
		return fmt.Errorf("esperar visibilidad de botón: %w", err)
	}
	if err := btn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click en botón %q: %w", buttonText, err)
	}
	log.Printf("Click en %q realizado. Esperando 5 segundos...", buttonText)

	// 3. Esperar 5 segundos
	time.Sleep(5 * time.Second)

	// 4. Cerrar sesión
	log.Println("Cerrando sesión...")
	if err := doLogout(page); err != nil {
		return fmt.Errorf("logout: %w", err)
	}

	return nil
}

// ─── Login ────────────────────────────────────────────────────────────────────

func doLogin(page *rod.Page, email, password string) error {
	emailSelectors := []string{
		`input[type="email"]`,
		`input[name="email"]`,
		`input[placeholder*="email"]`,
		`input[placeholder*="Email"]`,
		`input[id*="email"]`,
	}
	passwordSelectors := []string{
		`input[type="password"]`,
		`input[name="password"]`,
		`input[id*="password"]`,
	}

	// Campo email
	emailInput, err := findFirst(page, emailSelectors)
	if err != nil {
		return fmt.Errorf("campo email no encontrado: %w", err)
	}
	if err := emailInput.Input(email); err != nil {
		return fmt.Errorf("escribir email: %w", err)
	}

	// Botón siguiente (paso 1: email)
	log.Println("Click en #btn-next-login...")
	nextBtn, err := page.Timeout(actionTimeout).Element("#btn-next-login")
	if err != nil {
		return fmt.Errorf("botón #btn-next-login no encontrado: %w", err)
	}
	if err := nextBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click #btn-next-login: %w", err)
	}

	// Campo contraseña
	passwordInput, err := findFirst(page, passwordSelectors)
	if err != nil {
		return fmt.Errorf("campo password no encontrado: %w", err)
	}
	if err := passwordInput.Input(password); err != nil {
		return fmt.Errorf("escribir password: %w", err)
	}

	// Botón login (paso 2: password)
	log.Println("Click en #btn-login-login...")
	loginBtn, err := page.Timeout(actionTimeout).Element("#btn-login-login")
	if err != nil {
		return fmt.Errorf("botón #btn-login-login no encontrado: %w", err)
	}
	if err := loginBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click #btn-login-login: %w", err)
	}

	// Esperar redirección
	log.Println("Esperando redirección post-login...")
	deadline := time.Now().Add(pageTimeout)
	for time.Now().Before(deadline) {
		info, err := page.Info()
		if err == nil && info.URL != loginURL {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout esperando redirección post-login")
}

// ─── Logout ───────────────────────────────────────────────────────────────────

func doLogout(page *rod.Page) error {
	profileBtn, err := page.Timeout(actionTimeout).Element(".headerProfileName")
	if err != nil {
		return fmt.Errorf("botón .headerProfileName no encontrado: %w", err)
	}
	if err := profileBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click en .headerProfileName: %w", err)
	}

	logoutBtn, err := page.Timeout(actionTimeout).Element("#click-admin-header-logout")
	if err != nil {
		return fmt.Errorf("botón #click-admin-header-logout no encontrado: %w", err)
	}
	if err := logoutBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click en #click-admin-header-logout: %w", err)
	}

	log.Println("Sesión cerrada")
	return nil
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// findFirst prueba una lista de selectores CSS y devuelve el primero que encuentre
func findFirst(page *rod.Page, selectors []string) (*rod.Element, error) {
	for _, sel := range selectors {
		el, err := page.Timeout(actionTimeout).Element(sel)
		if err == nil && el != nil {
			return el, nil
		}
	}
	return nil, fmt.Errorf("ningún selector encontró un elemento")
}

// waitForElementByText espera un elemento por tag y texto visible (regex).
// Útil cuando el test-id o id del elemento puede cambiar entre deploys.
func waitForElementByText(page *rod.Page, tag, text string) (*rod.Element, error) {
	deadline := time.Now().Add(pageTimeout)
	for time.Now().Before(deadline) {
		el, err := page.ElementR(tag, text)
		if err == nil && el != nil {
			return el, nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf("timeout esperando <%s> con texto '%s'", tag, text)
}
