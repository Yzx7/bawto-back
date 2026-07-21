package controllers

import (
	"encoding/json"
	"time"

	"github.com/Yzx7/sacs-chatbots/engine"
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
	var flow engine.Flow
	if json.Unmarshal(bot.Flow, &flow) != nil || flow.Trigger.Type != "schedule" || flow.Trigger.ViewID == "" {
		return con.fail(c, 400, "el bot no tiene un flujo schedule con viewId")
	}
	report, err := scheduler.QueueFlow(c.Context(), con.Env.Postgres, bot, time.Now())
	if err != nil {
		return con.fail(c, 400, err.Error())
	}
	return con.ok(c, "ejecuciones encoladas", report)
}
