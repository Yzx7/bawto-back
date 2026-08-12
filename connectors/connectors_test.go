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

// Meudim distingue por prefijo las claves de navegador y las de servidor. La
// conexión de Bawto es servidor a servidor, por lo que no debe aceptar una
// publicable aunque hoy algunos endpoints admitan ambas.
func TestValidateCredentialExigeLaClaveSecreta(t *testing.T) {
	if err := ValidateCredential(DriverMeudim, "sk_live_abc"); err != nil {
		t.Fatalf("una clave secreta fue rechazada: %v", err)
	}
	if err := ValidateCredential(DriverMeudim, "sk_test_abc"); err != nil {
		t.Fatalf("una clave secreta de pruebas fue rechazada: %v", err)
	}
	for _, credential := range []string{"pk_live_abc", "pk_test_abc", "abc", "  "} {
		if err := ValidateCredential(DriverMeudim, credential); err == nil {
			t.Errorf("se aceptó la credencial %q", credential)
		}
	}
}
