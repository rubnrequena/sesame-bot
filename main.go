package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"github.com/joho/godotenv"

	"sesame-bot/internal/crypto"
	appdb "sesame-bot/internal/db"
	"sesame-bot/internal/scheduler"
	"sesame-bot/internal/ws"
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

// version is overridden at build time via -ldflags "-X main.version=vX.Y.Z"
// (Dockerfile reads it from ./VERSION). "dev" for local builds.
var version = "dev"

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
	dryRun         bool
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
	log.Printf("Sesame Bot %s arrancando", version)

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

	wsClient := &ws.Whatsapp{
		Token:      os.Getenv("EVOLUTION_TOKEN"),
		Instance:   os.Getenv("EVOLUTION_INSTANCE"),
		Name:       os.Getenv("EVOLUTION_NAME"),
		RetryDelay: 5,
		MaxRetries: 3,
	}

	sched := scheduler.New(pool, encKey, runActionBridge, wsClient)

	go startWebServer(pool, sched, wsClient)

	sched.Run(ctx)
}

// runActionBridge adapts the scheduler's call to the concrete runAction function.
// headless mode is controlled by the HEADLESS env var (default: true).
func runActionBridge(
	userID, email, password string,
	officeDays string,
	offLat, offLon, homeLat, homeLon float64,
	action string,
	_ time.Time,
) (string, error) {
	cfg := config{
		userID:         userID,
		email:          email,
		password:       password,
		headless:       os.Getenv("HEADLESS") != "false",
		dryRun:         os.Getenv("DRY_RUN") == "true",
		locationOffice: location{lat: offLat, lon: offLon},
		locationHome:   location{lat: homeLat, lon: homeLon},
		officeDays:     parseOfficeDays(officeDays),
	}
	return runAction(cfg, actionType(action))
}

// ─── Location helper ──────────────────────────────────────────────────────────

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

func parseTime(raw string) error {
	var h, m int
	if _, err := fmt.Sscanf(raw, "%d:%d", &h, &m); err != nil {
		return fmt.Errorf("formato esperado HH:MM")
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return fmt.Errorf("hora fuera de rango")
	}
	return nil
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

func runAction(cfg config, action actionType) (string, error) {
	logger := log.New(log.Writer(), fmt.Sprintf("[%s] ", cfg.userID), log.LstdFlags)

	// Determine location label for the log detail field.
	day := time.Now().Weekday()
	locLabel := "Casa"
	if cfg.officeDays[day] {
		locLabel = "Oficina"
	}

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

	loc := getLocationForDay(cfg, day)

	// Concede el permiso de geolocalización SIEMPRE: cada ejecución arranca un
	// Chrome nuevo sin preferencias, y sin el permiso concedido la SPA de
	// Sesame no muestra el botón de fichar, aunque la posición esté simulada.
	// El origen debe ser scheme://host sin path (formato que exige CDP).
	permCmd := proto.BrowserGrantPermissions{
		Permissions: []proto.BrowserPermissionType{
			proto.BrowserPermissionTypeGeolocation,
		},
		Origin: originOf(loginURL),
	}
	if err := permCmd.Call(browser); err != nil {
		return "", fmt.Errorf("conceder permiso de geolocalización: %w", err)
	}

	if loc.lat != 0 || loc.lon != 0 {
		accuracy := 10.0
		geoCmd := proto.EmulationSetGeolocationOverride{
			Latitude:  &loc.lat,
			Longitude: &loc.lon,
			Accuracy:  &accuracy,
		}
		if err := geoCmd.Call(page); err != nil {
			return "", fmt.Errorf("establecer geolocalización: %w", err)
		}
		logger.Printf("Geolocalización aplicada: %.6f, %.6f", loc.lat, loc.lon)
	} else {
		logger.Println("Sin coordenadas configuradas — se concede permiso GPS y se usa la ubicación por defecto del navegador")
	}

	logger.Println("Navegando al login...")
	if err := page.Navigate(loginURL); err != nil {
		return "", fmt.Errorf("navegar a login: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return "", fmt.Errorf("esperar carga login: %w", err)
	}
	if err := doLogin(page, cfg.email, cfg.password, logger); err != nil {
		debugDumpUI(page, logger)
		return "", fmt.Errorf("login: %w", err)
	}
	logger.Println("Login exitoso")

	// El deadline global de la página pudo agotarse durante el login; la fase de
	// fichaje necesita presupuesto propio o el botón de fichar fallará con
	// "context deadline exceeded".
	page = renewPage(page)

	holidays := waitHolidays()
	today := time.Now().Format("2006-01-02")
	if len(holidays) == 0 {
		logger.Println("holidays: sin datos de días libres — se procede con el fichaje")
	}
	for _, h := range holidays {
		if h.Date == today {
			logger.Printf("Día festivo detectado: %s (%s) — omitiendo fichaje", today, h.Name)
			return "", fmt.Errorf("%s: %w", h.Name, scheduler.ErrSkipped)
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
	btn, via, err := waitForClickByText(page, buttonText, pageTimeout)
	if err != nil {
		debugDumpUI(page, logger)
		return "", fmt.Errorf("buscar botón %q: %w", buttonText, err)
	}
	logger.Printf("Botón %q localizado (%s)", buttonText, via)
	if err := btn.WaitVisible(); err != nil {
		return "", fmt.Errorf("esperar visibilidad de botón: %w", err)
	}

	if cfg.dryRun {
		logger.Printf("[SIMULACRO] Botón %q localizado — click omitido (DRY_RUN=true)", buttonText)
		return locLabel, nil
	}

	if err := clickElement(page, btn); err != nil {
		return "", fmt.Errorf("click en botón %q: %w", buttonText, err)
	}
	logger.Printf("Click en %q realizado. Esperando 5 segundos...", buttonText)

	time.Sleep(5 * time.Second)

	logger.Println("Cerrando sesión...")
	if err := doLogout(page, logger); err != nil {
		return "", fmt.Errorf("logout: %w", err)
	}

	return locLabel, nil
}

func doLogin(page *rod.Page, email, password string, logger *log.Logger) error {
	// El deadline de la página pudo consumirse en Navigate/WaitLoad. El login
	// necesita su propio presupuesto o el click muere con
	// "context deadline exceeded" aunque el botón ya esté en el DOM.
	page = renewPage(page)

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
	// Los ids exactos son la vía principal; las heurísticas por id o submit se
	// verifican con el texto visible antes de aceptarlas.
	nextSelectors := []string{`#btn-next-login`}
	nextHeuristic := []string{
		`button[id$="next-login"]`,
		`button[type="submit"]`,
	}
	nextTexts := []string{"Siguiente", "Continuar", "Next"}

	loginSelectors := []string{`#btn-login-login`}
	loginHeuristic := []string{
		`button[id$="login-login"]`,
		`button[type="submit"]`,
	}
	loginTexts := []string{"Iniciar sesión", "Entrar", "Log in", "Sign in"}

	emailInput, err := findFirst(page, emailSelectors)
	if err != nil {
		return fmt.Errorf("campo email no encontrado: %w", err)
	}
	if err := emailInput.Input(email); err != nil {
		return fmt.Errorf("escribir email: %w", err)
	}

	logger.Println("Buscando botón siguiente del login...")
	nextBtn, via, err := waitForButton(page, nextSelectors, nextHeuristic, nextTexts, actionTimeout)
	if err != nil {
		return fmt.Errorf("botón siguiente del login no encontrado: %w", err)
	}
	logger.Printf("Botón siguiente localizado (%s)", via)
	if err := clickElement(page, nextBtn); err != nil {
		return fmt.Errorf("click en botón siguiente del login: %w", err)
	}

	passwordInput, err := findFirst(page, passwordSelectors)
	if err != nil {
		return fmt.Errorf("campo password no encontrado: %w", err)
	}
	if err := passwordInput.Input(password); err != nil {
		return fmt.Errorf("escribir password: %w", err)
	}

	logger.Println("Buscando botón de login...")
	loginBtn, via, err := waitForButton(page, loginSelectors, loginHeuristic, loginTexts, actionTimeout)
	if err != nil {
		return fmt.Errorf("botón de login no encontrado: %w", err)
	}
	logger.Printf("Botón de login localizado (%s)", via)
	if err := clickElement(page, loginBtn); err != nil {
		return fmt.Errorf("click en botón de login: %w", err)
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
	// La página tiene un deadline de por vida (pageTimeout) que puede agotarse
	// justo durante el logout (es el último paso). Se des acota para que la
	// búsqueda no muera por ese límite.
	page = page.Context(context.Background())

	// 1) Abrir el menú de usuario probando varios selectores (la UI de Sesame
	// cambia con frecuencia; .headerProfileName ya no existe).
	profileSelectors := []string{
		`.headerProfileName`,
		`[class*="ProfileName"]`,
		`[class*="profile"]`,
		`[class*="avatar"]`,
		`header button`,
	}
	for _, sel := range profileSelectors {
		el, err := page.Timeout(2 * time.Second).Element(sel)
		if err != nil || el == nil {
			continue
		}
		if err := el.Click(proto.InputMouseButtonLeft, 1); err == nil {
			break
		}
	}

	// 2) Buscar el botón de logout: primero por selectores directos y luego por
	// texto ("Cerrar sesión" / "Log out"), reintentando hasta el deadline.
	logoutSelectors := []string{
		`#click-admin-header-logout`,
		`[class*="logout"]`,
		`[data-test*="logout"]`,
		`[href*="logout"]`,
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, sel := range logoutSelectors {
			el, err := page.Timeout(1 * time.Second).Element(sel)
			if err != nil || el == nil {
				continue
			}
			if err := clickElement(page, el); err == nil {
				logger.Println("Sesión cerrada")
				return nil
			}
		}
		for _, txt := range []string{"cerrar sesión", "cerrar sesion", "log out"} {
			el, _, err := waitForClickByText(page, txt, 2*time.Second)
			if err != nil || el == nil {
				continue
			}
			if err := clickElement(page, el); err == nil {
				logger.Println("Sesión cerrada")
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}

	debugDumpUI(page, logger)
	return fmt.Errorf("no se encontró el botón de cerrar sesión")
}

// originOf reduce una URL a su origen (scheme://host[:port]), formato que
// exige CDP en Browser.grantPermissions (una URL con path no es un origen).
func originOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return rawURL
	}
	return u.Scheme + "://" + u.Host
}

// debugDumpUI vuelca textos visibles y una captura de pantalla para diagnosticar
// por qué no se encuentra el botón de fichar (overlays, onboarding, shadow DOM,
// iframes, cambios de texto en la UI de Sesame). Se ejecuta solo cuando la
// búsqueda del botón ha fallado.
func debugDumpUI(page *rod.Page, logger *log.Logger) {
	logger.Println("DEBUG: volcando estado de la página para diagnosticar...")
	// Contexto propio: la página tiene un deadline de por vida (pageTimeout) que
	// ya puede estar agotado cuando llegamos aquí.
	dbgCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dbgPage := page.Context(dbgCtx)
	res, err := dbgPage.Eval(`() => {
		const dedupe = new Set()
		const texts = []
		for (const el of document.querySelectorAll('button,[role=button],a,span,div,input,p')) {
			const raw = (el.innerText || el.textContent || el.value || '')
			const t = raw.trim().replace(/\s+/g, ' ')
			if (!t || t.length > 90 || dedupe.has(t)) continue
			// Sólo hojas o elementos cortos: evita volcar contenedores enormes
			if (el.children.length > 3) continue
			if (t.length < 3) continue
			if (t.toLowerCase().includes('sesame') && t.length > 40) continue
			dedupe.add(t)
			texts.push(t)
			if (texts.length >= 100) break
		}
		let shadowHosts = 0
		for (const el of document.querySelectorAll('*')) {
			if (el.shadowRoot) shadowHosts++
		}
		return {
			url: location.href,
			title: document.title,
			iframes: document.querySelectorAll('iframe').length,
			shadowHosts: shadowHosts,
			texts: texts,
		}
	}`)
	if err != nil {
		logger.Printf("DEBUG: error evaluando JS: %v", err)
	} else {
		v := res.Value
		logger.Printf("DEBUG: url=%s title=%q iframes=%d shadowHosts=%d",
			v.Get("url").Str(), v.Get("title").Str(), v.Get("iframes").Int(), v.Get("shadowHosts").Int())
		for _, t := range v.Get("texts").Arr() {
			logger.Printf("DEBUG texto: %q", t.Str())
		}
	}
	if shot, err := dbgPage.Screenshot(true, nil); err == nil {
		if werr := os.WriteFile("/tmp/sesame-debug.png", shot, 0o644); werr == nil {
			logger.Println("DEBUG: captura guardada en /tmp/sesame-debug.png")
		}
	}
}

// renewPage devuelve un clon de la página con un presupuesto de tiempo nuevo.
//
// La página nace con pageTimeout de vida completa y ese deadline arranca antes
// de Navigate: si la SPA tarda en cargar, las fases siguientes fallan con
// "context deadline exceeded" aunque el elemento exista. Se renueva por fase en
// lugar de acumular un único presupuesto global.
//
// Precondición: page debe venir de (*rod.Page).Timeout.
func renewPage(page *rod.Page) *rod.Page {
	return page.CancelTimeout().Timeout(pageTimeout)
}

// isClickable informa si el elemento tiene caja visible y acepta eventos de
// puntero. Los <span> internos de Sesame suelen tener pointer-events:none y el
// click nativo de rod falla con "element's pointer-events is none".
func isClickable(el *rod.Element) bool {
	res, err := el.Eval(`() => {
		const rect = this.getBoundingClientRect()
		if (rect.width === 0 && rect.height === 0) return false
		const style = getComputedStyle(this)
		if (style.display === 'none' || style.visibility === 'hidden') return false
		return style.pointerEvents !== 'none'
	}`)
	return err == nil && res.Value.Bool()
}

// elementMatcher filtra candidatos antes de comprobar si aceptan clicks. nil
// significa "cualquier elemento".
type elementMatcher func(*rod.Element) bool

// textMatcher devuelve un matcher que exige que el texto visible del elemento
// contenga alguno de los textos dados, ignorando mayúsculas y espacios extra.
func textMatcher(texts ...string) elementMatcher {
	needles := make([]string, len(texts))
	for i, t := range texts {
		needles[i] = strings.ToLower(t)
	}
	return func(el *rod.Element) bool {
		txt, err := el.Text()
		if err != nil {
			return false
		}
		normalized := strings.ToLower(strings.Join(strings.Fields(txt), " "))
		for _, needle := range needles {
			if strings.Contains(normalized, needle) {
				return true
			}
		}
		return false
	}
}

// jsClickGrace es el tiempo que se sigue buscando un candidato clicable antes de
// rendirse y devolver un match no clicable para intentar el click vía JS. Sin
// esta espera, una etiqueta visible antes que el botón real (p.ej. un span con
// pointer-events:none o el texto de estado del fichaje) provoca un JS click
// sobre el elemento equivocado.
const jsClickGrace = 5 * time.Second

// waitForClickable espera a que algún selector apunte a un elemento que cumpla
// match y acepte clicks reales. Reintenta hasta el deadline porque la SPA monta
// el formulario de forma asíncrona. Si sólo aparecen candidatos no clicables
// durante jsClickGrace, devuelve el primero para que clickElement resuelva vía
// JS: el evento burbujea hasta el contenedor que tiene el listener.
func waitForClickable(page *rod.Page, selectors []string, timeout time.Duration, match elementMatcher) (*rod.Element, string, error) {
	deadline := time.Now().Add(timeout)
	var firstMatch *rod.Element
	var firstSel string
	var firstMatchAt time.Time

	for time.Now().Before(deadline) {
		for _, sel := range selectors {
			els, err := page.Elements(sel)
			if err != nil {
				continue
			}
			for _, el := range els {
				if match != nil && !match(el) {
					continue
				}
				if vis, verr := el.Visible(); verr == nil && !vis {
					continue
				}
				if firstMatch == nil {
					firstMatch, firstSel = el, sel
					firstMatchAt = time.Now()
				}
				if isClickable(el) {
					return el, sel, nil
				}
			}
		}
		// Sólo se rinde tras jsClickGrace (acotado al presupuesto disponible):
		// el candidato no clicable puede ser una etiqueta que precede al botón.
		if firstMatch != nil && time.Since(firstMatchAt) >= min(jsClickGrace, timeout) {
			return firstMatch, firstSel + " (js-click)", nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	if firstMatch != nil {
		return firstMatch, firstSel + " (js-click)", nil
	}
	return nil, "", fmt.Errorf("timeout esperando elemento clicable en %v", selectors)
}

// waitForButton localiza un botón por id exacto, luego por heurísticas de id
// verificadas con el texto visible y, por último, sólo por texto. Sesame HR rota
// ids y clases entre despliegues, así que el login no puede depender de un único
// selector; además las heurísticas exigen texto para no pulsar el botón del paso
// anterior si la SPA deja los dos formularios montados.
func waitForButton(page *rod.Page, exact, heuristic, texts []string, timeout time.Duration) (*rod.Element, string, error) {
	if el, via, err := waitForClickable(page, exact, timeout/3, nil); err == nil {
		return el, via, nil
	}
	if len(heuristic) > 0 {
		if el, via, err := waitForClickable(page, heuristic, timeout/3, textMatcher(texts...)); err == nil {
			return el, via + " + texto", nil
		}
	}

	// El último tercio se reparte entre los textos candidatos.
	deadline := time.Now().Add(timeout / 3)
	for _, text := range texts {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		el, via, err := waitForClickByText(page, text, remaining)
		if err == nil {
			return el, fmt.Sprintf("texto %q (%s)", text, via), nil
		}
	}
	all := append(append([]string{}, exact...), heuristic...)
	return nil, "", fmt.Errorf("botón no localizado (selectores %v, textos %v)", all, texts)
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

// clickElement intenta el click nativo de rod (input real del navegador) y, si
// el elemento no es clicable (p.ej. pointer-events:none), hace fallback a un
// click vía JS. El evento JS burbujea hasta el contenedor con el listener.
func clickElement(page *rod.Page, el *rod.Element) error {
	err := el.Click(proto.InputMouseButtonLeft, 1)
	if err == nil {
		return nil
	}
	log.Printf("click nativo falló (%v), probando click vía JS...", err)
	res, jsErr := el.Eval(`() => { this.click(); return true }`)
	if jsErr == nil && res != nil {
		return nil
	}
	return fmt.Errorf("click nativo: %v — click vía JS: %w", err, jsErr)
}

// waitForClickByText localiza el botón de fichaje por su texto visible ("Entrar"
// o "Salir") y devuelve un elemento sobre el que se puede hacer click.
//
// Sesame HR cambia su interfaz con frecuencia: el texto puede vivir en un
// <button>, en un elemento con [role=button], en la familia de clases
// hr-button-*, o en un <span> interior con pointer-events:none (caso actual,
// donde el click simulado de rod sobre el span falla con
// "element's pointer-events is none"). Por eso se prueban varios selectores y
// se comprueba que el candidato acepte clicks reales (rect no vacío y
// pointer-events distintos de none). Si tras varios intentos sólo hay
// candidatos no clicables, se recurre a el.click() vía JS, que dispara el
// evento igualmente y burbujea hasta el contenedor con el listener.
func waitForClickByText(page *rod.Page, text string, timeout time.Duration) (*rod.Element, string, error) {
	// Ordenados de más a menos probable que acepten el click.
	return waitForClickable(page, []string{
		`button`,
		`[role="button"]`,
		`[class*="hr-button"]`,
		`span`,
	}, timeout, textMatcher(text))
}
