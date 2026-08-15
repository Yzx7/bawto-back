// Package connectors decide **a quién se le permite llamar**.
//
// Está separado de los clientes concretos a propósito: la comprobación tiene que
// ocurrir cuando se guarda la conexión, no cuando se abre. Si la lista viviera
// dentro del cliente, una conexión con una URL cualquiera se persistiría sin
// problema y el rechazo llegaría en mitad de una conversación, o no llegaría.
//
// El flujo nunca aporta una URL: el bloque referencia una conexión por su clave
// técnica y el destino sale de la fila. Esta lista es la segunda barrera, para
// que un administrador tampoco pueda apuntar una conexión a la red interna.
package connectors

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
)

// DriverMeudim es el backend de e-commerce headless (api.meud.im).
const DriverMeudim = "meudim"

// DriverDatasetAPI es un dataset genérico servido por HTTP: la tool
// `dataset_query` del §3 de PLAN-HACKATON-MARCA-BLANCA-Y-MCP.md. A diferencia
// de Meudim, su host no se conoce en tiempo de compilación —lo entrega el
// equipo del hackatón al desplegar su endpoint—, así que sus hosts no viven en
// `allowedHosts` sino en `datasetAPIAllowedHosts`, leídos de una variable de
// entorno. Ver esa función para el porqué.
const DriverDatasetAPI = "dataset_api"

// datasetAPIAllowedHostsEnv fija los hosts permitidos para DriverDatasetAPI,
// separados por coma. Vacía deja la lista vacía: ninguna conexión se guarda
// hasta declarar el host aquí, que es el fallo seguro mientras el endpoint del
// equipo no existe. Sigue sin ser el flujo ni el panel quien lo elige.
const datasetAPIAllowedHostsEnv = "DATASET_API_ALLOWED_HOSTS"

// allowedHosts es la lista permitida por driver, para los que se conocen en
// código. Un driver nuevo entra aquí con sus hosts; no basta con implementar
// su cliente. DriverDatasetAPI queda fuera a propósito (ver datasetAPIAllowedHosts).
var allowedHosts = map[string][]string{
	DriverMeudim: {"api.meud.im"},
}

// datasetAPIAllowedHosts lee la variable de entorno en cada llamada y no una
// sola vez al importar el paquete: los `var` de nivel de paquete corren antes
// de que `main` cargue el `.env` con godotenv, así que cachear el valor en la
// inicialización lo habría dejado vacío para siempre en un arranque normal.
func datasetAPIAllowedHosts() []string {
	raw := strings.TrimSpace(os.Getenv(datasetAPIAllowedHostsEnv))
	if raw == "" {
		return nil
	}
	var hosts []string
	for _, part := range strings.Split(raw, ",") {
		if host := strings.ToLower(strings.TrimSpace(part)); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// hostsForDriver resuelve los hosts permitidos de un driver, cualquiera sea su
// fuente. `known` distingue "el driver existe pero no tiene host configurado
// todavía" de "el driver no existe": la primera es un problema de despliegue
// que ValidateTarget explica con el host recibido; la segunda es la lista de
// drivers disponibles.
func hostsForDriver(driver string) (hosts []string, known bool) {
	if driver == DriverDatasetAPI {
		return datasetAPIAllowedHosts(), true
	}
	hosts, known = allowedHosts[driver]
	return hosts, known
}

// Drivers devuelve los drivers conocidos en orden estable, para el panel.
func Drivers() []string {
	out := make([]string, 0, len(allowedHosts)+1)
	for driver := range allowedHosts {
		out = append(out, driver)
	}
	out = append(out, DriverDatasetAPI)
	sort.Strings(out)
	return out
}

// ValidateTarget comprueba que el par (driver, URL) puede guardarse.
//
// Exige HTTPS sin excepción de loopback: una conexión que apunte a 127.0.0.1
// alcanzaría los servicios de la propia máquina —incluido este backend— y eso es
// exactamente el agujero que la lista intenta cerrar. Las pruebas de los clientes
// construyen el cliente directamente y no pasan por aquí, que es el sitio
// correcto para esa diferencia.
func ValidateTarget(driver, baseURL string) error {
	driver = strings.TrimSpace(driver)
	hosts, known := hostsForDriver(driver)
	if !known {
		return fmt.Errorf("driver %q no permitido; disponibles: %s", driver, strings.Join(Drivers(), ", "))
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return fmt.Errorf("la URL de la conexión es inválida: %w", err)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("la URL de la conexión debe ser https, no %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return fmt.Errorf("la URL de la conexión no puede llevar credenciales")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("la URL de la conexión no puede llevar query ni fragmento")
	}
	host := strings.ToLower(parsed.Hostname())
	if slices.Contains(hosts, host) {
		return nil
	}
	return fmt.Errorf("el host %q no está permitido para el driver %q; permitidos: %s",
		host, driver, strings.Join(hosts, ", "))
}

// ValidateCredential comprueba que la clave es del tipo que la conexión debe usar.
//
// Vive aquí y no en quien guarda la conexión porque es conocimiento del driver:
// Bawto habla con Meudim exclusivamente desde el servidor, así que exige una
// `sk_`. El alcance operativo no lo decide la credencial sino los métodos que el
// driver implementa y las herramientas que el runtime registra.
func ValidateCredential(driver, credential string) error {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return fmt.Errorf("la conexión requiere una credencial")
	}
	if driver == DriverMeudim && !strings.HasPrefix(credential, "sk_") {
		return fmt.Errorf("Meudim exige una clave secreta (sk_) para esta conexión servidor a servidor")
	}
	return nil
}

// DefaultBaseURL es la URL con la que se crea una conexión cuando el
// administrador no escribe ninguna.
func DefaultBaseURL(driver string) string {
	if driver == DriverMeudim {
		return "https://api.meud.im"
	}
	return ""
}
