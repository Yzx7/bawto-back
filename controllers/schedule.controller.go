package controllers

import (
	"time"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/scheduler"
	"github.com/gofiber/fiber/v2"
)

// QueueBotFlowSchedule encola a mano las ejecuciones de **este** flujo, sin
// esperar al cron.
//
// Va colgado del flujo y no del bot porque la versión anterior listaba los
// flujos programados publicados y se quedaba con el primero: con dos
// recordatorios en el mismo bot no había forma de elegir, y el multiflujo hace
// que eso deje de ser un caso raro.
//
// Encolar **no es enviar**: el worker vuelve a comprobar en la entrega que el
// registro siga en la vista, que el contacto siga activo, el tope de
// recordatorios y el estado del chat. Repetirlo dentro del mismo minuto es
// idempotente por `run_key`.
func (con *Controller) QueueBotFlowSchedule(c *fiber.Ctx) error {
	bot, flow, err := con.flowWithRole(c, "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	if flow.TriggerType != "schedule" {
		return con.fail(c, fiber.StatusBadRequest, "solo se puede encolar un flujo programado")
	}
	scheduled, err := models.PublishedScheduleFlow(c.Context(), con.Env.Postgres, bot.ID, flow.ID)
	if err != nil {
		return con.failFlow(c, "PublishedScheduleFlow", bot.ID, err, "no se pudo leer el flujo")
	}
	if scheduled == nil {
		return con.fail(c, fiber.StatusConflict,
			"el flujo no está publicado: encolar usa la versión publicada, no el borrador")
	}
	report, err := scheduler.QueueFlow(c.Context(), con.Env.Postgres, *scheduled,
		time.Now().UTC().Truncate(time.Minute), "manual")
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	return con.ok(c, "ejecuciones encoladas", report)
}
