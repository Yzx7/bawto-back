package controllers

import (
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/connectors/meudprov"
)

// Cada código de MEUD pide una reacción distinta y el panel las distingue por lo
// que devuelve este backend. Dos que no se pueden confundir:
//
//   - el 404 de admins es un mensaje escrito para el cliente («pídele que cree su
//     cuenta primero») y tiene que llegar entero, porque es la única instrucción
//     accionable de toda la respuesta;
//   - el 401 es la clave de máquina desincronizada con el `.env` de MEUD. Eso no
//     le dice nada a quien mira el asistente y no se arregla reintentando, así
//     que sale como 503 con un texto de operaciones.
func TestProvisionFailureDistingueLoQueLeeElClienteDeLoQueMiraOperaciones(t *testing.T) {
	casos := []struct {
		nombre  string
		err     error
		status  int
		mensaje string
	}{
		{
			nombre:  "correo sin cuenta en meud.im",
			err:     &meudprov.Error{StatusCode: http.StatusNotFound, Message: "Pídele que cree su cuenta primero."},
			status:  fiber.StatusNotFound,
			mensaje: "Pídele que cree su cuenta primero.",
		},
		{
			nombre: "clave de máquina inválida",
			err:    &meudprov.Error{StatusCode: http.StatusUnauthorized, Message: "unauthorized"},
			status: fiber.StatusServiceUnavailable,
		},
		{
			nombre: "aprovisionamiento sin configurar en MEUD",
			err:    &meudprov.Error{StatusCode: http.StatusServiceUnavailable, Message: "provisioning disabled"},
			status: fiber.StatusServiceUnavailable,
		},
		{
			nombre: "MEUD caído",
			err:    &meudprov.Error{StatusCode: http.StatusInternalServerError, Message: "boom"},
			status: fiber.StatusBadGateway,
		},
		{
			nombre: "sin respuesta",
			err:    &meudprov.Error{Unreachable: true, Message: "connection refused"},
			status: fiber.StatusBadGateway,
		},
	}
	for _, caso := range casos {
		status, mensaje := provisionFailure(caso.err)
		if status != caso.status {
			t.Errorf("%s: status = %d, esperado %d", caso.nombre, status, caso.status)
		}
		if caso.mensaje != "" && mensaje != caso.mensaje {
			t.Errorf("%s: mensaje = %q, esperado %q", caso.nombre, mensaje, caso.mensaje)
		}
		if mensaje == "" {
			t.Errorf("%s: se devolvió un mensaje vacío", caso.nombre)
		}
	}
}

// El 401 y el 503 delatan configuración del servidor. Reenviarlos tal cual le
// pondría delante a un cliente el estado del `.env` de otro producto, que ni
// entiende ni puede arreglar.
func TestProvisionFailureNoFiltraElDetalleDeLaCredencial(t *testing.T) {
	_, mensaje := provisionFailure(&meudprov.Error{
		StatusCode: http.StatusUnauthorized,
		Message:    "X-Provision-Key inválida para 127.0.0.1:8865",
	})
	if mensaje == "X-Provision-Key inválida para 127.0.0.1:8865" {
		t.Fatalf("el mensaje interno llegó al panel: %q", mensaje)
	}
}
