package connectors

import (
	"strings"
	"testing"
)

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
// DriverDatasetAPI no tiene host en código: lo entrega el equipo del hackatón
// por variable de entorno. Sin ella, el fallo debe ser seguro (ninguna
// conexión se guarda), no "cualquier host vale".
func TestValidateTargetDatasetAPISinVariableDeEntornoRechazaTodo(t *testing.T) {
	t.Setenv(datasetAPIAllowedHostsEnv, "")
	if err := ValidateTarget(DriverDatasetAPI, "https://cualquier-cosa.example.com"); err == nil {
		t.Fatal("sin DATASET_API_ALLOWED_HOSTS no debía aceptarse ningún host")
	}
	// El driver existe (a diferencia de uno inventado): el error debe hablar
	// del host, no de un driver desconocido.
	err := ValidateTarget(DriverDatasetAPI, "https://cualquier-cosa.example.com")
	if err == nil {
		t.Fatal("se esperaba rechazo")
	}
	if got := err.Error(); !strings.Contains(got, "host") {
		t.Fatalf("se esperaba un error sobre el host, llegó: %q", got)
	}
}

// Declarado el host por variable de entorno, ValidateTarget lo acepta y sigue
// rechazando cualquier otro — la misma barrera que Meudim, con la fuente de
// la lista movida a runtime porque el host no se conoce en tiempo de
// compilación.
func TestValidateTargetDatasetAPIConVariableDeEntorno(t *testing.T) {
	t.Setenv(datasetAPIAllowedHostsEnv, "api.equipo-hackaton.example.com, otro.example.com")
	if err := ValidateTarget(DriverDatasetAPI, "https://api.equipo-hackaton.example.com"); err != nil {
		t.Fatalf("el host declarado por variable de entorno fue rechazado: %v", err)
	}
	if err := ValidateTarget(DriverDatasetAPI, "https://otro.example.com"); err != nil {
		t.Fatalf("el segundo host declarado fue rechazado: %v", err)
	}
	if err := ValidateTarget(DriverDatasetAPI, "https://sin-declarar.example.com"); err == nil {
		t.Fatal("un host no declarado no debía aceptarse")
	}
	// La misma protección SSRF de siempre: exigir https y rechazar loopback.
	if err := ValidateTarget(DriverDatasetAPI, "http://api.equipo-hackaton.example.com"); err == nil {
		t.Fatal("sin TLS no debía aceptarse")
	}
}

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
