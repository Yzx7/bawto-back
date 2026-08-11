package connectors

import "testing"

func TestValidateTargetAceptaElHostDelDriver(t *testing.T) {
	if err := ValidateTarget(DriverMeudim, "https://api.meud.im"); err != nil {
		t.Fatalf("el host oficial fue rechazado: %v", err)
	}
	if err := ValidateTarget(DriverMeudim, "https://api.meud.im/"); err != nil {
		t.Fatalf("la barra final fue rechazada: %v", err)
	}
	if got := DefaultBaseURL(DriverMeudim); got != "https://api.meud.im" {
		t.Fatalf("URL por defecto inesperada: %q", got)
	}
}

// Cada caso de esta tabla es una forma distinta de convertir el conector en un
// vector SSRF o de filtrar la credencial. Se comprueban juntos porque quien
// añada un driver nuevo tiene que seguir pasándolos todos.
func TestValidateTargetRechazaDestinosPeligrosos(t *testing.T) {
	casos := []struct {
		nombre  string
		driver  string
		baseURL string
	}{
		{"driver desconocido", "shopify", "https://api.meud.im"},
		{"host ajeno", DriverMeudim, "https://evil.example.com"},
		{"subdominio parecido", DriverMeudim, "https://api.meud.im.evil.example.com"},
		{"sin TLS", DriverMeudim, "http://api.meud.im"},
		{"loopback", DriverMeudim, "https://127.0.0.1:3009"},
		{"red interna", DriverMeudim, "https://10.0.0.5"},
		{"metadatos de nube", DriverMeudim, "https://169.254.169.254"},
		{"credenciales en la URL", DriverMeudim, "https://user:pass@api.meud.im"},
		{"query pegada", DriverMeudim, "https://api.meud.im?x=1"},
		{"vacía", DriverMeudim, ""},
	}
	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			if err := ValidateTarget(caso.driver, caso.baseURL); err == nil {
				t.Fatalf("se aceptó un destino que no debía: %q", caso.baseURL)
			}
		})
	}
}

// La regla que impide que el bot acabe con acceso total a la tienda. Vive en el
// driver y no en quien guarda la conexión porque es conocimiento de Meudim: el
// prefijo distingue publicable de secreta.
func TestValidateCredentialRechazaLaClaveSecreta(t *testing.T) {
	if err := ValidateCredential(DriverMeudim, "pk_live_abc"); err != nil {
		t.Fatalf("una clave publicable fue rechazada: %v", err)
	}
	if err := ValidateCredential(DriverMeudim, "pk_test_abc"); err != nil {
		t.Fatalf("una clave publicable de pruebas fue rechazada: %v", err)
	}
	for _, credential := range []string{"sk_live_abc", "sk_test_abc", "abc", "  "} {
		if err := ValidateCredential(DriverMeudim, credential); err == nil {
			t.Errorf("se aceptó la credencial %q", credential)
		}
	}
}
