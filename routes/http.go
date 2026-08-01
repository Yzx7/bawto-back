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
	org.Get("/:orgId/costs", con.GetOrganizationCosts)
	org.Get("/:orgId/billing", con.GetOrganizationBilling)
	org.Get("/:orgId/billing/statements/:statementId", con.GetOrganizationBillingStatement)
	org.Get("/:orgId/data-objects", con.ListOrgDataObjects)
	org.Post("/:orgId/data-objects", con.CreateOrgDataObject)
	org.Get("/:orgId/data-objects/:objectId/fields", con.ListOrgDataFields)
	org.Post("/:orgId/data-objects/:objectId/fields", con.UpsertOrgDataField)
	org.Get("/:orgId/data-objects/:objectId/records", con.ListOrgDataRecords)
	org.Post("/:orgId/data-objects/:objectId/records", con.CreateOrgDataRecord)
	org.Post("/:orgId/data-objects/:objectId/records/import", con.ImportOrgDataRecordsCSV)
	org.Get("/:orgId/contacts", con.ListOrgContacts)
	org.Post("/:orgId/contacts", con.SaveOrgContact)
	org.Put("/:orgId/contacts/:contactId", con.SaveOrgContact)
	org.Post("/:orgId/contacts/import", con.ImportOrgContactsCSV)
	org.Get("/:orgId/contact-fields", con.ListOrgContactFields)
	org.Post("/:orgId/contact-fields", con.UpsertOrgContactField)
	org.Post("/:orgId/data-records/:recordId/contacts/:contactId", con.LinkOrgDataRecordContact)
	org.Delete("/:orgId/data-records/:recordId/contacts/:contactId", con.UnlinkOrgDataRecordContact)
	org.Get("/:orgId/data-objects/:objectId/views", con.ListOrgDataViews)
	org.Post("/:orgId/data-objects/:objectId/views", con.CreateOrgDataView)
	org.Put("/:orgId/data-objects/:objectId/views/:viewId", con.UpdateOrgDataView)
	org.Delete("/:orgId/data-objects/:objectId/views/:viewId", con.DeleteOrgDataView)

	// ---- Bots (protegido; autz por membresía en la org dueña) ----
	bots := app.Group("/bots", authmw.VerifyToken)
	bots.Get("/:botId", con.GetBot)
	bots.Put("/:botId", con.UpdateBot)
	bots.Delete("/:botId", con.DeleteBot)
	bots.Get("/:botId/variables", con.ListFlowVariables)
	// Multiflujo (§10.1). Estas rutas son el único lugar donde se edita y publica
	// un grafo; el webhook ejecuta la versión publicada.
	bots.Get("/:botId/flows", con.ListBotFlows)
	bots.Post("/:botId/flows", con.CreateBotFlow)
	bots.Get("/:botId/flows/:flowId", con.GetBotFlowByID)
	bots.Patch("/:botId/flows/:flowId", con.UpdateBotFlowMeta)
	bots.Get("/:botId/flows/:flowId/draft", con.GetBotFlowDraft)
	bots.Put("/:botId/flows/:flowId/draft", con.UpdateBotFlowDraft)
	bots.Post("/:botId/flows/:flowId/validate", con.ValidateBotFlow)
	bots.Post("/:botId/flows/:flowId/publish", con.PublishBotFlow)
	bots.Post("/:botId/flows/:flowId/pause", con.PauseBotFlow)
	bots.Post("/:botId/flows/:flowId/resume", con.ResumeBotFlow)
	bots.Post("/:botId/flows/:flowId/archive", con.ArchiveBotFlow)
	bots.Get("/:botId/flows/:flowId/versions", con.ListBotFlowVersions)
	bots.Post("/:botId/flows/:flowId/versions/:versionId/restore", con.RestoreBotFlowVersion)
	bots.Post("/:botId/flows/:flowId/duplicate", con.DuplicateBotFlow)
	// Interfaz operativa (§10.2 y §10.3). El preview no escribe nada; el
	// historial y sus acciones se acotan siempre por bot.
	bots.Post("/:botId/flows/:flowId/schedule/preview", con.PreviewBotFlowSchedule)
	bots.Get("/:botId/flows/:flowId/occurrences", con.ListBotFlowOccurrences)
	bots.Post("/:botId/schedule/validate-cron", con.ValidateCronExpression)
	bots.Get("/:botId/flow-runs", con.ListBotFlowRuns)
	bots.Get("/:botId/costs", con.GetBotCosts)
	bots.Get("/:botId/flow-runs/:runId", con.GetBotFlowRun)
	bots.Post("/:botId/flow-runs/:runId/retry", con.RetryBotFlowRun)
	bots.Post("/:botId/flow-runs/:runId/cancel", con.CancelBotFlowRun)
	bots.Get("/:botId/channel", con.GetBotChannel)
	bots.Put("/:botId/channel", con.ConnectBotChannel)
	bots.Post("/:botId/channel/embedded", con.ConnectBotChannelEmbedded)
	bots.Post("/:botId/channel/register", con.RegisterBotChannel)
	bots.Get("/:botId/templates", con.ListBotTemplates)
	bots.Post("/:botId/templates/sync", con.SyncBotTemplates)
	bots.Post("/:botId/templates/:name/test", con.TestBotTemplate)
	bots.Get("/:botId/contacts/:contactId/billing", con.ListBilling)
	bots.Post("/:botId/contacts/:contactId/billing", con.CreateBilling)
	bots.Get("/:botId/audiences", con.ListAudiences)
	bots.Post("/:botId/audiences", con.CreateAudience)
	bots.Get("/:botId/audiences/:audienceId/contacts", con.ResolveAudience)
	bots.Post("/:botId/audiences/:audienceId/contacts/:contactId", con.AddAudienceContact)
	bots.Post("/:botId/flows/:flowId/schedule/queue", con.QueueBotFlowSchedule)
	bots.Get("/:botId/chats", con.ListBotChats)
	bots.Get("/:botId/stream", con.StreamBotEvents)

	// ---- Bandeja de atención humana (autz por la org dueña del bot) ----
	chats := app.Group("/chats", authmw.VerifyToken)
	chats.Get("/:chatId", con.GetChat)
	chats.Get("/:chatId/messages", con.ListChatMessages)
	chats.Post("/:chatId/messages", con.SendChatMessage)
	chats.Put("/:chatId/mode", con.SetChatMode)
	chats.Post("/:chatId/reset", con.ResetChatFlowState)
	chats.Post("/:chatId/read", con.MarkChatRead)

	msgs := app.Group("/messages", authmw.VerifyToken)
	msgs.Get("/:messageId/media", con.GetMessageMedia)

	// ---- Webhook WhatsApp (Meta): único, ruteo por phone_number_id ----
	webhook := app.Group("/webhook")
	webhook.Get("/whatsapp", con.WhatsAppVerify)
	webhook.Post("/whatsapp", con.WhatsAppWebhook)
}
