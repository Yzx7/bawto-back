package controllers

import (
	"encoding/json"
	"errors"
	"github.com/Yzx7/sacs-chatbots/models"
	"github.com/Yzx7/sacs-chatbots/types"
	"github.com/gofiber/fiber/v2"
	"github.com/jackc/pgx/v5/pgconn"
	"strconv"
	"strings"
)

func orgDataWriteMessage(err error, fallback string) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fallback
	}
	switch pgErr.Code {
	case "23505":
		return "ya existe un objeto con esa clave en la organización"
	case "42703", "42P01":
		return "la base de datos requiere la migración de datos por organización"
	default:
		return fallback
	}
}

func orgContactWriteMessage(err error, fallback string) string {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fallback
	}
	switch pgErr.Code {
	case "23505":
		return "ya existe un contacto con ese teléfono en la organización"
	case "42703", "42P01":
		return "la base de datos requiere la migración de datos por organización"
	default:
		return fallback
	}
}

func (con *Controller) ListOrgDataObjects(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org); e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListDataObjectsByOrg(c.Context(), con.Env.Postgres, org)
	if e != nil {
		return con.fail(c, 500, "error obteniendo objetos")
	}
	return con.ok(c, "ok", v)
}
func (con *Controller) CreateOrgDataObject(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org, "owner", "admin", "member"); e != nil {
		return con.failErr(c, e)
	}
	var in struct{ Key, Name, PluralName string }
	if c.BodyParser(&in) != nil || strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.PluralName) == "" {
		return con.fail(c, 400, "objeto inválido")
	}
	v, e := models.CreateDataObjectByOrg(c.Context(), con.Env.Postgres, org, strings.TrimSpace(in.Key), strings.TrimSpace(in.Name), strings.TrimSpace(in.PluralName))
	if e != nil {
		return con.fail(c, 400, orgDataWriteMessage(e, "no se pudo crear el objeto"))
	}
	return c.Status(201).JSON(types.OK("objeto creado", v))
}
func (con *Controller) ListOrgDataFields(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org); e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListDataFieldsByOrg(c.Context(), con.Env.Postgres, org, c.Params("objectId"))
	if e != nil {
		return con.fail(c, 500, "error obteniendo campos")
	}
	return con.ok(c, "ok", v)
}
func (con *Controller) UpsertOrgDataField(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org, "owner", "admin", "member"); e != nil {
		return con.failErr(c, e)
	}
	var in struct {
		Key, Label, Type string
		Required         bool
	}
	if c.BodyParser(&in) != nil || strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Label) == "" {
		return con.fail(c, 400, "campo inválido")
	}
	v, e := models.UpsertDataFieldByOrg(c.Context(), con.Env.Postgres, org, c.Params("objectId"), strings.TrimSpace(in.Key), strings.TrimSpace(in.Label), strings.TrimSpace(in.Type), in.Required)
	if e != nil {
		return con.fail(c, 400, "no se pudo guardar el campo")
	}
	return c.Status(201).JSON(types.OK("campo guardado", v))
}
func (con *Controller) ListOrgDataRecords(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org); e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListDataRecordsByOrg(c.Context(), con.Env.Postgres, org, c.Params("objectId"))
	if e != nil {
		return con.fail(c, 500, "error obteniendo registros")
	}
	return con.ok(c, "ok", v)
}
func (con *Controller) CreateOrgDataRecord(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org, "owner", "admin", "member"); e != nil {
		return con.failErr(c, e)
	}
	body := c.Body()
	if !json.Valid(body) {
		return con.fail(c, 400, "registro inválido")
	}
	v, e := models.CreateDataRecordByOrg(c.Context(), con.Env.Postgres, org, c.Params("objectId"), json.RawMessage(body))
	if e != nil {
		return con.fail(c, 400, e.Error())
	}
	return c.Status(201).JSON(types.OK("registro creado", v))
}
func (con *Controller) ListOrgContacts(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org); e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListContactsByOrg(c.Context(), con.Env.Postgres, org)
	if e != nil {
		return con.fail(c, 500, "error obteniendo contactos")
	}
	return con.ok(c, "ok", v)
}

func (con *Controller) SaveOrgContact(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org, "owner", "admin", "member"); e != nil {
		return con.failErr(c, e)
	}
	var in struct {
		Phone, Name, Status string
		Data                json.RawMessage
	}
	if e := c.BodyParser(&in); e != nil || len(in.Data) > 0 && !json.Valid(in.Data) {
		return con.fail(c, 400, "contacto inválido")
	}
	v, e := models.SaveContactByOrg(c.Context(), con.Env.Postgres, org, c.Params("contactId"), in.Phone, strings.TrimSpace(in.Name), strings.TrimSpace(in.Status), in.Data)
	if e != nil {
		return con.fail(c, 400, orgContactWriteMessage(e, e.Error()))
	}
	return c.Status(201).JSON(types.OK("contacto guardado", v))
}

func (con *Controller) ListOrgContactFields(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org); e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListContactFieldsByOrg(c.Context(), con.Env.Postgres, org)
	if e != nil {
		return con.fail(c, 500, "error obteniendo campos de contacto")
	}
	return con.ok(c, "ok", v)
}

func (con *Controller) UpsertOrgContactField(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org, "owner", "admin", "member"); e != nil {
		return con.failErr(c, e)
	}
	var in struct {
		Key, Label, Type string
		Required         bool
	}
	if c.BodyParser(&in) != nil || strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Label) == "" {
		return con.fail(c, 400, "campo inválido")
	}
	v, e := models.UpsertContactFieldByOrg(c.Context(), con.Env.Postgres, org, strings.TrimSpace(in.Key), strings.TrimSpace(in.Label), strings.TrimSpace(in.Type), in.Required)
	if e != nil {
		return con.fail(c, 400, "no se pudo guardar el campo")
	}
	return c.Status(201).JSON(types.OK("campo guardado", v))
}

func (con *Controller) ImportOrgContactsCSV(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org, "owner", "admin", "member"); e != nil {
		return con.failErr(c, e)
	}
	file, e := c.FormFile("file")
	if e != nil {
		return con.fail(c, 400, "adjunta un archivo CSV en el campo file")
	}
	if !strings.HasSuffix(strings.ToLower(file.Filename), ".csv") {
		return con.fail(c, 400, "por ahora solo se aceptan archivos CSV")
	}
	src, e := file.Open()
	if e != nil {
		return con.fail(c, 400, "no se pudo abrir el archivo")
	}
	defer src.Close()
	report, e := models.ParseContactsCSV(src, c.FormValue("phoneColumn"), c.FormValue("nameColumn"), c.FormValue("statusColumn"))
	if e != nil {
		return con.fail(c, 400, e.Error())
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
		if _, e = models.SaveContactByOrg(c.Context(), con.Env.Postgres, org, "", row.Phone, row.Name, row.Status, row.Data); e != nil {
			return con.fail(c, 400, "la importación se interrumpió: "+e.Error())
		}
		imported++
	}
	return con.ok(c, "contactos importados", fiber.Map{"imported": imported, "report": report})
}
func (con *Controller) LinkOrgDataRecordContact(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org, "owner", "admin", "member"); e != nil {
		return con.failErr(c, e)
	}
	if e := models.LinkRecordContactByOrg(c.Context(), con.Env.Postgres, org, c.Params("recordId"), c.Params("contactId"), strings.TrimSpace(c.Query("role", "primary"))); e != nil {
		return con.fail(c, 400, e.Error())
	}
	return con.ok(c, "relación creada", nil)
}
func (con *Controller) ListOrgDataViews(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org); e != nil {
		return con.failErr(c, e)
	}
	v, e := models.ListDataViewsByOrg(c.Context(), con.Env.Postgres, org, c.Params("objectId"))
	if e != nil {
		return con.fail(c, 500, "error obteniendo vistas")
	}
	return con.ok(c, "ok", v)
}
func (con *Controller) CreateOrgDataView(c *fiber.Ctx) error {
	org := c.Params("orgId")
	if _, e := con.requireOrgRole(c, org, "owner", "admin", "member"); e != nil {
		return con.failErr(c, e)
	}
	var in struct {
		Name   string
		Filter json.RawMessage
	}
	if c.BodyParser(&in) != nil || strings.TrimSpace(in.Name) == "" || (len(in.Filter) > 0 && !json.Valid(in.Filter)) {
		return con.fail(c, 400, "vista inválida")
	}
	v, e := models.CreateDataViewByOrg(c.Context(), con.Env.Postgres, org, c.Params("objectId"), strings.TrimSpace(in.Name), in.Filter)
	if e != nil {
		return con.fail(c, 400, "no se pudo crear la vista")
	}
	return c.Status(201).JSON(types.OK("vista creada", v))
}
