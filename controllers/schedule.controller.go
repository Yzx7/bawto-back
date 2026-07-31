package controllers

import (
	"time"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/scheduler"
	"github.com/gofiber/fiber/v2"
)

// QueueScheduledFlow genera runs para la vista del trigger schedule. Es seguro
// llamarlo más de una vez: la clave diaria evita duplicados por registro/contacto.
func (con *Controller) QueueScheduledFlow(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	flows, err := models.ListPublishedScheduleFlows(c.Context(), con.Env.Postgres)
	if err != nil {
		return con.failErr(c, err)
	}
	var scheduled *models.ScheduledFlow
	for i := range flows {
		if flows[i].BotID == bot.ID {
			scheduled = &flows[i]
			break
		}
	}
	if scheduled == nil {
		return con.fail(c, 400, "el bot no tiene un flujo schedule publicado")
	}
	report, err := scheduler.QueueFlow(c.Context(), con.Env.Postgres, *scheduled, time.Now().UTC().Truncate(time.Minute))
	if err != nil {
		return con.fail(c, 400, err.Error())
	}
	return con.ok(c, "ejecuciones encoladas", report)
}
