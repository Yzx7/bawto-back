package controllers

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
	"github.com/gofiber/fiber/v2"
)

func (con *Controller) ListContacts(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"))
	if err != nil {
		return con.failErr(c, err)
	}
	contacts, err := models.ListContactsByBot(c.Context(), con.Env.Postgres, bot.ID)
	if err != nil {
		return con.fail(c, 500, "error obteniendo contactos")
	}
	return con.ok(c, "ok", contacts)
}

// ImportContactsCSV previsualiza o importa contactos de un CSV. La fila de
// encabezados requiere phone (o phoneColumn); las demás columnas se guardan en data.
func (con *Controller) ImportContactsCSV(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	file, err := c.FormFile("file")
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, "adjunta un archivo CSV en el campo file")
	}
	if !strings.HasSuffix(strings.ToLower(file.Filename), ".csv") {
		return con.fail(c, fiber.StatusBadRequest, "por ahora solo se aceptan archivos CSV")
	}
	src, err := file.Open()
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, "no se pudo abrir el archivo")
	}
	defer src.Close()
	report, err := models.ParseContactsCSV(src, c.FormValue("phoneColumn"), c.FormValue("nameColumn"), c.FormValue("statusColumn"))
	if err != nil {
		return con.fail(c, fiber.StatusBadRequest, err.Error())
	}
	preview, _ := strconv.ParseBool(c.FormValue("preview"))
	if preview {
		return con.ok(c, "previsualización lista", report)
	}
	imported := 0
	for _, row := range report.Rows {
		if row.Error != "" {
			continue
		}
		if _, err := models.UpsertContact(c.Context(), con.Env.Postgres, bot.ID, row.Phone, row.Name, row.Status, row.Data); err != nil {
			return con.fail(c, fiber.StatusInternalServerError, "la importación se interrumpió")
		}
		imported++
	}
	return con.ok(c, "contactos importados", fiber.Map{"imported": imported, "report": report})
}

func (con *Controller) UpsertContact(c *fiber.Ctx) error {
	bot, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member")
	if err != nil {
		return con.failErr(c, err)
	}
	var in struct {
		Phone, Name, Status string
		Data                json.RawMessage
	}
	if err := c.BodyParser(&in); err != nil || len(in.Data) > 0 && !json.Valid(in.Data) {
		return con.fail(c, 400, "contacto inválido")
	}
	if len(models.NormalizePhone(in.Phone)) < 6 {
		return con.fail(c, 400, "teléfono inválido")
	}
	contact, err := models.UpsertContact(c.Context(), con.Env.Postgres, bot.ID, in.Phone, strings.TrimSpace(in.Name), strings.TrimSpace(in.Status), in.Data)
	if err != nil {
		return con.fail(c, 500, "no se pudo guardar el contacto")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("contacto guardado", contact))
}

func (con *Controller) ListBilling(c *fiber.Ctx) error {
	if _, err := con.botWithRole(c, c.Params("botId")); err != nil {
		return con.failErr(c, err)
	}
	billing, err := models.ListBillingByContact(c.Context(), con.Env.Postgres, c.Params("botId"), c.Params("contactId"))
	if err != nil {
		return con.fail(c, 500, "error obteniendo cobros")
	}
	return con.ok(c, "ok", billing)
}

func (con *Controller) CreateBilling(c *fiber.Ctx) error {
	if _, err := con.botWithRole(c, c.Params("botId"), "owner", "admin", "member"); err != nil {
		return con.failErr(c, err)
	}
	var in struct{ Period, Amount, Currency, DueDate string }
	if err := c.BodyParser(&in); err != nil || strings.TrimSpace(in.Period) == "" || strings.TrimSpace(in.Amount) == "" || strings.TrimSpace(in.DueDate) == "" {
		return con.fail(c, 400, "cobro inválido")
	}
	billing, err := models.CreateBillingRecord(c.Context(), con.Env.Postgres, c.Params("botId"), c.Params("contactId"), strings.TrimSpace(in.Period), strings.TrimSpace(in.Amount), strings.TrimSpace(in.Currency), strings.TrimSpace(in.DueDate))
	if err != nil {
		return con.fail(c, 400, "no se pudo crear el cobro")
	}
	return c.Status(fiber.StatusCreated).JSON(types.OK("cobro creado", billing))
}
