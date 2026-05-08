package main

import (
	"context"
	"crypto/rand"
	"embed"
	"fmt"
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
		Error   string
		Email   string
		Pending bool
	}
	tmpl := parseTemplates("register.html")
	render := func(w http.ResponseWriter, d data) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, d); err != nil {
			log.Printf("register render: %v", err)
		}
	}

	return func(w http.ResponseWriter, r *http.Request) {
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

		count, err := appdb.CountUsers(r.Context(), pool)
		if err != nil {
			http.Error(w, "Error interno", http.StatusInternalServerError)
			return
		}

		// First user becomes admin and is auto-approved; subsequent users need approval
		isAdmin := count == 0
		isApproved := count == 0

		user, err := appdb.CreateUser(r.Context(), pool, email, string(hash), isAdmin, isApproved)
		if err != nil {
			render(w, data{Error: "Error creando el usuario: " + err.Error(), Email: email})
			return
		}

		if !isApproved {
			render(w, data{Pending: true})
			return
		}

		token, err := appdb.CreateSession(r.Context(), pool, user.ID, appdb.SessionTTLShort)
		if err != nil {
			http.Error(w, "Error interno", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, token, 86400)
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
		if r.Method == http.MethodGet {
			render(w, data{AllowRegister: true})
			return
		}

		email := strings.TrimSpace(r.FormValue("email"))
		password := r.FormValue("password")
		remember := r.FormValue("remember") == "1"

		user, err := appdb.GetUserByEmail(r.Context(), pool, email)
		if err != nil || !user.IsActive {
			render(w, data{Error: "Credenciales incorrectas", Email: email, AllowRegister: true})
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
			render(w, data{Error: "Credenciales incorrectas", Email: email, AllowRegister: true})
			return
		}

		ttl := appdb.SessionTTLShort
		maxAge := 86400
		if remember {
			ttl = appdb.SessionTTLLong
			maxAge = 30 * 86400
		}

		token, err := appdb.CreateSession(r.Context(), pool, user.ID, ttl)
		if err != nil {
			http.Error(w, "Error interno", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, token, maxAge)
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

type dayOverrideUI struct {
	Weekday int
	Name    string
	Hours   string // interleaved CSV: "09:00,14:00,15:00,18:00"
}

var dayNamesES = map[time.Weekday]string{
	time.Monday:    "Lunes",
	time.Tuesday:   "Martes",
	time.Wednesday: "Miércoles",
	time.Thursday:  "Jueves",
	time.Friday:    "Viernes",
	time.Saturday:  "Sábado",
	time.Sunday:    "Domingo",
}

func handleConfig(pool *pgxpool.Pool, sched *scheduler.Scheduler) http.HandlerFunc {
	type data struct {
		User               *models.User
		Cfg                *models.UserConfig
		LocationOffice     string
		LocationHome       string
		AllDays            []dayOption
		DayOverrides       []dayOverrideUI
		Success            bool
		Error              string
		PwSuccess          bool
		PwError            string
		PwMode             string
		AccountPwSuccess   bool
		AccountPwError     string
		DayOverrideSuccess bool
		DayOverrideError   string
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

		existingOverrides, _ := appdb.GetDayOverrides(r.Context(), pool, user.ID)
		overridesMap := make(map[int]models.DayOverride)
		for _, o := range existingOverrides {
			overridesMap[o.Weekday] = o
		}
		var dayOverridesList []dayOverrideUI
		for _, wd := range []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday} {
			hours := ""
			if o, ok := overridesMap[int(wd)]; ok {
				hours = interleaveHours(o.HoursIn, o.HoursOut)
			}
			dayOverridesList = append(dayOverridesList, dayOverrideUI{
				Weekday: int(wd),
				Name:    dayNamesES[wd],
				Hours:   hours,
			})
		}

		pwMode := "db"
		if _, inMem := sched.MemPasswords.Load(user.ID); inMem {
			pwMode = "memory"
		}

		return data{
			User:               user,
			Cfg:                cfg,
			LocationOffice:     locationToStr(cfg.LocationOfficeLat, cfg.LocationOfficeLon),
			LocationHome:       locationToStr(cfg.LocationHomeLat, cfg.LocationHomeLon),
			AllDays:            allDays,
			DayOverrides:       dayOverridesList,
			Success:            success,
			Error:              errMsg,
			PwSuccess:          pwSuccess,
			PwError:            pwErr,
			PwMode:             pwMode,
			AccountPwSuccess:   r.URL.Query().Get("account_pw_success") == "1",
			AccountPwError:     r.URL.Query().Get("account_pw_error"),
			DayOverrideSuccess: r.URL.Query().Get("day_override_success") == "1",
			DayOverrideError:   r.URL.Query().Get("day_override_error"),
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
			pwSuccess := r.URL.Query().Get("pw_success") == "1"
			pwErr := r.URL.Query().Get("pw_error")
			render(w, buildData(r, user, false, "", pwSuccess, pwErr))
			return
		}

		if !user.IsApproved {
			render(w, buildData(r, user, false, "Tu cuenta está pendiente de aprobación por un administrador.", false, ""))
			return
		}

		if err := r.ParseForm(); err != nil {
			render(w, buildData(r, user, false, "Error al parsear el formulario", false, ""))
			return
		}

		sesameEmail := strings.TrimSpace(r.FormValue("sesame_email"))
		locOfficeRaw := strings.TrimSpace(r.FormValue("location_office"))
		locHomeRaw := strings.TrimSpace(r.FormValue("location_home"))
		officeDaysSelected := r.Form["office_days"]

		if sesameEmail == "" {
			render(w, buildData(r, user, false, "El email de Sesame es obligatorio", false, ""))
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

// ─── Account password ─────────────────────────────────────────────────────────

func handleConfigAccountPassword(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r)

		currentPw := r.FormValue("current_password")
		newPw := r.FormValue("new_password")
		confirmPw := r.FormValue("confirm_password")

		redirect := func(errMsg string) {
			if errMsg != "" {
				http.Redirect(w, r, "/config?account_pw_error="+strings.ReplaceAll(errMsg, " ", "+"), http.StatusFound)
			} else {
				http.Redirect(w, r, "/config?account_pw_success=1", http.StatusFound)
			}
		}

		if currentPw == "" || newPw == "" || confirmPw == "" {
			redirect("Todos los campos son obligatorios")
			return
		}

		dbUser, err := appdb.GetUserByID(r.Context(), pool, user.ID)
		if err != nil {
			redirect("Error obteniendo datos del usuario")
			return
		}

		if bcrypt.CompareHashAndPassword([]byte(dbUser.PasswordHash), []byte(currentPw)) != nil {
			redirect("La contraseña actual no es correcta")
			return
		}

		if len(newPw) < 8 {
			redirect("La nueva contraseña debe tener al menos 8 caracteres")
			return
		}

		if newPw != confirmPw {
			redirect("Las contraseñas nuevas no coinciden")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(newPw), 12)
		if err != nil {
			redirect("Error procesando la contraseña")
			return
		}

		if err := appdb.UpdateUserPassword(r.Context(), pool, user.ID, string(hash)); err != nil {
			redirect("Error guardando la contraseña")
			return
		}

		redirect("")
	}
}

// ─── Day overrides ────────────────────────────────────────────────────────────

func handleConfigDayOverrides(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := currentUser(r)
		if !user.IsApproved {
			http.Redirect(w, r, "/config?day_override_error=Cuenta+pendiente+de+aprobación", http.StatusFound)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/config?day_override_error=Error+al+parsear+formulario", http.StatusFound)
			return
		}

		weekdayStr := r.FormValue("weekday")
		hours := strings.TrimSpace(r.FormValue("hours"))

		weekday, err := strconv.Atoi(weekdayStr)
		if err != nil || weekday < 0 || weekday > 6 {
			http.Redirect(w, r, "/config?day_override_error=Día+inválido", http.StatusFound)
			return
		}

		if hours == "" {
			if err := appdb.DeleteDayOverride(r.Context(), pool, user.ID, weekday); err != nil {
				log.Printf("handleConfigDayOverrides delete: %v", err)
			}
			http.Redirect(w, r, "/config?day_override_success=1", http.StatusFound)
			return
		}

		times := splitTimes(hours)
		if len(times) < 2 || len(times)%2 != 0 {
			http.Redirect(w, r, "/config?day_override_error=El+número+de+horas+debe+ser+par+y+al+menos+2", http.StatusFound)
			return
		}

		for _, t := range times {
			if err := parseTime(t); err != nil {
				http.Redirect(w, r, "/config?day_override_error=Hora+inválida:+"+strings.ReplaceAll(t, " ", "+"), http.StatusFound)
				return
			}
		}

		var ins, outs []string
		for i, t := range times {
			if i%2 == 0 {
				ins = append(ins, t)
			} else {
				outs = append(outs, t)
			}
		}

		override := &models.DayOverride{
			UserID:   user.ID,
			Weekday:  weekday,
			HoursIn:  strings.Join(ins, ","),
			HoursOut: strings.Join(outs, ","),
		}

		if err := appdb.UpsertDayOverride(r.Context(), pool, override); err != nil {
			log.Printf("handleConfigDayOverrides upsert: %v", err)
			http.Redirect(w, r, "/config?day_override_error=Error+guardando+el+override", http.StatusFound)
			return
		}

		http.Redirect(w, r, "/config?day_override_success=1", http.StatusFound)
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
  <td>
    {{if .IsApproved}}<span class="badge badge-ok">Aprobado</span>
    {{else}}<span class="badge badge-skip">Pendiente</span>{{end}}
  </td>
  <td style="color:#6e6e73;font-size:.825rem">{{.CreatedAt.Format "02/01/2006"}}</td>
  <td>
    <button class="btn btn-ghost btn-sm"
      hx-post="/admin/users/{{.ID}}/toggle"
      hx-target="#user-{{.ID}}"
      hx-swap="outerHTML">
      {{if .IsActive}}Desactivar{{else}}Activar{{end}}
    </button>
    {{if not .IsApproved}}
    <button class="btn btn-ghost btn-sm" style="margin-left:.35rem"
      hx-post="/admin/users/{{.ID}}/approve"
      hx-target="#user-{{.ID}}"
      hx-swap="outerHTML">
      Aprobar
    </button>
    {{end}}
    <button class="btn btn-ghost btn-sm" style="margin-left:.35rem"
      hx-post="/admin/users/{{.ID}}/reset-password"
      hx-target="#reset-result"
      hx-swap="innerHTML">
      Reset pwd
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

func handleAdminApproveUser(pool *pgxpool.Pool) http.HandlerFunc {
	rowTmpl := template.Must(template.New("row").Parse(`
<tr id="user-{{.ID}}">
  <td>{{.Email}}</td>
  <td>{{if .IsAdmin}}<span class="badge badge-ok">Admin</span>{{else}}Usuario{{end}}</td>
  <td>
    {{if .IsActive}}<span class="badge badge-active">Activo</span>
    {{else}}<span class="badge badge-inactive">Inactivo</span>{{end}}
  </td>
  <td>
    {{if .IsApproved}}<span class="badge badge-ok">Aprobado</span>
    {{else}}<span class="badge badge-skip">Pendiente</span>{{end}}
  </td>
  <td style="color:#6e6e73;font-size:.825rem">{{.CreatedAt.Format "02/01/2006"}}</td>
  <td>
    <button class="btn btn-ghost btn-sm"
      hx-post="/admin/users/{{.ID}}/toggle"
      hx-target="#user-{{.ID}}"
      hx-swap="outerHTML">
      {{if .IsActive}}Desactivar{{else}}Activar{{end}}
    </button>
    {{if not .IsApproved}}
    <button class="btn btn-ghost btn-sm" style="margin-left:.35rem"
      hx-post="/admin/users/{{.ID}}/approve"
      hx-target="#user-{{.ID}}"
      hx-swap="outerHTML">
      Aprobar
    </button>
    {{end}}
    <button class="btn btn-ghost btn-sm" style="margin-left:.35rem"
      hx-post="/admin/users/{{.ID}}/reset-password"
      hx-target="#reset-result"
      hx-swap="innerHTML">
      Reset pwd
    </button>
    <a href="/admin/users/{{.ID}}/logs" class="btn btn-ghost btn-sm" style="margin-left:.35rem">Logs</a>
  </td>
</tr>`))

	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 4 {
			http.Error(w, "ID inválido", http.StatusBadRequest)
			return
		}
		targetID := parts[3]

		if err := appdb.ApproveUser(r.Context(), pool, targetID); err != nil {
			http.Error(w, "Error aprobando usuario", http.StatusInternalServerError)
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

func generateRandomPassword(length int) (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i, v := range b {
		b[i] = charset[int(v)%len(charset)]
	}
	return string(b), nil
}

func handleAdminResetPassword(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) < 4 {
			http.Error(w, "ID inválido", http.StatusBadRequest)
			return
		}
		targetID := parts[3]

		user, err := appdb.GetUserByID(r.Context(), pool, targetID)
		if err != nil {
			http.Error(w, "Usuario no encontrado", http.StatusNotFound)
			return
		}

		newPassword, err := generateRandomPassword(16)
		if err != nil {
			http.Error(w, "Error generando contraseña", http.StatusInternalServerError)
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), 12)
		if err != nil {
			http.Error(w, "Error procesando contraseña", http.StatusInternalServerError)
			return
		}

		if err := appdb.UpdateUserPassword(r.Context(), pool, targetID, string(hash)); err != nil {
			http.Error(w, "Error actualizando contraseña", http.StatusInternalServerError)
			return
		}

		log.Printf("Admin: contraseña reseteada para usuario %s (%s)", targetID, user.Email)

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w,
			`<div class="flash-ok" style="justify-content:space-between;align-items:center">
  <span>Nueva contraseña para <strong>%s</strong>:</span>
  <code id="new-pw" style="background:#e8f9ee;padding:.2rem .6rem;border-radius:6px;font-size:.95rem;cursor:pointer;user-select:all"
    onclick="navigator.clipboard.writeText(this.innerText).then(()=>this.style.outline='2px solid #1a7f4b')" title="Clic para copiar">%s</code>
</div>`,
			template.HTMLEscapeString(user.Email), newPassword)
	}
}

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
	mux.HandleFunc("/config/account-password", requireAuth(pool, handleConfigAccountPassword(pool)))
	mux.HandleFunc("/config/day-overrides", requireAuth(pool, handleConfigDayOverrides(pool)))
	mux.HandleFunc("/logs", requireAuth(pool, handleLogs(pool)))

	mux.HandleFunc("/admin", requireAdmin(pool, handleAdmin(pool)))
	mux.HandleFunc("/admin/users/", requireAdmin(pool, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/toggle") {
			handleAdminToggleUser(pool)(w, r)
		} else if strings.HasSuffix(r.URL.Path, "/approve") {
			handleAdminApproveUser(pool)(w, r)
		} else if strings.HasSuffix(r.URL.Path, "/reset-password") {
			handleAdminResetPassword(pool)(w, r)
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

func setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
}

func locationToStr(lat, lon float64) string {
	if lat == 0 && lon == 0 {
		return ""
	}
	return strconv.FormatFloat(lat, 'f', -1, 64) + "," + strconv.FormatFloat(lon, 'f', -1, 64)
}

func interleaveHours(hoursIn, hoursOut string) string {
	ins := splitTimes(hoursIn)
	outs := splitTimes(hoursOut)
	n := len(ins)
	if len(outs) > n {
		n = len(outs)
	}
	combined := make([]string, 0, n*2)
	for i := 0; i < n; i++ {
		if i < len(ins) {
			combined = append(combined, ins[i])
		}
		if i < len(outs) {
			combined = append(combined, outs[i])
		}
	}
	return strings.Join(combined, ",")
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

// Ensure pgx and bcrypt imports are used (suppress unused import errors)
var _ = pgx.ErrNoRows
