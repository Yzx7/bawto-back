package routes

import (
	"github.com/gofiber/fiber/v2"

	"github.com/Yzx7/sacs-chatbots/controllers"
	"github.com/Yzx7/sacs-chatbots/env"
	authmw "github.com/Yzx7/sacs-chatbots/middlewares/auth"
	"github.com/Yzx7/sacs-chatbots/types"
)

// RegisterHTTP declara los endpoints y su cadena de middlewares.
//
// Convención (igual a meud): las rutas de app se sirven en la raíz y el
// frontend las consume vía su proxy `/api/[...path]` → Go `/<path>`, inyectando
// el JWT en el header x-access-token. Los webhooks los llama el proveedor
// (Meta) directo al backend, por eso van en su propio prefijo público.
func RegisterHTTP(app *fiber.App, e *env.Env) {
	authmw.Configure(e)
	con := controllers.New(e)

	// ---- Perfil (claims del JWT) --------------------------------
	app.Get("/me", authmw.VerifyToken, func(c *fiber.Ctx) error {
		claims, _ := authmw.Current(c)
		return c.JSON(types.OK("ok", claims))
	})

	// ---- Organizaciones y miembros (todo protegido) -------------
	org := app.Group("/orgs", authmw.VerifyToken)
	org.Get("/", con.GetMyOrganizations)
	org.Post("/", con.CreateOrganization)
	org.Put("/:orgId", con.UpdateOrganization)
	org.Delete("/:orgId", con.DeleteOrganization)
	org.Get("/:orgId/members", con.GetOrgMembers)
	org.Post("/:orgId/members", con.AddOrgMember)
	org.Put("/:orgId/members/:memberId/role", con.UpdateMemberRole)
	org.Delete("/:orgId/members/:memberId", con.RemoveMember)
	org.Get("/:orgId/bots", con.GetOrgBots)
	org.Post("/:orgId/bots", con.CreateBot)
	org.Get("/:orgId/data-objects", con.ListOrgDataObjects)
	org.Post("/:orgId/data-objects", con.CreateOrgDataObject)
	org.Get("/:orgId/data-objects/:objectId/fields", con.ListOrgDataFields)
	org.Post("/:orgId/data-objects/:objectId/fields", con.UpsertOrgDataField)
	org.Get("/:orgId/data-objects/:objectId/records", con.ListOrgDataRecords)
	org.Post("/:orgId/data-objects/:objectId/records", con.CreateOrgDataRecord)
	org.Get("/:orgId/contacts", con.ListOrgContacts)
	org.Post("/:orgId/contacts", con.SaveOrgContact)
	org.Put("/:orgId/contacts/:contactId", con.SaveOrgContact)
	org.Post("/:orgId/contacts/import", con.ImportOrgContactsCSV)
	org.Get("/:orgId/contact-fields", con.ListOrgContactFields)
	org.Post("/:orgId/contact-fields", con.UpsertOrgContactField)
	org.Post("/:orgId/data-records/:recordId/contacts/:contactId", con.LinkOrgDataRecordContact)
	org.Get("/:orgId/data-objects/:objectId/views", con.ListOrgDataViews)
	org.Post("/:orgId/data-objects/:objectId/views", con.CreateOrgDataView)

	// ---- Bots (protegido; autz por membresía en la org dueña) ----
	bots := app.Group("/bots", authmw.VerifyToken)
	bots.Get("/:botId", con.GetBot)
	bots.Put("/:botId", con.UpdateBot)
	bots.Delete("/:botId", con.DeleteBot)
	bots.Get("/:botId/flow", con.GetBotFlow)
	bots.Put("/:botId/flow", con.UpdateBotFlow)
	bots.Get("/:botId/channel", con.GetBotChannel)
	bots.Put("/:botId/channel", con.ConnectBotChannel)
	bots.Post("/:botId/channel/embedded", con.ConnectBotChannelEmbedded)
	bots.Post("/:botId/channel/register", con.RegisterBotChannel)
	bots.Get("/:botId/contacts", con.ListContacts)
	bots.Post("/:botId/contacts", con.UpsertContact)
	bots.Post("/:botId/contacts/import", con.ImportContactsCSV)
	bots.Get("/:botId/contacts/:contactId/billing", con.ListBilling)
	bots.Post("/:botId/contacts/:contactId/billing", con.CreateBilling)
	bots.Get("/:botId/contact-fields", con.ListContactFields)
	bots.Post("/:botId/contact-fields", con.UpsertContactField)
	bots.Get("/:botId/audiences", con.ListAudiences)
	bots.Post("/:botId/audiences", con.CreateAudience)
	bots.Get("/:botId/audiences/:audienceId/contacts", con.ResolveAudience)
	bots.Post("/:botId/audiences/:audienceId/contacts/:contactId", con.AddAudienceContact)
	bots.Get("/:botId/data-objects", con.ListDataObjects)
	bots.Post("/:botId/data-objects", con.CreateDataObject)
	bots.Get("/:botId/data-objects/:objectId/fields", con.ListDataFields)
	bots.Post("/:botId/data-objects/:objectId/fields", con.UpsertDataField)
	bots.Get("/:botId/data-objects/:objectId/records", con.ListDataRecords)
	bots.Post("/:botId/data-objects/:objectId/records", con.CreateDataRecord)
	bots.Get("/:botId/data-objects/:objectId/views", con.ListDataViews)
	bots.Post("/:botId/data-objects/:objectId/views", con.CreateDataView)
	bots.Get("/:botId/data-views/:viewId/records", con.ResolveDataView)
	bots.Post("/:botId/data-records/:recordId/contacts/:contactId", con.LinkDataRecordContact)
	bots.Post("/:botId/schedule/queue", con.QueueScheduledFlow)

	// ---- Webhook WhatsApp (Meta): único, ruteo por phone_number_id ----
	webhook := app.Group("/webhook")
	webhook.Get("/whatsapp", con.WhatsAppVerify)
	webhook.Post("/whatsapp", con.WhatsAppWebhook)
}
