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
	"slices"
	"sort"
	"strings"
)

// DriverMeudim es el backend de e-commerce headless (api.meud.im).
const DriverMeudim = "meudim"

// allowedHosts es la lista permitida por driver. Un driver nuevo entra aquí con
// sus hosts; no basta con implementar su cliente.
var allowedHosts = map[string][]string{
	DriverMeudim: {"api.meud.im"},
}

// Drivers devuelve los drivers conocidos en orden estable, para el panel.
func Drivers() []string {
	out := make([]string, 0, len(allowedHosts))
	for driver := range allowedHosts {
		out = append(out, driver)
	}
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
	hosts, known := allowedHosts[driver]
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

// DefaultBaseURL es la URL con la que se crea una conexión cuando el
// administrador no escribe ninguna.
func DefaultBaseURL(driver string) string {
	if driver == DriverMeudim {
		return "https://api.meud.im"
	}
	return ""
}
