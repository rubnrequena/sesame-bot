package main

import (
	"fmt"
	"log"
	"os"
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

// ─── Config ──────────────────────────────────────────────────────────────────

type config struct {
	email    string
	password string
	headless bool
	schedule []scheduledTime
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

	var schedule []scheduledTime

	for _, raw := range splitTimes(os.Getenv("HOURS_IN")) {
		st, err := parseTime(raw, actionIn)
		if err != nil {
			log.Fatalf("HOURS_IN tiene un valor inválido '%s': %v", raw, err)
		}
		schedule = append(schedule, st)
	}

	for _, raw := range splitTimes(os.Getenv("HOURS_OUT")) {
		st, err := parseTime(raw, actionOut)
		if err != nil {
			log.Fatalf("HOURS_OUT tiene un valor inválido '%s': %v", raw, err)
		}
		schedule = append(schedule, st)
	}

	if len(schedule) == 0 {
		log.Fatal("Debes configurar al menos una hora en HOURS_IN o HOURS_OUT")
	}

	return config{
		email:    email,
		password: password,
		headless: headless,
		schedule: schedule,
	}
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
	log.Printf("Modo headless: %v", cfg.headless)
	log.Println("Horario configurado:")
	for _, st := range cfg.schedule {
		log.Printf("  %02d:%02d → %s", st.hour, st.minute, st.action)
	}

	// Conjunto de acciones ya ejecutadas en la jornada actual (evita doble ejecución)
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
		}

		for _, st := range cfg.schedule {
			key := fmt.Sprintf("%s-%02d:%02d", today, st.hour, st.minute)
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
	// Abrir browser
	u := launcher.New().
		Headless(cfg.headless).
		Set("disable-blink-features", "AutomationControlled").
		Set("no-sandbox", "").
		MustLaunch()

	browser := rod.New().ControlURL(u).MustConnect()
	defer browser.MustClose()

	page := browser.MustPage("").Timeout(pageTimeout)

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

	// 4. Esperar 5 segundos
	time.Sleep(5 * time.Second)

	// 5. Cerrar sesión
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
	// Abrir menú de perfil
	profileBtn, err := page.Timeout(actionTimeout).Element(".headerProfileName")
	if err != nil {
		return fmt.Errorf("botón .headerProfileName no encontrado: %w", err)
	}
	if err := profileBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click en .headerProfileName: %w", err)
	}

	// Hacer click en logout
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
