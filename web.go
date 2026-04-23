package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─── Config holder (thread-safe) ─────────────────────────────────────────────

type configHolder struct {
	mu  sync.RWMutex
	cfg config
}

func (h *configHolder) get() config {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cfg
}

func (h *configHolder) set(c config) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg = c
}

// ─── Session store ────────────────────────────────────────────────────────────

const (
	cookieName  = "sesame_session"
	sessionTTL  = 24 * time.Hour
	defaultPort = "8080"
)

type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{tokens: make(map[string]time.Time)}
}

func (s *sessionStore) create() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.tokens[token] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return token, nil
}

func (s *sessionStore) validate(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.tokens[token]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(s.tokens, token)
		return false
	}
	return true
}

func (s *sessionStore) revoke(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

// ─── Templates ────────────────────────────────────────────────────────────────

var loginTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html lang="es">
<head>
  <meta charset="UTF-8">
  <title>Sesame Bot — Login</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: #f5f5f7;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      margin: 0;
      padding: 1rem;
      color: #1d1d1f;
    }
    .card {
      background: #fff;
      border-radius: 16px;
      padding: 2.5rem 2rem;
      width: 100%;
      max-width: 380px;
      box-shadow: 0 2px 20px rgba(0,0,0,.07);
    }
    h2 {
      font-size: 1.4rem;
      font-weight: 600;
      margin: 0 0 1.75rem;
      letter-spacing: -.02em;
      color: #1d1d1f;
    }
    label {
      display: block;
      font-size: .8rem;
      font-weight: 500;
      color: #6e6e73;
      text-transform: uppercase;
      letter-spacing: .05em;
      margin-top: 1.25rem;
    }
    .pw-wrap {
      position: relative;
      margin-top: .4rem;
    }
    input[type=password], input[type=text].pw-field {
      width: 100%;
      padding: .65rem 2.8rem .65rem .85rem;
      border: 1.5px solid #e0e0e5;
      border-radius: 10px;
      font-size: 1rem;
      color: #1d1d1f;
      background: #fafafa;
      outline: none;
      transition: border-color .15s, box-shadow .15s;
    }
    input[type=password]:focus, input[type=text].pw-field:focus {
      border-color: #0071e3;
      box-shadow: 0 0 0 3px rgba(0,113,227,.12);
      background: #fff;
    }
    .pw-toggle {
      position: absolute;
      right: .65rem;
      top: 50%;
      transform: translateY(-50%);
      background: none;
      border: none;
      margin: 0;
      padding: .25rem;
      width: auto;
      cursor: pointer;
      color: #aeaeb2;
      display: flex;
      align-items: center;
      transition: color .15s;
    }
    .pw-toggle:hover { color: #1d1d1f; background: none; }
    .pw-toggle:active { transform: translateY(-50%) scale(.9); }
    button[type=submit] {
      width: 100%;
      margin-top: 1.5rem;
      padding: .7rem;
      border: none;
      border-radius: 10px;
      background: #0071e3;
      color: #fff;
      font-size: 1rem;
      font-weight: 500;
      cursor: pointer;
      transition: background .15s, transform .1s;
    }
    button[type=submit]:hover { background: #0077ed; }
    button[type=submit]:active { transform: scale(.98); }
    .error {
      display: flex;
      align-items: center;
      gap: .45rem;
      margin-top: 1rem;
      padding: .6rem .8rem;
      border-radius: 8px;
      background: #fff2f2;
      color: #c0392b;
      font-size: .875rem;
    }
  </style>
</head>
<body>
  <div class="card">
    <h2>Sesame Bot</h2>
    <form method="POST" action="/login">
      <label>Contraseña de administrador</label>
      <div class="pw-wrap">
        <input id="pw" type="password" name="password" class="pw-field" autofocus required>
        <button type="button" class="pw-toggle" onclick="togglePw()" aria-label="Mostrar contraseña">
          <svg id="icon-show" xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
            <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"/><circle cx="12" cy="12" r="3"/>
          </svg>
          <svg id="icon-hide" xmlns="http://www.w3.org/2000/svg" width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="display:none">
            <path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94"/><path d="M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19"/><line x1="1" y1="1" x2="23" y2="23"/>
          </svg>
        </button>
      </div>
      {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
      <button type="submit">Entrar</button>
    </form>
  </div>
  <script>
    function togglePw() {
      var inp = document.getElementById('pw');
      var show = document.getElementById('icon-show');
      var hide = document.getElementById('icon-hide');
      if (inp.type === 'password') {
        inp.type = 'text';
        show.style.display = 'none';
        hide.style.display = '';
      } else {
        inp.type = 'password';
        show.style.display = '';
        hide.style.display = 'none';
      }
    }
  </script>
</body>
</html>`))

type loginData struct {
	Error string
}

type dayOption struct {
	Name     string
	Selected bool
}

type configData struct {
	HoursIn        string
	HoursOut       string
	Weekend        bool
	WeekendFalse   bool
	LocationOffice string
	LocationHome   string
	AllDays        []dayOption
	Success        bool
	Error          string
}

var configTmpl = template.Must(template.New("config").Parse(`<!DOCTYPE html>
<html lang="es">
<head>
  <meta charset="UTF-8">
  <title>Sesame Bot — Configuración</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; }
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
      background: #f5f5f7;
      min-height: 100vh;
      margin: 0;
      padding: 2rem 1rem;
      color: #1d1d1f;
    }
    .card {
      background: #fff;
      border-radius: 18px;
      padding: 2.5rem 2.25rem;
      width: 100%;
      max-width: 560px;
      margin: 0 auto;
      box-shadow: 0 2px 24px rgba(0,0,0,.07);
    }
    h2 {
      font-size: 1.4rem;
      font-weight: 600;
      margin: 0 0 1.5rem;
      letter-spacing: -.02em;
    }
    label {
      display: block;
      font-size: .78rem;
      font-weight: 500;
      color: #6e6e73;
      text-transform: uppercase;
      letter-spacing: .055em;
      margin-top: 1.4rem;
    }
    .hint {
      font-size: .8em;
      color: #aeaeb2;
      font-weight: 400;
      text-transform: none;
      letter-spacing: 0;
    }
    input[type=text], select {
      width: 100%;
      padding: .65rem .85rem;
      margin-top: .4rem;
      border: 1.5px solid #e0e0e5;
      border-radius: 10px;
      font-size: .95rem;
      color: #1d1d1f;
      background: #fafafa;
      outline: none;
      transition: border-color .15s, box-shadow .15s;
      font-family: inherit;
    }
    input[type=text]:focus, select:focus {
      border-color: #0071e3;
      box-shadow: 0 0 0 3px rgba(0,113,227,.12);
      background: #fff;
    }
    select[multiple] {
      padding: .5rem .85rem;
      line-height: 1.8;
    }
    .flash-ok {
      display: flex;
      align-items: center;
      gap: .5rem;
      padding: .7rem 1rem;
      border-radius: 10px;
      background: #f0faf4;
      color: #1a7f4b;
      font-size: .875rem;
      margin-bottom: 1.25rem;
    }
    .flash-err {
      display: flex;
      align-items: center;
      gap: .5rem;
      padding: .7rem 1rem;
      border-radius: 10px;
      background: #fff2f2;
      color: #c0392b;
      font-size: .875rem;
      margin-bottom: 1.25rem;
    }
    .actions {
      margin-top: 2rem;
      display: flex;
      align-items: center;
      gap: 1.25rem;
    }
    button {
      padding: .65rem 2.2rem;
      border: none;
      border-radius: 10px;
      background: #0071e3;
      color: #fff;
      font-size: .95rem;
      font-weight: 500;
      cursor: pointer;
      transition: background .15s, transform .1s;
      font-family: inherit;
    }
    button:hover { background: #0077ed; }
    button:active { transform: scale(.98); }
    a.logout {
      font-size: .875rem;
      color: #aeaeb2;
      text-decoration: none;
      transition: color .15s;
    }
    a.logout:hover { color: #6e6e73; }
  </style>
</head>
<body>
  <div class="card">
  <h2>Configuración del scheduler</h2>
  {{if .Success}}<div class="flash-ok">Configuración guardada correctamente.</div>{{end}}
  {{if .Error}}<div class="flash-err">{{.Error}}</div>{{end}}

  <form method="POST" action="/config">

    <label>HOURS_IN <span class="hint">(ej: 09:00 o 09:00,14:00)</span>
      <input type="text" name="hours_in" value="{{.HoursIn}}" required>
    </label>

    <label>HOURS_OUT <span class="hint">(ej: 18:00 o 13:00,18:00)</span>
      <input type="text" name="hours_out" value="{{.HoursOut}}" required>
    </label>

    <label>WEEKEND — ejecutar en fin de semana
      <select name="weekend">
        <option value="true"{{if .Weekend}} selected{{end}}>Sí (true)</option>
        <option value="false"{{if .WeekendFalse}} selected{{end}}>No (false)</option>
      </select>
    </label>

    <label>LOCATION_OFFICE <span class="hint">(latitud,longitud)</span>
      <input type="text" name="location_office" value="{{.LocationOffice}}">
    </label>

    <label>LOCATION_HOME <span class="hint">(latitud,longitud)</span>
      <input type="text" name="location_home" value="{{.LocationHome}}">
    </label>

    <label>OFFICE_DAYS <span class="hint">(Ctrl/Cmd+click para selección múltiple)</span>
      <select name="office_days" multiple size="7">
        {{range .AllDays}}
        <option value="{{.Name}}"{{if .Selected}} selected{{end}}>{{.Name}}</option>
        {{end}}
      </select>
    </label>

    <div class="actions">
      <button type="submit">Guardar</button>
      <a class="logout" href="/logout">Cerrar sesión</a>
    </div>
  </form>
  </div>
</body>
</html>`))

// ─── Helpers ─────────────────────────────────────────────────────────────────

var weekdayOrder = []time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
	time.Friday, time.Saturday, time.Sunday,
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func locationToString(loc location) string {
	if loc.lat == 0 && loc.lon == 0 {
		return ""
	}
	return formatFloat(loc.lat) + "," + formatFloat(loc.lon)
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func configToData(cfg config, success bool, errMsg string) configData {
	days := make([]dayOption, len(weekdayOrder))
	for i, wd := range weekdayOrder {
		days[i] = dayOption{
			Name:     titleCase(dayNames[wd]),
			Selected: cfg.officeDays[wd],
		}
	}

	return configData{
		HoursIn:        strings.Join(cfg.hoursIn, ","),
		HoursOut:       strings.Join(cfg.hoursOut, ","),
		Weekend:        cfg.weekend,
		WeekendFalse:   !cfg.weekend,
		LocationOffice: locationToString(cfg.locationOffice),
		LocationHome:   locationToString(cfg.locationHome),
		AllDays:        days,
		Success:        success,
		Error:          errMsg,
	}
}

// ─── .env file update ────────────────────────────────────────────────────────

func updateDotEnv(path string, changes map[string]string) error {
	var lines []string

	f, err := os.Open(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			return err
		}
	}

	written := make(map[string]bool)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || trimmed == "" {
			continue
		}
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		if newVal, ok := changes[key]; ok {
			lines[i] = key + "=" + newVal
			written[key] = true
		}
	}

	for key, val := range changes {
		if !written[key] {
			lines = append(lines, key+"="+val)
		}
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ─── Auth middleware ──────────────────────────────────────────────────────────

func authMiddleware(store *sessionStore, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil || !store.validate(cookie.Value) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// ─── Handlers ────────────────────────────────────────────────────────────────

func handleLoginGET(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := loginTmpl.Execute(w, loginData{}); err != nil {
		log.Printf("error renderizando login: %v", err)
	}
}

func handleLoginPOST(w http.ResponseWriter, r *http.Request, store *sessionStore) {
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		http.Error(w, "ADMIN_PASSWORD no configurado en el servidor", http.StatusInternalServerError)
		return
	}

	input := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(input), []byte(adminPassword)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := loginTmpl.Execute(w, loginData{Error: "Contraseña incorrecta"}); err != nil {
			log.Printf("error renderizando login: %v", err)
		}
		return
	}

	token, err := store.create()
	if err != nil {
		http.Error(w, "Error interno al crear sesión", http.StatusInternalServerError)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, "/config", http.StatusFound)
}

func handleLogout(w http.ResponseWriter, r *http.Request, store *sessionStore) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		store.revoke(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	http.Redirect(w, r, "/login", http.StatusFound)
}

func handleConfigGET(w http.ResponseWriter, r *http.Request, holder *configHolder) {
	data := configToData(holder.get(), false, "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := configTmpl.Execute(w, data); err != nil {
		log.Printf("error renderizando config: %v", err)
	}
}

func handleConfigPOST(w http.ResponseWriter, r *http.Request, holder *configHolder) {
	if err := r.ParseForm(); err != nil {
		renderConfigError(w, holder, "Error al parsear el formulario")
		return
	}

	hoursIn := strings.TrimSpace(r.FormValue("hours_in"))
	hoursOut := strings.TrimSpace(r.FormValue("hours_out"))
	weekendVal := r.FormValue("weekend")
	locOfficeRaw := strings.TrimSpace(r.FormValue("location_office"))
	locHomeRaw := strings.TrimSpace(r.FormValue("location_home"))
	officeDaysSelected := r.Form["office_days"]

	// Validar HOURS_IN
	parsedIn := splitTimes(hoursIn)
	for _, t := range parsedIn {
		if _, err := parseTime(t, actionIn); err != nil {
			renderConfigError(w, holder, fmt.Sprintf("HOURS_IN inválido (%q): %v", t, err))
			return
		}
	}
	if len(parsedIn) == 0 {
		renderConfigError(w, holder, "HOURS_IN no puede estar vacío")
		return
	}

	// Validar HOURS_OUT
	parsedOut := splitTimes(hoursOut)
	for _, t := range parsedOut {
		if _, err := parseTime(t, actionOut); err != nil {
			renderConfigError(w, holder, fmt.Sprintf("HOURS_OUT inválido (%q): %v", t, err))
			return
		}
	}
	if len(parsedOut) == 0 {
		renderConfigError(w, holder, "HOURS_OUT no puede estar vacío")
		return
	}

	// Validar localizaciones
	if _, err := parseLocation(locOfficeRaw); err != nil {
		renderConfigError(w, holder, fmt.Sprintf("LOCATION_OFFICE inválido: %v", err))
		return
	}
	if _, err := parseLocation(locHomeRaw); err != nil {
		renderConfigError(w, holder, fmt.Sprintf("LOCATION_HOME inválido: %v", err))
		return
	}

	// Construir OFFICE_DAYS string
	officeDaysStr := strings.Join(officeDaysSelected, ",")

	// Actualizar .env
	changes := map[string]string{
		"HOURS_IN":        hoursIn,
		"HOURS_OUT":       hoursOut,
		"WEEKEND":         weekendVal,
		"LOCATION_OFFICE": locOfficeRaw,
		"LOCATION_HOME":   locHomeRaw,
		"OFFICE_DAYS":     officeDaysStr,
	}
	if err := updateDotEnv(".env", changes); err != nil {
		log.Printf("error actualizando .env: %v", err)
	}

	// Actualizar variables de entorno del proceso
	for k, v := range changes {
		os.Setenv(k, v)
	}

	// Recargar config
	newCfg, err := buildConfig()
	if err != nil {
		renderConfigError(w, holder, fmt.Sprintf("Error en la nueva configuración: %v", err))
		return
	}
	holder.set(newCfg)
	log.Println("Configuración actualizada desde la UI web")

	data := configToData(newCfg, true, "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := configTmpl.Execute(w, data); err != nil {
		log.Printf("error renderizando config: %v", err)
	}
}

func renderConfigError(w http.ResponseWriter, holder *configHolder, msg string) {
	data := configToData(holder.get(), false, msg)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	if err := configTmpl.Execute(w, data); err != nil {
		log.Printf("error renderizando config: %v", err)
	}
}

// ─── Server ───────────────────────────────────────────────────────────────────

func startWebServer(holder *configHolder) {
	port := os.Getenv("ADMIN_PORT")
	if port == "" {
		port = defaultPort
	}

	store := newSessionStore()
	mux := http.NewServeMux()

	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleLoginGET(w, r)
		case http.MethodPost:
			handleLoginPOST(w, r, store)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		handleLogout(w, r, store)
	})

	mux.HandleFunc("/config", authMiddleware(store, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleConfigGET(w, r, holder)
		case http.MethodPost:
			handleConfigPOST(w, r, holder)
		default:
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/config", http.StatusFound)
	})

	log.Printf("Admin UI disponible en http://localhost:%s/login", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Printf("Error en servidor web admin: %v", err)
	}
}
