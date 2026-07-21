// Package accesslog registra metadatos de cada petición HTTP.
package accesslog

import (
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Request deja una entrada al terminar cada petición. No registra query, body
// ni headers para no persistir tokens, credenciales o contenido de mensajes.
func Request(logger *slog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		started := time.Now()
		err := c.Next()
		status := c.Response().StatusCode()
		if err != nil {
			if fe, ok := err.(*fiber.Error); ok {
				status = fe.Code
			} else {
				status = fiber.StatusInternalServerError
			}
		}

		attrs := []any{
			"method", c.Method(),
			"path", c.Path(),
			"status", status,
			"duration", time.Since(started).String(),
			"ip", c.IP(),
		}
		if err != nil {
			attrs = append(attrs, "err", err.Error())
		}
		if status >= fiber.StatusInternalServerError {
			logger.Error("http request", attrs...)
		} else {
			logger.Info("http request", attrs...)
		}
		return err
	}
}
