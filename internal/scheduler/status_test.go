package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestStatusFor fija el contrato de cómo una ejecución se traduce a estado y
// mensaje. El caso clave es ErrLogoutFailed: el fichaje se realizó y sólo falló
// el cierre de sesión, así que el run cuenta como correcto con aviso.
func TestStatusFor(t *testing.T) {
	cases := []struct {
		name       string
		locLabel   string
		err        error
		wantStatus string
		wantSubstr string
	}{
		{"sin error", "Oficina", nil, "ok", "Oficina"},
		{"fichaje omitido", "", ErrSkipped, "skipped", "skipped"},
		{"logout fallido pero fichaje hecho", "Oficina", ErrLogoutFailed{Err: errors.New("no se encontró el botón")}, "ok", "cierre de sesión"},
		{"error real", "", errors.New("campo email no encontrado"), "error", "campo email"},
	}
	for _, c := range cases {
		status, msg := statusFor(c.locLabel, c.err)
		if status != c.wantStatus {
			t.Errorf("%s: status = %q, quiero %q", c.name, status, c.wantStatus)
		}
		if c.wantSubstr != "" && !strings.Contains(msg, c.wantSubstr) {
			t.Errorf("%s: msg = %q, debe contener %q", c.name, msg, c.wantSubstr)
		}
	}
}

// TestTailLogs comprueba que el log que se devuelve al panel está acotado y que
// conserva las líneas más recientes, que son las que explican el fallo.
func TestTailLogs(t *testing.T) {
	var raw string
	for i := 0; i < 1000; i++ {
		raw += fmt.Sprintf("línea %d\n", i)
	}
	lines := tailLogs(raw)
	if len(lines) != maxLogLines {
		t.Errorf("esperaba %d líneas, obtuve %d", maxLogLines, len(lines))
	}
	if lines[len(lines)-1] != "línea 999" {
		t.Errorf("la última línea debería ser la más reciente: %q", lines[len(lines)-1])
	}
}
