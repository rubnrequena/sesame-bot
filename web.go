package main

import (
	"context"
	"embed"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	appdb "sesame-bot/internal/db"
	appcrypto "sesame-bot/internal/crypto"
	"sesame-bot/internal/models"
	"sesame-bot/internal/scheduler"
)

//go:embed templates
var templateFS embed.FS

const cookieName = "sesame_session"
const defaultPort = "8080"

// userContextKey is used to store the authenticated user in request context.
type userContextKey struct{}

// ─── Template helpers ─────────────────────────────────────────────────────────

var tmplFuncs = template.FuncMap{
	"not": func(b bool) bool { return !b },
}

func parseTemplates(name string) *template.Template {
	return template.Must(
		template.New("base.html").Funcs(tmplFuncs).ParseFS(templateFS, "templates/base.html", "templates/"+name),
	)
}

// ─── Auth middleware ──────────────────────────────────────────────────────────

func requireAuth(pool *pgxpool.Pool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(cookieName)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		user, err := appdb.GetSessionUser(r.Context(), pool, cookie.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		ctx := context.WithValue(r.Context(), userContextKey{}, user)
		next(w, r.WithContext(ctx))
	}
}

func requireAdmin(pool *pgxpool.Pool, next http.HandlerFunc) http.HandlerFunc {
	return requireAuth(pool, func(w http.ResponseWriter, r *http.Request) {
		user := r.Context().Value(userContextKey{}).(*models.User)
		if !user.IsAdmin {
			http.Error(w, "Acceso denegado", http.StatusForbidden)
			return
		}
		next(w, r)
	})
}

func currentUser(r *http.Request) *models.User {
	u, _ := r.Context().Value(userContextKey{}).(*models.User)
	return u
}

// ─── Register ─────────────────────────────────────────────────────────────────

func handleRegister(pool *pgxpool.Pool) http.HandlerFunc {
	type data struct {
		Error string
		Email string
	}
	tmpl := parseTemplates("register.html")
	render := func(w http.ResponseWriter, d data) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, d); err != nil {
			log.Printf("register render: %v", err)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		// Only allow registration when no users exist yet
		count, err := appdb.CountUsers(r.Context(), pool)
		if err != nil {
			http.Error(w, "Error interno", http.StatusInternalServerError)
			return
		}
		if count > 0 {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		if r.Method == http.MethodGet {
			render(w, data{})
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		password := r.FormValue("password")

		if email == "" || len(password) < 8 {
			render(w, data{Error: "El correo y la contraseña (mínimo 8 caracteres) son obligatorios", Email: email})
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
		if err != nil {
			http.Error(w, "Error interno", http.StatusInternalServerError)
			return
		}

		// First user is always admin
		user, err := appdb.CreateUser(r.Context(), pool, email, string(hash), true)
		if err != nil {
			render(w, data{Error: "Error creando el usuario: " + err.Error(), Email: email})
			return
		}

		token, err := appdb.CreateSession(r.Context(), pool, user.ID)
		if err != nil {
			http.Error(w, "Error interno", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, token)
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	}
}

// ─── Login ────────────────────────────────────────────────────────────────────

func handleLogin(pool *pgxpool.Pool) http.HandlerFunc {
	type data struct {
		Error         string
		Email         string
		AllowRegister bool
	}
	tmpl := parseTemplates("login.html")
	render := func(w http.ResponseWriter, d data) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, d); err != nil {
			log.Printf("login render: %v", err)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		count, _ := appdb.CountUsers(r.Context(), pool)
		allowRegister := count == 0

		if r.Method == http.MethodGet {
			render(w, data{AllowRegister: allowRegister})
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		password := r.FormValue("password")

		user, err := appdb.GetUserByEmail(r.Context(), pool, email)
		if err != nil || !user.IsActive {
			render(w, data{Error: "Credenciales incorrectas", Email: email, AllowRegister: allowRegister})
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
			render(w, data{Error: "Credenciales incorrectas", Email: email, AllowRegister: allowRegister})
			return
		}

		token, err := appdb.CreateSession(r.Context(), pool, user.ID)
		if err != nil {
			http.Error(w, "Error interno", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, token)
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	}
}

// ─── Logout ───────────────────────────────────────────────────────────────────

func handleLogout(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(cookieName); err == nil {
			_ = appdb.DeleteSession(r.Context(), pool, cookie.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

// ─── Dashboard ────────────────────────────────────────────────────────────────

func handleDashboard(pool *pgxpool.Pool, sched *scheduler.Scheduler) http.HandlerFunc {
	type data struct {
		User            *models.User
		Config          *models.UserConfig
		HasPassword     bool
		PasswordInMemory bool
		Logs            []models.CheckinLog
	}
	tmpl := parseTemplates("dashboard.html")

	return func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r)
		cfg, err := appdb.GetUserConfig(r.Context(), pool, user.ID)
		if err != nil {
			cfg = &models.UserConfig{}
		}

		_, inMemory := sched.MemPasswords.Load(user.ID)
		hasPassword := inMemory || cfg.SesamePasswordEnc != ""

		logs, _ := appdb.GetUserLogs(r.Context(), pool, user.ID, 5, 0)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data{
			User:             user,
			Config:           cfg,
			HasPassword:      hasPassword,
			PasswordInMemory: inMemory,
			Logs:             logs,
		}); err != nil {
			log.Printf("dashboard render: %v", err)
		}
	}
}

// ─── Config ───────────────────────────────────────────────────────────────────

type dayOption struct {
	Name     string
	Selected bool
}

func handleConfig(pool *pgxpool.Pool, sched *scheduler.Scheduler) http.HandlerFunc {
	type data struct {
		User           *models.User
		Cfg            *models.UserConfig
		LocationOffice string
		LocationHome   string
		AllDays        []dayOption
		Success        bool
		Error          string
		PwSuccess      bool
		PwError        string
		PwMode         string
	}

	tmpl := parseTemplates("config.html")

	buildData := func(r *http.Request, user *models.User, success bool, errMsg string, pwSuccess bool, pwErr string) data {
		cfg, _ := appdb.GetUserConfig(r.Context(), pool, user.ID)
		if cfg == nil {
			cfg = &models.UserConfig{}
		}

		officeDaysMap := parseOfficeDays(cfg.OfficeDays)
		var allDays []dayOption
		for _, wd := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday} {
			allDays = append(allDays, dayOption{
				Name:     titleCase(dayNames[wd]),
				Selected: officeDaysMap[wd],
			})
		}

		pwMode := "db"
		if _, inMem := sched.MemPasswords.Load(user.ID); inMem {
			pwMode = "memory"
		}

		return data{
			User:           user,
			Cfg:            cfg,
			LocationOffice: locationToStr(cfg.LocationOfficeLat, cfg.LocationOfficeLon),
			LocationHome:   locationToStr(cfg.LocationHomeLat, cfg.LocationHomeLon),
			AllDays:        allDays,
			Success:        success,
			Error:          errMsg,
			PwSuccess:      pwSuccess,
			PwError:        pwErr,
			PwMode:         pwMode,
		}
	}

	render := func(w http.ResponseWriter, d data) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, d); err != nil {
			log.Printf("config render: %v", err)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r)

		if r.Method == http.MethodGet {
			render(w, buildData(r, user, false, "", false, ""))
			return
		}

		if err := r.ParseForm(); err != nil {
			render(w, buildData(r, user, false, "Error al parsear el formulario", false, ""))
			return
		}

		sesameEmail := strings.TrimSpace(r.FormValue("sesame_email"))
		hoursIn := strings.TrimSpace(r.FormValue("hours_in"))
		hoursOut := strings.TrimSpace(r.FormValue("hours_out"))
		weekendVal := r.FormValue("weekend")
		locOfficeRaw := strings.TrimSpace(r.FormValue("location_office"))
		locHomeRaw := strings.TrimSpace(r.FormValue("location_home"))
		officeDaysSelected := r.Form["office_days"]

		if sesameEmail == "" {
			render(w, buildData(r, user, false, "El email de Sesame es obligatorio", false, ""))
			return
		}
		for _, t := range splitTimes(hoursIn) {
			if _, err := parseTime(t, actionIn); err != nil {
				render(w, buildData(r, user, false, "HOURS_IN inválido: "+err.Error(), false, ""))
				return
			}
		}
		if len(splitTimes(hoursIn)) == 0 {
			render(w, buildData(r, user, false, "El horario de entrada es obligatorio", false, ""))
			return
		}
		for _, t := range splitTimes(hoursOut) {
			if _, err := parseTime(t, actionOut); err != nil {
				render(w, buildData(r, user, false, "HOURS_OUT inválido: "+err.Error(), false, ""))
				return
			}
		}
		if len(splitTimes(hoursOut)) == 0 {
			render(w, buildData(r, user, false, "El horario de salida es obligatorio", false, ""))
			return
		}

		locOffice, err := parseLocation(locOfficeRaw)
		if err != nil {
			render(w, buildData(r, user, false, "Ubicación oficina inválida: "+err.Error(), false, ""))
			return
		}
		locHome, err := parseLocation(locHomeRaw)
		if err != nil {
			render(w, buildData(r, user, false, "Ubicación casa inválida: "+err.Error(), false, ""))
			return
		}

		weekend := weekendVal == "true"
		officeDaysStr := strings.Join(officeDaysSelected, ",")

		// Preserve existing password enc
		existingCfg, _ := appdb.GetUserConfig(r.Context(), pool, user.ID)
		pwEnc := ""
		if existingCfg != nil {
			pwEnc = existingCfg.SesamePasswordEnc
		}

		cfg := &models.UserConfig{
			UserID:            user.ID,
			SesameEmail:       sesameEmail,
			SesamePasswordEnc: pwEnc,
			Headless:          true,
			Weekend:           weekend,
			HoursIn:           hoursIn,
			HoursOut:          hoursOut,
			LocationOfficeLat: locOffice.lat,
			LocationOfficeLon: locOffice.lon,
			LocationHomeLat:   locHome.lat,
			LocationHomeLon:   locHome.lon,
			OfficeDays:        officeDaysStr,
		}

		if err := appdb.UpsertUserConfig(r.Context(), pool, cfg); err != nil {
			render(w, buildData(r, user, false, "Error guardando la configuración: "+err.Error(), false, ""))
			return
		}

		log.Printf("Config actualizada para usuario %s", user.ID)
		render(w, buildData(r, user, true, "", false, ""))
	}
}

// ─── Config password ──────────────────────────────────────────────────────────

func handleConfigPassword(pool *pgxpool.Pool, sched *scheduler.Scheduler) http.HandlerFunc {
	type data struct {
		User      *models.User
		PwSuccess bool
		PwError   string
	}
	tmpl := parseTemplates("config.html")
	_ = tmpl

	return func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Error al parsear formulario", http.StatusBadRequest)
			return
		}

		mode := r.FormValue("pw_mode")
		password := r.FormValue("sesame_password")

		switch mode {
		case "clear":
			sched.MemPasswords.Delete(user.ID)
			if err := appdb.ClearSesamePassword(r.Context(), pool, user.ID); err != nil {
				log.Printf("handleConfigPassword: error borrando pw: %v", err)
			}
		case "memory":
			if password == "" {
				http.Redirect(w, r, "/config?pw_error=La+contrase%C3%B1a+no+puede+estar+vac%C3%ADa", http.StatusFound)
				return
			}
			sched.MemPasswords.Store(user.ID, password)
			// Clear any DB-stored version
			_ = appdb.ClearSesamePassword(r.Context(), pool, user.ID)
		case "db":
			if password == "" {
				http.Redirect(w, r, "/config?pw_error=La+contrase%C3%B1a+no+puede+estar+vac%C3%ADa", http.StatusFound)
				return
			}
			encKey, err := appcrypto.LoadKey()
			if err != nil || len(encKey) == 0 {
				http.Redirect(w, r, "/config?pw_error=ENCRYPTION_KEY+no+configurada+en+el+servidor", http.StatusFound)
				return
			}
			enc, err := appcrypto.Encrypt(encKey, password)
			if err != nil {
				http.Redirect(w, r, "/config?pw_error=Error+cifrando+la+contrase%C3%B1a", http.StatusFound)
				return
			}
			if err := appdb.UpdateSesamePassword(r.Context(), pool, user.ID, enc); err != nil {
				log.Printf("handleConfigPassword: error guardando pw cifrada: %v", err)
				http.Redirect(w, r, "/config?pw_error=Error+guardando+la+contrase%C3%B1a", http.StatusFound)
				return
			}
			// Remove from memory if it was there
			sched.MemPasswords.Delete(user.ID)
		}

		http.Redirect(w, r, "/config?pw_success=1", http.StatusFound)
	}
}

// ─── Logs ─────────────────────────────────────────────────────────────────────

func handleLogs(pool *pgxpool.Pool) http.HandlerFunc {
	const pageSize = 25

	type data struct {
		User       *models.User
		Logs       []models.CheckinLog
		HasMore    bool
		NextOffset int
	}
	tmpl := parseTemplates("logs.html")
	rowsTmpl := template.Must(template.New("rows").Parse(`
{{range .}}
<tr>
  <td>{{.ExecutedAt.Format "02/01/2006 15:04:05"}}</td>
  <td><strong>{{.Action}}</strong></td>
  <td>
    {{if eq .Status "ok"}}<span class="badge badge-ok">OK</span>
    {{else if eq .Status "error"}}<span class="badge badge-err">Error</span>
    {{else}}<span class="badge badge-skip">Omitido</span>{{end}}
  </td>
  <td style="color:#6e6e73;font-size:.825rem">{{.Message}}</td>
</tr>
{{end}}`))

	return func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r)
		offsetStr := r.URL.Query().Get("offset")
		offset, _ := strconv.Atoi(offsetStr)

		logs, _ := appdb.GetUserLogs(r.Context(), pool, user.ID, pageSize+1, offset)
		hasMore := len(logs) > pageSize
		if hasMore {
			logs = logs[:pageSize]
		}

		// HTMX partial: return only rows
		if r.Header.Get("HX-Request") == "true" && offset > 0 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = rowsTmpl.Execute(w, logs)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data{
			User:       user,
			Logs:       logs,
			HasMore:    hasMore,
			NextOffset: offset + pageSize,
		}); err != nil {
			log.Printf("logs render: %v", err)
		}
	}
}

// ─── Admin ────────────────────────────────────────────────────────────────────

func handleAdmin(pool *pgxpool.Pool) http.HandlerFunc {
	type data struct {
		User       *models.User
		Users      []models.User
		RecentLogs []models.CheckinLog
		Success    string
	}
	tmpl := parseTemplates("admin.html")

	return func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r)
		users, _ := appdb.ListUsers(r.Context(), pool)
		recentLogs, _ := appdb.GetAllUsersRecentLogs(r.Context(), pool, 20)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data{
			User:       user,
			Users:      users,
			RecentLogs: recentLogs,
		}); err != nil {
			log.Printf("admin render: %v", err)
		}
	}
}

func handleAdminToggleUser(pool *pgxpool.Pool) http.HandlerFunc {
	type rowData struct {
		models.User
	}
	rowTmpl := template.Must(template.New("row").Parse(`
<tr id="user-{{.ID}}">
  <td>{{.Email}}</td>
  <td>{{if .IsAdmin}}<span class="badge badge-ok">Admin</span>{{else}}Usuario{{end}}</td>
  <td>
    {{if .IsActive}}<span class="badge badge-active">Activo</span>
    {{else}}<span class="badge badge-inactive">Inactivo</span>{{end}}
  </td>
  <td style="color:#6e6e73;font-size:.825rem">{{.CreatedAt.Format "02/01/2006"}}</td>
  <td>
    <button class="btn btn-ghost btn-sm"
      hx-post="/admin/users/{{.ID}}/toggle"
      hx-target="#user-{{.ID}}"
      hx-swap="outerHTML">
      {{if .IsActive}}Desactivar{{else}}Activar{{end}}
    </button>
    <a href="/admin/users/{{.ID}}/logs" class="btn btn-ghost btn-sm" style="margin-left:.35rem">Logs</a>
  </td>
</tr>`))

	return func(w http.ResponseWriter, r *http.Request) {
		// Extract user ID from path: /admin/users/{id}/toggle
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 4 {
			http.Error(w, "ID inválido", http.StatusBadRequest)
			return
		}
		targetID := parts[3]

		if err := appdb.ToggleUserActive(r.Context(), pool, targetID); err != nil {
			http.Error(w, "Error actualizando usuario", http.StatusInternalServerError)
			return
		}
		user, err := appdb.GetUserByID(r.Context(), pool, targetID)
		if err != nil {
			http.Error(w, "Usuario no encontrado", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = rowTmpl.Execute(w, user)
	}
}

func handleAdminUserLogs(pool *pgxpool.Pool) http.HandlerFunc {
	type data struct {
		User       *models.User
		TargetUser *models.User
		Logs       []models.CheckinLog
	}
	tmpl := parseTemplates("admin_user_logs.html")

	return func(w http.ResponseWriter, r *http.Request) {
		// /admin/users/{id}/logs
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 4 {
			http.Error(w, "ID inválido", http.StatusBadRequest)
			return
		}
		targetID := parts[3]

		user := currentUser(r)
		targetUser, err := appdb.GetUserByID(r.Context(), pool, targetID)
		if err != nil {
			http.Error(w, "Usuario no encontrado", http.StatusNotFound)
			return
		}
		logs, _ := appdb.GetUserLogs(r.Context(), pool, targetID, 50, 0)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data{User: user, TargetUser: targetUser, Logs: logs}); err != nil {
			log.Printf("admin user logs render: %v", err)
		}
	}
}

// ─── Server ───────────────────────────────────────────────────────────────────

func startWebServer(pool *pgxpool.Pool, sched *scheduler.Scheduler) {
	port := os.Getenv("ADMIN_PORT")
	if port == "" {
		port = defaultPort
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/register", handleRegister(pool))
	mux.HandleFunc("/login", handleLogin(pool))
	mux.HandleFunc("/logout", handleLogout(pool))

	mux.HandleFunc("/dashboard", requireAuth(pool, handleDashboard(pool, sched)))
	mux.HandleFunc("/config", requireAuth(pool, handleConfig(pool, sched)))
	mux.HandleFunc("/config/password", requireAuth(pool, handleConfigPassword(pool, sched)))
	mux.HandleFunc("/logs", requireAuth(pool, handleLogs(pool)))

	mux.HandleFunc("/admin", requireAdmin(pool, handleAdmin(pool)))
	mux.HandleFunc("/admin/users/", requireAdmin(pool, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/toggle") {
			handleAdminToggleUser(pool)(w, r)
		} else if strings.HasSuffix(r.URL.Path, "/logs") {
			handleAdminUserLogs(pool)(w, r)
		} else {
			http.NotFound(w, r)
		}
	}))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})

	// Periodically clean expired sessions
	go func() {
		for {
			time.Sleep(1 * time.Hour)
			_ = appdb.CleanExpiredSessions(context.Background(), pool)
		}
	}()

	log.Printf("Servidor web disponible en http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Printf("Error en servidor web: %v", err)
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400,
	})
}

func locationToStr(lat, lon float64) string {
	if lat == 0 && lon == 0 {
		return ""
	}
	return strconv.FormatFloat(lat, 'f', -1, 64) + "," + strconv.FormatFloat(lon, 'f', -1, 64)
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// Ensure pgx and bcrypt imports are used (suppress unused import errors)
var _ = pgx.ErrNoRows
