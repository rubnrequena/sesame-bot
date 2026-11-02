package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseLoginToken(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{"envoltorio data", `{"data":"jwt-token"}`, "jwt-token", false},
		{"envoltorio token", `{"token":"jwt-token"}`, "jwt-token", false},
		{"json string", `"jwt-token"`, "jwt-token", false},
		{"crudo", `jwt-token`, "jwt-token", false},
		{"vacío", ``, "", true},
		{"no reconocido", `{"foo":"bar"}`, "", true},
	}
	for _, c := range cases {
		got, err := parseLoginToken([]byte(c.body))
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: esperaba error, obtuve %q", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: error inesperado: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPickEmployee(t *testing.T) {
	entries := []meEntry{
		{Employee: &meEmployee{ID: "e1", Email: "one@example.com", MyCompany: &meCompany{ID: "c1"}}},
		{Employee: &meEmployee{ID: "e2", Email: "two@example.com", Companies: []meCompany{{ID: "c2"}}}},
		{Employee: &meEmployee{ID: "e3", Email: "three@example.com"}, MyCompany: &meCompany{ID: "c3"}},
		{Employee: &meEmployee{ID: "e4", Email: "four@example.com", AccessibleCompanies: []meCompany{{ID: "c4"}}}},
		{ID: "e5", Email: "five@example.com", CompanyID: "c5"},
	}

	id, company, err := pickEmployee(entries, "TWO@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id != "e2" || company != "c2" {
		t.Errorf("match por email: got %q/%q, want e2/c2", id, company)
	}

	id, company, err = pickEmployee(entries, "missing@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id != "e1" || company != "c1" {
		t.Errorf("fallback: got %q/%q, want e1/c1", id, company)
	}

	id, company, err = pickEmployee(entries, "three@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id != "e3" || company != "c3" {
		t.Errorf("company en la entrada: got %q/%q, want e3/c3", id, company)
	}

	id, company, err = pickEmployee(entries, "five@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id != "e5" || company != "c5" {
		t.Errorf("entrada plana: got %q/%q, want e5/c5", id, company)
	}

	if _, _, err := pickEmployee(nil, "x@example.com"); err == nil {
		t.Error("esperaba error con lista vacía")
	}
}

func TestParseMe(t *testing.T) {
	enveloped := []byte(`{"data":[{"employee":{"id":"e1","email":"one@example.com","myCompany":{"id":"c1"}}}]}`)
	entries, err := parseMe(enveloped)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Employee == nil || entries[0].Employee.ID != "e1" {
		t.Fatalf("parseo con envoltorio inesperado: %+v", entries)
	}

	direct := []byte(`[{"id":"e2","email":"two@example.com","myCompany":{"id":"c2"}}]`)
	entries, err = parseMe(direct)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "e2" {
		t.Fatalf("parseo directo inesperado: %+v", entries)
	}

	if _, err := parseMe([]byte(`no-json`)); err == nil {
		t.Error("esperaba error con body inválido")
	}
}

func TestHolidayOnAndNext(t *testing.T) {
	entries := []holidayEntry{
		{Date: "2026-01-01", Name: "Año Nuevo"},
		{Date: "2026-11-02", Name: "Día siguiente a Todos los Santos"},
		{Date: "2026-12-25", Name: "Natividad del Señor"},
	}

	name, ok := holidayOn(entries, "2026-11-02")
	if !ok || name != "Día siguiente a Todos los Santos" {
		t.Errorf("holidayOn: got %q/%t", name, ok)
	}
	if _, ok := holidayOn(entries, "2026-11-03"); ok {
		t.Error("2026-11-03 no es festivo")
	}

	next := nextHoliday(entries, "2026-11-02")
	if next == nil || next.Date != "2026-12-25" {
		t.Errorf("nextHoliday: got %+v", next)
	}
	if next := nextHoliday(entries, "2026-12-31"); next != nil {
		t.Errorf("no debería haber siguiente: %+v", next)
	}
}

func TestFetchHolidaysPaginatesAndSendsIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Esid"); got != "e1" {
			t.Errorf("Esid = %q", got)
		}
		if got := r.Header.Get("Csid"); got != "c1" {
			t.Errorf("Csid = %q", got)
		}
		if r.URL.Path != "/api/v3/employees/e1/holidays/2026" {
			t.Errorf("path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			fmt.Fprint(w, `{"data":[{"date":"2026-01-01","name":"Año Nuevo"}],"meta":{"currentPage":1,"lastPage":2}}`)
		case "2":
			fmt.Fprint(w, `{"data":[{"date":"2026-11-02","name":"Día siguiente a Todos los Santos"}],"meta":{"currentPage":2,"lastPage":2}}`)
		default:
			t.Errorf("page = %q", page)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	auth := sesameAuth{
		Token:      "test-token",
		APIBase:    server.URL,
		EmployeeID: "e1",
		CompanyID:  "c1",
	}
	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	holidays, err := fetchHolidays(context.Background(), auth, "one@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(holidays) != 2 {
		t.Fatalf("esperaba 2 festivos, obtuve %d: %+v", len(holidays), holidays)
	}
	if name, ok := holidayOn(holidays, "2026-11-02"); !ok || !strings.Contains(name, "Todos los Santos") {
		t.Errorf("festivo de hoy no encontrado: %q/%t", name, ok)
	}
}

func TestFetchHolidaysHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	auth := sesameAuth{
		Token:      "test-token",
		APIBase:    server.URL,
		EmployeeID: "e1",
		CompanyID:  "c1",
	}
	_, err := fetchHolidays(context.Background(), auth, "", time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("esperaba error 401, obtuve %v", err)
	}
}

func TestResolveEmployee(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/security/me" {
			t.Errorf("path = %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, `{"data":[
			{"employee":{"id":"e1","email":"one@example.com","myCompany":{"id":"c1"}}},
			{"employee":{"id":"e2","email":"two@example.com","myCompany":{"id":"c2"}}}
		]}`)
	}))
	defer server.Close()

	id, company, err := resolveEmployee(context.Background(),
		sesameAuth{Token: "test-token", APIBase: server.URL}, "two@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if id != "e2" || company != "c2" {
		t.Fatalf("got %q/%q, want e2/c2", id, company)
	}
}

func TestAPITokenAndOrigin(t *testing.T) {
	if got := apiOrigin("https://back-eu5.sesametime.com/api/v3/x"); got != "https://back-eu5.sesametime.com" {
		t.Errorf("apiOrigin = %q", got)
	}
	if got := apiOrigin("/api/v3/x"); got != "" {
		t.Errorf("apiOrigin relativo = %q", got)
	}
	if !looksLikeToken("eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.firma") {
		t.Error("un JWT válido no debería rechazarse")
	}
	if looksLikeToken("<html>login</html>") {
		t.Error("HTML no debería aceptarse como token")
	}
}
