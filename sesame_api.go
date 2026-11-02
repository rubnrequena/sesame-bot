package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	// authCaptureTimeout es lo que se espera, ya con el login hecho, a que la SPA
	// emita una petición autenticada (o a que llegue el body del login).
	authCaptureTimeout = 10 * time.Second
	// loginBodyTimeout acota la lectura del body del login. Usa un contexto propio
	// para no depender del deadline de la página.
	loginBodyTimeout = 5 * time.Second
	// apiTimeout es el presupuesto de cada llamada HTTP a la API de Sesame.
	apiTimeout = 10 * time.Second
	// maxHolidayPages evita un bucle si la API devuelve un meta.lastPage absurdo.
	maxHolidayPages = 50
	// maxAPIBodyBytes acota bodies de error/respuesta para no llenar memoria.
	maxAPIBodyBytes = 2 << 20

	sesameBrowserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
		"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
)

// sesameForwardHeaders son cabeceras de la SPA que conviene reutilizar. La API
// puede exigirlas según la región/empresa; no se reenvían cookies ni nada que
// contenga secretos adicionales.
var sesameForwardHeaders = []string{
	"X-Region",
	"Rsrc",
	"X-Sesame-Origin",
	"X-Timezone",
	"X-Bi-Version",
}

var sesameHTTPClient = &http.Client{Timeout: apiTimeout}

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

// sesameAuth es la sesión mínima para llamar a la API: token Bearer, host de la
// API y datos del empleado. Todo vive en memoria durante el run.
type sesameAuth struct {
	Token        string
	APIBase      string
	EmployeeID   string
	CompanyID    string
	ExtraHeaders map[string]string
}

type meCompany struct {
	ID string `json:"id"`
}

type meEmployee struct {
	ID                  string      `json:"id"`
	Email               string      `json:"email"`
	CompanyID           string      `json:"companyId"`
	MyCompany           *meCompany  `json:"myCompany"`
	Companies           []meCompany `json:"companies"`
	AccessibleCompanies []meCompany `json:"accessibleCompanies"`
}

type meEntry struct {
	Employee            *meEmployee `json:"employee"`
	ID                  string      `json:"id"`
	Email               string      `json:"email"`
	CompanyID           string      `json:"companyId"`
	MyCompany           *meCompany  `json:"myCompany"`
	Companies           []meCompany `json:"companies"`
	AccessibleCompanies []meCompany `json:"accessibleCompanies"`
}

type meEnvelope struct {
	Data []meEntry `json:"data"`
}

// startSesameAuthCapture registra los listeners CDP ANTES de navegar y devuelve
// una función que, ya hecho el login, espera el token de la sesión.
//
// La página que se pasa debe tener un contexto de vida larga (context.Background):
// la página de login normal nace con 30s y el login puede tardar hasta 90s. Si el
// listener se ata a esos 30s, muere antes de capturar nada.
func startSesameAuthCapture(page *rod.Page, logger *log.Logger) func() (sesameAuth, error) {
	ch := make(chan sesameAuth, 1)
	var loggedToken sync.Once
	publish := func(a sesameAuth) {
		if a.Token == "" || a.APIBase == "" {
			return
		}
		select {
		case ch <- a:
			loggedToken.Do(func() {
				logger.Printf("holidays: token de sesión capturado (api=%s)", a.APIBase)
			})
		default:
			// Ya hay un token; el primero gana.
		}
	}

	enableCtx, enableCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := (proto.NetworkEnable{}).Call(page.Context(enableCtx))
	enableCancel()
	if err != nil {
		logger.Printf("holidays: error habilitando Network: %v", err)
		return func() (sesameAuth, error) {
			return sesameAuth{}, fmt.Errorf("habilitar Network: %w", err)
		}
	}

	var mu sync.Mutex
	requestURLs := make(map[proto.NetworkRequestID]string)
	requestHeaders := make(map[proto.NetworkRequestID]proto.NetworkHeaders)
	loginRequests := make(map[proto.NetworkRequestID]string)

	// captureFromHeaders lee el Authorization y las cabeceras útiles de una
	// petición de la API. Es la vía principal: la SPA hace security/me en cuanto
	// termina el login.
	captureFromHeaders := func(headers proto.NetworkHeaders, rawURL string) {
		if rawURL == "" || !strings.Contains(rawURL, "/api/v3/") {
			return
		}
		token := authorizationToken(headers)
		if !looksLikeToken(token) {
			return
		}
		base := apiOrigin(rawURL)
		if base == "" {
			return
		}
		publish(sesameAuth{
			Token:        token,
			APIBase:      base,
			EmployeeID:   headerValue(headers, "Esid"),
			CompanyID:    headerValue(headers, "Csid"),
			ExtraHeaders: extractExtraHeaders(headers),
		})
	}

	wait := page.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			if e.Request == nil || !strings.Contains(e.Request.URL, "/api/v3/") {
				return
			}
			mu.Lock()
			requestURLs[e.RequestID] = e.Request.URL
			pending := requestHeaders[e.RequestID]
			delete(requestHeaders, e.RequestID)
			mu.Unlock()

			captureFromHeaders(e.Request.Headers, e.Request.URL)
			if pending != nil {
				// La extra info puede llegar antes que el request; se guardó para
				// capturarla ahora que ya se conoce la URL.
				captureFromHeaders(pending, e.Request.URL)
			}
		},
		func(e *proto.NetworkRequestWillBeSentExtraInfo) {
			mu.Lock()
			rawURL := requestURLs[e.RequestID]
			if rawURL == "" {
				requestHeaders[e.RequestID] = e.Headers
				mu.Unlock()
				return
			}
			mu.Unlock()
			captureFromHeaders(e.Headers, rawURL)
		},
		func(e *proto.NetworkResponseReceived) {
			if e.Response == nil || e.Response.Status < 200 || e.Response.Status >= 300 {
				return
			}
			if !strings.Contains(e.Response.URL, "/api/v3/security/login") {
				return
			}
			mu.Lock()
			loginRequests[e.RequestID] = e.Response.URL
			mu.Unlock()
		},
		func(e *proto.NetworkLoadingFinished) {
			mu.Lock()
			delete(requestURLs, e.RequestID)
			delete(requestHeaders, e.RequestID)
			rawURL, ok := loginRequests[e.RequestID]
			delete(loginRequests, e.RequestID)
			mu.Unlock()
			if !ok {
				return
			}
			requestID := e.RequestID
			go func() {
				// Fallback best-effort: si Chrome ya liberó el body, se ignora.
				// La vía principal es el header Authorization de las peticiones de
				// la SPA; si tampoco hay token, waitAuth devuelve el timeout.
				token, err := loginTokenFromResponse(page, requestID)
				if err != nil {
					return
				}
				if !looksLikeToken(token) {
					logger.Printf("holidays: la respuesta del login no parece un token")
					return
				}
				base := apiOrigin(rawURL)
				if base == "" {
					logger.Printf("holidays: respuesta de login sin host de API")
					return
				}
				publish(sesameAuth{Token: token, APIBase: base})
			}()
		},
	)
	go wait()

	return func() (sesameAuth, error) {
		select {
		case auth := <-ch:
			return auth, nil
		case <-time.After(authCaptureTimeout):
			return sesameAuth{}, fmt.Errorf("timeout esperando token de la sesión (%s)", authCaptureTimeout)
		}
	}
}

// loginTokenFromResponse lee el body de la respuesta del login. Usa un contexto
// propio de 5s: la página de login puede haber agotado su deadline.
func loginTokenFromResponse(page *rod.Page, requestID proto.NetworkRequestID) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), loginBodyTimeout)
	defer cancel()

	res, err := (proto.NetworkGetResponseBody{RequestID: requestID}).Call(page.Context(ctx))
	if err != nil {
		return "", fmt.Errorf("Network.getResponseBody: %w", err)
	}
	body := res.Body
	if res.Base64Encoded {
		decoded, decErr := base64.StdEncoding.DecodeString(res.Body)
		if decErr != nil {
			return "", fmt.Errorf("decodificar body base64: %w", decErr)
		}
		body = string(decoded)
	}
	// El body contiene el token: nunca se loguea.
	return parseLoginToken([]byte(body))
}

func parseLoginToken(body []byte) (string, error) {
	raw := strings.TrimSpace(string(body))
	if raw == "" {
		return "", fmt.Errorf("respuesta de login vacía")
	}

	// Forma esperada por la SPA: { "data": "<jwt>" }. Se acepta { "token": ... }
	// por si la API cambia el envoltorio.
	var envelope struct {
		Data  string `json:"data"`
		Token string `json:"token"`
	}
	if strings.HasPrefix(raw, "{") {
		if err := json.Unmarshal([]byte(raw), &envelope); err == nil {
			if envelope.Data != "" {
				return envelope.Data, nil
			}
			if envelope.Token != "" {
				return envelope.Token, nil
			}
		}
	}

	// Forma JSON string: "eyJ..."
	var str string
	if err := json.Unmarshal([]byte(raw), &str); err == nil && str != "" {
		return str, nil
	}

	// Token crudo. Un JWT no contiene espacios ni llaves.
	if !strings.ContainsAny(raw, "{}[]\" \n\t\r") {
		return raw, nil
	}
	return "", fmt.Errorf("respuesta de login no reconocida (len=%d)", len(raw))
}

// fetchHolidays consulta /security/me si hace falta para resolver el empleado y
// después pagina /employees/{id}/holidays/{year}. No escribe nada.
func fetchHolidays(ctx context.Context, auth sesameAuth, email string, now time.Time) ([]holidayEntry, error) {
	if auth.Token == "" || auth.APIBase == "" {
		return nil, fmt.Errorf("sesión sin token o sin host de API")
	}
	if auth.EmployeeID == "" || auth.CompanyID == "" {
		id, company, err := resolveEmployee(ctx, auth, email)
		if err != nil {
			return nil, err
		}
		auth.EmployeeID = id
		auth.CompanyID = company
	}

	year := now.Year()
	all := make([]holidayEntry, 0, 32)
	for page := 1; page <= maxHolidayPages; page++ {
		p, err := fetchHolidayPage(ctx, auth, year, page)
		if err != nil {
			return nil, err
		}
		all = append(all, p.Data...)
		if p.Meta.LastPage <= page {
			break
		}
	}
	return all, nil
}

func resolveEmployee(ctx context.Context, auth sesameAuth, email string) (string, string, error) {
	body, err := sesameGet(ctx, auth, "/api/v3/security/me", false)
	if err != nil {
		return "", "", fmt.Errorf("consultar empleado: %w", err)
	}
	entries, err := parseMe(body)
	if err != nil {
		return "", "", err
	}
	id, company, err := pickEmployee(entries, email)
	if err != nil {
		return "", "", fmt.Errorf("resolver empleado: %w", err)
	}
	return id, company, nil
}

func fetchHolidayPage(ctx context.Context, auth sesameAuth, year, page int) (holidaysPage, error) {
	path := fmt.Sprintf("/api/v3/employees/%s/holidays/%d?page=%d",
		url.PathEscape(auth.EmployeeID), year, page)
	body, err := sesameGet(ctx, auth, path, true)
	if err != nil {
		return holidaysPage{}, err
	}
	var p holidaysPage
	if err := json.Unmarshal(body, &p); err != nil {
		return holidaysPage{}, fmt.Errorf("parsear festivos (página %d): %w", page, err)
	}
	return p, nil
}

func sesameGet(ctx context.Context, auth sesameAuth, path string, withIdentity bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, auth.APIBase+path, nil)
	if err != nil {
		return nil, fmt.Errorf("crear petición: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "es-ES,es;q=0.9")
	req.Header.Set("User-Agent", sesameBrowserUA)
	origin := originOf(loginURL)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	for k, v := range auth.ExtraHeaders {
		req.Header.Set(k, v)
	}
	if withIdentity {
		if auth.EmployeeID != "" {
			req.Header.Set("Esid", auth.EmployeeID)
		}
		if auth.CompanyID != "" {
			req.Header.Set("Csid", auth.CompanyID)
		}
	}

	resp, err := sesameHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("leer respuesta de %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s: HTTP %d: %.200s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func parseMe(body []byte) ([]meEntry, error) {
	var env meEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Data != nil {
		return env.Data, nil
	}
	var arr []meEntry
	if err := json.Unmarshal(body, &arr); err == nil {
		return arr, nil
	}
	return nil, fmt.Errorf("respuesta de security/me no reconocida")
}

func pickEmployee(entries []meEntry, wantedEmail string) (string, string, error) {
	var firstID, firstCompany string
	found := false

	for i := range entries {
		entry := &entries[i]
		id, candidateEmail, company := "", "", ""
		if entry.Employee != nil {
			id = entry.Employee.ID
			candidateEmail = entry.Employee.Email
			company = companyID(entry.Employee)
		}
		// security/me mezcla datos en la entrada y dentro de employee según la
		// versión de la SPA: se aceptan ambos.
		if id == "" {
			id = entry.ID
		}
		if candidateEmail == "" {
			candidateEmail = entry.Email
		}
		if company == "" {
			company = entryCompanyID(entry)
		}
		if id == "" {
			continue
		}
		if !found {
			firstID, firstCompany = id, company
			found = true
		}
		if wantedEmail != "" && strings.EqualFold(candidateEmail, wantedEmail) {
			return id, company, nil
		}
	}
	if found {
		return firstID, firstCompany, nil
	}
	return "", "", fmt.Errorf("security/me no devolvió ningún empleado")
}

func companyID(emp *meEmployee) string {
	if emp == nil {
		return ""
	}
	if emp.CompanyID != "" {
		return emp.CompanyID
	}
	if emp.MyCompany != nil && emp.MyCompany.ID != "" {
		return emp.MyCompany.ID
	}
	return firstCompanyID(emp.Companies, emp.AccessibleCompanies)
}

func entryCompanyID(entry *meEntry) string {
	if entry == nil {
		return ""
	}
	if entry.CompanyID != "" {
		return entry.CompanyID
	}
	if entry.MyCompany != nil && entry.MyCompany.ID != "" {
		return entry.MyCompany.ID
	}
	return firstCompanyID(entry.Companies, entry.AccessibleCompanies)
}

func firstCompanyID(groups ...[]meCompany) string {
	for _, group := range groups {
		for _, c := range group {
			if c.ID != "" {
				return c.ID
			}
		}
	}
	return ""
}

func holidayOn(entries []holidayEntry, today string) (string, bool) {
	for _, h := range entries {
		if h.Date == today {
			return h.Name, true
		}
	}
	return "", false
}

func nextHoliday(entries []holidayEntry, today string) *holidayEntry {
	var next *holidayEntry
	for i := range entries {
		if entries[i].Date <= today {
			continue
		}
		if next == nil || entries[i].Date < next.Date {
			next = &entries[i]
		}
	}
	return next
}

// looksLikeToken descarta bodies HTML o valores que no son el JWT de la API.
// El token de Sesame es un JWT con tres segmentos separados por puntos.
func looksLikeToken(token string) bool {
	if len(token) < 20 || strings.Count(token, ".") != 2 {
		return false
	}
	return !strings.ContainsAny(token, " \n\t\r<>\"'")
}

func authorizationToken(headers proto.NetworkHeaders) string {
	raw := headerValue(headers, "Authorization")
	if raw == "" {
		return ""
	}
	const bearer = "Bearer "
	if len(raw) > len(bearer) && strings.EqualFold(raw[:len(bearer)], bearer) {
		raw = raw[len(bearer):]
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "Bearer") {
		return ""
	}
	return raw
}

func headerValue(headers proto.NetworkHeaders, name string) string {
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return strings.TrimSpace(v.Str())
		}
	}
	return ""
}

func extractExtraHeaders(headers proto.NetworkHeaders) map[string]string {
	out := make(map[string]string, len(sesameForwardHeaders))
	for _, name := range sesameForwardHeaders {
		if v := headerValue(headers, name); v != "" {
			out[http.CanonicalHeaderKey(name)] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func apiOrigin(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
