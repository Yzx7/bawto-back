package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Despacho de flujos restringidos por audiencia
// (PLAN-FLUJOS-POR-AUDIENCIA-Y-PERMISOS §2 y §8).
//
// Contra Postgres de verdad: lo que se afirma es qué flujo elige el webhook, y
// eso depende de `PublishedMessageFlows` —orden por is_fallback, priority, id— y
// de `QueryDataRecords`. Con stubs se estaría probando el stub.

func dispatchController(t *testing.T) (*Controller, *pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("DATABASE_URL no seteada; se omite el test de integración")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Logger a /dev/null: el descarte por audiencia registra en cada turno y no
	// queremos que la salida del test lo esconda todo.
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Controller{Env: &env.Env{Postgres: pool, Logger: quiet}}, pool, ctx
}

func idAleatorio(prefijo string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefijo + hex.EncodeToString(b)
}

// escenario monta org + bot + tabla de perfiles y devuelve lo necesario para
// publicar flujos y comprobar a quién atienden.
type escenario struct {
	bot    *models.BotChannel
	object *models.DataObject
	pool   *pgxpool.Pool
	ctx    context.Context
	t      *testing.T
}

func nuevoEscenario(t *testing.T, prefijo string) *escenario {
	t.Helper()
	con, pool, ctx := dispatchController(t)
	_ = con

	userID := idAleatorio(prefijo)
	if _, err := pool.Exec(ctx,
		`INSERT INTO "user"(id, name, email, "emailVerified") VALUES ($1,$2,$3,false)`,
		userID, "Dispatch", userID+"@test.local"); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, userID) })

	org, err := models.CreateOrganization(ctx, pool, userID, "Org despacho", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() { models.DeleteOrganization(ctx, pool, org.ID) })

	bot, err := models.CreateBot(ctx, pool, org.ID, "Bot despacho", "")
	if err != nil {
		t.Fatalf("CreateBot: %v", err)
	}
	object, err := models.CreateDataObjectByOrg(ctx, pool, org.ID, "perfiles_disp", "Perfil", "Perfiles")
	if err != nil {
		t.Fatalf("CreateDataObjectByOrg: %v", err)
	}
	if _, err := models.UpsertDataFieldByOrg(ctx, pool, org.ID, object.ID, "piloto", "Piloto", "text", false); err != nil {
		t.Fatalf("UpsertDataFieldByOrg: %v", err)
	}
	return &escenario{
		bot:    &models.BotChannel{ID: bot.ID, OrgID: org.ID},
		object: object,
		pool:   pool,
		ctx:    ctx,
		t:      t,
	}
}

// publicaFlujo crea y publica un flujo `message` que reconoce cualquier mensaje.
func (e *escenario) publicaFlujo(key string, priority int, audience string) *models.Flow {
	e.t.Helper()
	graph := json.RawMessage(`{
		"id":"` + key + `","name":"` + key + `","trigger":{"type":"message","match":"any"},
		"nodes":[{"id":"n1","kind":"send","body":"hola"},{"id":"n2","kind":"action","action":"end"}],
		"edges":[{"id":"e0","source":"trigger","target":"n1"},{"id":"e1","source":"n1","target":"n2"}]
	}`)
	flow, err := models.CreateFlow(e.ctx, e.pool, e.bot.ID, models.NewFlow{
		Key: key, Name: key, TriggerType: "message", Priority: priority, Draft: graph, UserID: "tester",
	})
	if err != nil {
		e.t.Fatalf("CreateFlow %s: %v", key, err)
	}
	if audience != "" {
		if _, err := models.SetFlowAudience(e.ctx, e.pool, e.bot.ID, flow.ID, json.RawMessage(audience), "tester"); err != nil {
			e.t.Fatalf("SetFlowAudience %s: %v", key, err)
		}
	}
	definition, checksum, err := engine.CanonicalChecksum(graph)
	if err != nil {
		e.t.Fatalf("CanonicalChecksum: %v", err)
	}
	if _, err := models.PublishFlow(e.ctx, e.pool, e.bot.ID, flow.ID, definition, checksum, "tester"); err != nil {
		e.t.Fatalf("PublishFlow %s: %v", key, err)
	}
	return flow
}

func (e *escenario) contacto(phone, piloto string) *models.Contact {
	e.t.Helper()
	contact, err := models.SaveContactByOrg(e.ctx, e.pool, e.bot.OrgID, "", phone, "C", "active", nil)
	if err != nil {
		e.t.Fatalf("SaveContactByOrg: %v", err)
	}
	record, err := models.CreateDataRecordByOrg(e.ctx, e.pool, e.bot.OrgID, e.object.ID,
		json.RawMessage(`{"piloto":"`+piloto+`"}`))
	if err != nil {
		e.t.Fatalf("CreateDataRecordByOrg: %v", err)
	}
	if err := models.LinkRecordContactByOrg(e.ctx, e.pool, e.bot.OrgID, record.ID, contact.ID, "primary"); err != nil {
		e.t.Fatalf("LinkRecordContactByOrg: %v", err)
	}
	return contact
}

const condicionDispatch = `{"object":"perfiles_disp","linkCurrentContact":"true",` +
	`"where.1.field":"piloto","where.1.op":"eq","where.1.value":"si"}`

// El caso central: dos flujos que reconocen el mismo mensaje, uno restringido
// con prioridad menor. Quien está en la audiencia entra al restringido; quien no,
// al general. La prioridad sigue siendo el único mecanismo de precedencia — no se
// añadió una regla de "el restringido manda", porque dos mecanismos de orden
// compitiendo es de donde salen los despachos impredecibles.
func TestDespachoEligeElRestringidoSoloParaSuAudiencia(t *testing.T) {
	e := nuevoEscenario(t, "disp1_")
	con, _, ctx := dispatchController(t)

	e.publicaFlujo("piloto", 10, condicionDispatch)
	e.publicaFlujo("general", 100, "")

	dentro := e.contacto("51900444001", "si")
	fuera := e.contacto("51900444002", "no")

	if ref := con.messageFlowForInput(ctx, e.bot, dentro.PhoneNormalized, "", "hola"); ref == nil || ref.Key != "piloto" {
		t.Fatalf("el contacto de la audiencia debía entrar al flujo restringido, y fue: %+v", ref)
	}
	if ref := con.messageFlowForInput(ctx, e.bot, fuera.PhoneNormalized, "", "hola"); ref == nil || ref.Key != "general" {
		t.Fatalf("el contacto de fuera debía caer en el general, y fue: %+v", ref)
	}
}

// Una audiencia que no incluye a nadie deja mudo a SU flujo, no al bot: el
// general sigue atendiendo. Es la diferencia entre un piloto vacío y una avería.
func TestAudienciaVaciaNoRompeElFlujoGeneral(t *testing.T) {
	e := nuevoEscenario(t, "disp2_")
	con, _, ctx := dispatchController(t)

	e.publicaFlujo("piloto", 10, condicionDispatch)
	e.publicaFlujo("general", 100, "")

	// Nadie con piloto = si.
	nadie := e.contacto("51900555001", "no")
	if ref := con.messageFlowForInput(ctx, e.bot, nadie.PhoneNormalized, "", "hola"); ref == nil || ref.Key != "general" {
		t.Fatalf("con la audiencia vacía debe atender el general, y fue: %+v", ref)
	}
}

// Si el único flujo publicado está restringido, quien queda fuera no recibe
// ningún flujo. **Nunca al revés**: un contacto de fuera no puede colarse en el
// restringido por ser el único que hay.
func TestSinFlujoGeneralElDeFueraNoRecibeNada(t *testing.T) {
	e := nuevoEscenario(t, "disp3_")
	con, _, ctx := dispatchController(t)

	e.publicaFlujo("piloto", 10, condicionDispatch)
	fuera := e.contacto("51900666001", "no")

	if ref := con.messageFlowForInput(ctx, e.bot, fuera.PhoneNormalized, "", "hola"); ref != nil {
		t.Fatalf("un contacto de fuera no puede entrar al único flujo, que es restringido: %+v", ref)
	}
}

// **La regla 1 ignora la audiencia, y es deliberado.**
//
// Una conversación a medias sigue en SU flujo aunque el contacto ya no cumpla la
// condición. Comprobar la audiencia aquí lo sacaría a mitad de un `wait`,
// perdiendo su nodo y las variables ya recogidas, que es justo lo que el punto 1
// existe para evitar. La consecuencia —salir de la audiencia no surte efecto
// hasta la próxima conversación— se resuelve con la acción de cortar
// conversaciones activas, que es una decisión del operador.
//
// Si alguien "arregla" esto añadiendo el predicado a la regla 1, este test lo dice.
func TestConversacionAMediasIgnoraLaAudiencia(t *testing.T) {
	e := nuevoEscenario(t, "disp4_")
	con, _, ctx := dispatchController(t)

	restringido := e.publicaFlujo("piloto", 10, condicionDispatch)
	e.publicaFlujo("general", 100, "")

	// Contacto que NO cumple, pero cuya conversación quedó sellada al restringido.
	fuera := e.contacto("51900777001", "no")

	ref := con.messageFlowForInput(ctx, e.bot, fuera.PhoneNormalized, restringido.ID, "hola")
	if ref == nil || ref.Key != "piloto" {
		t.Fatalf("una conversación a medias debe seguir en su flujo aunque el contacto ya no cumpla: %+v", ref)
	}
}

// Entre dos flujos que ambos reconocen el mensaje y ambos incluyen al contacto,
// sigue decidiendo la prioridad. La audiencia acota a quién, no reordena.
func TestLaPrioridadSigueDecidiendoEntreDosQueIncluyen(t *testing.T) {
	e := nuevoEscenario(t, "disp5_")
	con, _, ctx := dispatchController(t)

	e.publicaFlujo("primero", 5, condicionDispatch)
	e.publicaFlujo("segundo", 50, condicionDispatch)

	dentro := e.contacto("51900888001", "si")
	if ref := con.messageFlowForInput(ctx, e.bot, dentro.PhoneNormalized, "", "hola"); ref == nil || ref.Key != "primero" {
		t.Fatalf("debía ganar el de prioridad menor, y fue: %+v", ref)
	}
}
