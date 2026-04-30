package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

	appdb "sesame-bot/internal/db"
	"sesame-bot/internal/crypto"
	"sesame-bot/internal/models"
	"sesame-bot/internal/scheduler"
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

type scheduledTime struct {
	hour   int
	minute int
	action actionType
}

type daySchedule struct {
	in  []string
	out []string
}

type location struct {
	lat float64
	lon float64
}

var dayNames = map[time.Weekday]string{
	time.Sunday:    "SUNDAY",
	time.Monday:    "MONDAY",
	time.Tuesday:   "TUESDAY",
	time.Wednesday: "WEDNESDAY",
	time.Thursday:  "THURSDAY",
	time.Friday:    "FRIDAY",
	time.Saturday:  "SATURDAY",
}

type config struct {
	userID         string
	email          string
	password       string
	headless       bool
	weekend        bool
	dryRun         bool
	hoursIn        []string
	hoursOut       []string
	overrides      map[time.Weekday]daySchedule
	locationOffice location
	locationHome   location
	officeDays     map[time.Weekday]bool
}

type holidayEntry struct {
	Date string `json:"date"` // "YYYY-MM-DD"
	Name string `json:"name"`
}

type holidaysPage struct {
	Data []holidayEntry `json:"data"`
	Meta struct {
		CurrentPage int `json:"currentPage"`
		LastPage    int `json:"lastPage"`
	} `json:"meta"`
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	// Load .env if present (local dev); in Docker vars come via --env-file
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("Aviso cargando .env: %v", err)
	}

	ctx := context.Background()

	pool, err := appdb.Connect(ctx)
	if err != nil {
		log.Fatalf("Error conectando a la base de datos: %v", err)
	}
	defer pool.Close()
	log.Println("Conectado a PostgreSQL")

	if err := appdb.RunMigrations(ctx, pool, "migrations"); err != nil {
		log.Fatalf("Error en migraciones: %v", err)
	}

	encKey, err := crypto.LoadKey()
	if err != nil {
		log.Fatalf("Error cargando ENCRYPTION_KEY: %v", err)
	}

	sched := scheduler.New(pool, encKey, runActionBridge)

	go startWebServer(pool, sched)

	sched.Run(ctx)
}

// runActionBridge adapts the scheduler's generic call to the concrete runAction function.
func runActionBridge(
	userID, email, password string,
	headless, weekend bool,
	hoursIn, hoursOut, officeDays string,
	offLat, offLon, homeLat, homeLon float64,
	overrides []models.DayOverride,
	action string,
	_ time.Time,
) error {
	cfg := config{
		userID:   userID,
		email:    email,
		password: password,
		headless: headless,
		weekend:  weekend,
		dryRun:   os.Getenv("DRY_RUN") == "true",
		hoursIn:  splitTimes(hoursIn),
		hoursOut: splitTimes(hoursOut),
		overrides: buildOverridesFromDB(overrides),
		locationOffice: location{lat: offLat, lon: offLon},
		locationHome:   location{lat: homeLat, lon: homeLon},
		officeDays:     parseOfficeDays(officeDays),
	}
	return runAction(cfg, actionType(action))
}

func buildOverridesFromDB(rows []models.DayOverride) map[time.Weekday]daySchedule {
	out := make(map[time.Weekday]daySchedule)
	for _, o := range rows {
		out[time.Weekday(o.Weekday)] = daySchedule{
			in:  splitTimes(o.HoursIn),
			out: splitTimes(o.HoursOut),
		}
	}
	return out
}

// ─── Schedule helpers ─────────────────────────────────────────────────────────

func getScheduleForDay(cfg config, day time.Weekday) []scheduledTime {
	isWeekend := day == time.Saturday || day == time.Sunday
	if isWeekend && !cfg.weekend {
		return nil
	}

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
		if st, err := parseTime(raw, actionIn); err == nil {
			schedule = append(schedule, st)
		}
	}
	for _, raw := range outTimes {
		if st, err := parseTime(raw, actionOut); err == nil {
			schedule = append(schedule, st)
		}
	}
	return schedule
}

func getLocationForDay(cfg config, day time.Weekday) location {
	if cfg.officeDays[day] {
		return cfg.locationOffice
	}
	return cfg.locationHome
}

// ─── Parsers ──────────────────────────────────────────────────────────────────

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

func parseOfficeDays(raw string) map[time.Weekday]bool {
	result := make(map[time.Weekday]bool)
	if raw == "" {
		return result
	}
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
		}
	}
	return result
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

// ─── Holiday helpers ──────────────────────────────────────────────────────────

// startHolidayCapture enables CDP network tracking on the page BEFORE any navigation
// and listens for the holidays API response that the SPA makes with its own auth session.
// It returns a wait function that must be called after login to collect the results.
func startHolidayCapture(page *rod.Page, logger *log.Logger) func() []holidayEntry {
	type pageResult struct {
		entries  []holidayEntry
		lastPage int
	}
	ch := make(chan pageResult, 10)
	year := strconv.Itoa(time.Now().Year())

	if err := (proto.NetworkEnable{}).Call(page); err != nil {
		logger.Printf("holidays: error habilitando network tracking: %v", err)
		return func() []holidayEntry { return nil }
	}

	wait := page.EachEvent(func(e *proto.NetworkResponseReceived) {
		if !strings.Contains(e.Response.URL, "holidays") || !strings.Contains(e.Response.URL, year) {
			return
		}
		logger.Printf("holidays: respuesta detectada: %s (status %d)", e.Response.URL, e.Response.Status)

		result, err := proto.NetworkGetResponseBody{RequestID: e.RequestID}.Call(page)
		if err != nil {
			return // body not yet buffered by Chrome, will be captured on the next event
		}

		bodyStr := result.Body
		if result.Base64Encoded {
			decoded, decErr := base64.StdEncoding.DecodeString(result.Body)
			if decErr != nil {
				logger.Printf("holidays: error decodificando base64: %v", decErr)
				return
			}
			bodyStr = string(decoded)
		}

		var p holidaysPage
		if err := json.Unmarshal([]byte(bodyStr), &p); err != nil {
			logger.Printf("holidays: error parseando JSON: %v — body: %.200s", err, bodyStr)
			return
		}
		logger.Printf("holidays: página %d/%d, %d entradas", p.Meta.CurrentPage, p.Meta.LastPage, len(p.Data))
		select {
		case ch <- pageResult{p.Data, p.Meta.LastPage}:
		default:
			logger.Printf("holidays: canal lleno, descartando página %d", p.Meta.CurrentPage)
		}
	})
	go wait()

	return func() []holidayEntry {
		select {
		case first := <-ch:
			all := first.entries
			for p := 2; p <= first.lastPage; p++ {
				select {
				case next := <-ch:
					all = append(all, next.entries...)
				case <-time.After(5 * time.Second):
					logger.Printf("holidays: timeout esperando página %d/%d", p, first.lastPage)
				}
			}
			logger.Printf("holidays: total %d días libres cargados", len(all))
			return all
		case <-time.After(8 * time.Second):
			logger.Println("holidays: timeout — no se recibió respuesta del endpoint holidays")
			return nil
		}
	}
}

// ─── Browser automation ───────────────────────────────────────────────────────

func runAction(cfg config, action actionType) error {
	logger := log.New(log.Writer(), fmt.Sprintf("[%s] ", cfg.userID), log.LstdFlags)

	u := launcher.New().
		Headless(cfg.headless).
		Set("disable-blink-features", "AutomationControlled").
		Set("no-sandbox", "").
		MustLaunch()

	browser := rod.New().ControlURL(u).MustConnect()
	defer browser.MustClose()

	page := browser.MustPage("").Timeout(pageTimeout)

	// Register CDP network listener before any navigation so the SPA request is captured.
	waitHolidays := startHolidayCapture(page, logger)

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
		permCmd := proto.BrowserGrantPermissions{
			Permissions: []proto.BrowserPermissionType{
				proto.BrowserPermissionTypeGeolocation,
			},
			Origin: loginURL,
		}
		if err := permCmd.Call(browser); err != nil {
			return fmt.Errorf("conceder permiso de geolocalización: %w", err)
		}
		logger.Printf("Geolocalización aplicada: %.6f, %.6f", loc.lat, loc.lon)
	}

	logger.Println("Navegando al login...")
	if err := page.Navigate(loginURL); err != nil {
		return fmt.Errorf("navegar a login: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return fmt.Errorf("esperar carga login: %w", err)
	}
	if err := doLogin(page, cfg.email, cfg.password, logger); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	logger.Println("Login exitoso")

	holidays := waitHolidays()
	today := time.Now().Format("2006-01-02")
	if len(holidays) == 0 {
		logger.Println("holidays: sin datos de días libres — se procede con el fichaje")
	}
	for _, h := range holidays {
		if h.Date == today {
			logger.Printf("Día festivo detectado: %s (%s) — omitiendo fichaje", today, h.Name)
			return fmt.Errorf("%s: %w", h.Name, scheduler.ErrSkipped)
		}
	}

	var next *holidayEntry
	for i := range holidays {
		if holidays[i].Date > today {
			if next == nil || holidays[i].Date < next.Date {
				next = &holidays[i]
			}
		}
	}
	if next != nil {
		logger.Printf("Próximo día libre: %s (%s)", next.Date, next.Name)
	}

	var buttonText string
	if action == actionIn {
		buttonText = "Entrar"
	} else {
		buttonText = "Salir"
	}

	logger.Printf("Buscando botón %q...", buttonText)
	btn, err := waitForElementByText(page, "span", buttonText)
	if err != nil {
		return fmt.Errorf("buscar botón %q: %w", buttonText, err)
	}
	if err := btn.WaitVisible(); err != nil {
		return fmt.Errorf("esperar visibilidad de botón: %w", err)
	}

	if cfg.dryRun {
		logger.Printf("[SIMULACRO] Botón %q localizado — click omitido (DRY_RUN=true)", buttonText)
		return nil
	}

	if err := btn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click en botón %q: %w", buttonText, err)
	}
	logger.Printf("Click en %q realizado. Esperando 5 segundos...", buttonText)

	time.Sleep(5 * time.Second)

	logger.Println("Cerrando sesión...")
	if err := doLogout(page, logger); err != nil {
		return fmt.Errorf("logout: %w", err)
	}

	return nil
}

func doLogin(page *rod.Page, email, password string, logger *log.Logger) error {
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

	emailInput, err := findFirst(page, emailSelectors)
	if err != nil {
		return fmt.Errorf("campo email no encontrado: %w", err)
	}
	if err := emailInput.Input(email); err != nil {
		return fmt.Errorf("escribir email: %w", err)
	}

	logger.Println("Click en #btn-next-login...")
	nextBtn, err := page.Timeout(actionTimeout).Element("#btn-next-login")
	if err != nil {
		return fmt.Errorf("botón #btn-next-login no encontrado: %w", err)
	}
	if err := nextBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click #btn-next-login: %w", err)
	}

	passwordInput, err := findFirst(page, passwordSelectors)
	if err != nil {
		return fmt.Errorf("campo password no encontrado: %w", err)
	}
	if err := passwordInput.Input(password); err != nil {
		return fmt.Errorf("escribir password: %w", err)
	}

	logger.Println("Click en #btn-login-login...")
	loginBtn, err := page.Timeout(actionTimeout).Element("#btn-login-login")
	if err != nil {
		return fmt.Errorf("botón #btn-login-login no encontrado: %w", err)
	}
	if err := loginBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click #btn-login-login: %w", err)
	}

	logger.Println("Esperando redirección post-login...")
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

func doLogout(page *rod.Page, logger *log.Logger) error {
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

	logger.Println("Sesión cerrada")
	return nil
}

func findFirst(page *rod.Page, selectors []string) (*rod.Element, error) {
	for _, sel := range selectors {
		el, err := page.Timeout(actionTimeout).Element(sel)
		if err == nil && el != nil {
			return el, nil
		}
	}
	return nil, fmt.Errorf("ningún selector encontró un elemento")
}

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
