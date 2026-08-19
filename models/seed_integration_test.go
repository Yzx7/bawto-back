package models

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/db/defaults"
)

// Pruebas de la semilla de creación (PLAN-SEMILLA-DE-ORGANIZACION-Y-BOT.md).
// Las de integración necesitan DATABASE_URL y el backend local apagado: el worker
// de entrega reclama los runs de la prueba porque ClaimFlowRun no filtra por bot.

// El motor enruta por source/target y nunca lee edges[].id, pero React Flow lo
// exige para dibujar. CreateFlow era la única escritura del borrador que no
// pasaba por engine.NormalizeEdgeIDs, y era inofensivo solo mientras todo flujo
// nacía vacío: con la semilla, el primer flujo del primer bot se abriría en el
// editor sin una sola conexión y el autor lo daría por roto.
func TestCreateFlowRellenaLosIDsDeArista(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "semilla_ids_")

	sinIDs := json.RawMessage(`{"id":"f_ids","name":"Sin ids","trigger":{"type":"message","match":"any"},
		"nodes":[{"id":"n1","kind":"send","body":"hola"},{"id":"n2","kind":"action","action":"end"}],
		"edges":[{"source":"trigger","target":"n1"},{"source":"n1","target":"n2"}]}`)

	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "sin-ids", Name: "Sin ids", TriggerType: "message", Draft: sinIDs, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	// Se relee de PostgreSQL: lo que importa es lo que el editor va a recibir, no
	// lo que quedó en memoria.
	guardado, err := GetFlow(ctx, pool, bot.ID, flow.ID)
	if err != nil || guardado == nil {
		t.Fatalf("GetFlow: err=%v flow=%v", err, guardado)
	}
	for i, id := range idsDeAristas(t, guardado.Draft) {
		if id == "" {
			t.Fatalf("la arista %d se guardó sin id: el editor la dibujaría desconectada", i)
		}
	}
}

func idsDeAristas(t *testing.T, draft json.RawMessage) []string {
	t.Helper()
	var doc struct {
		Edges []struct {
			ID string `json:"id"`
		} `json:"edges"`
	}
	if err := json.Unmarshal(draft, &doc); err != nil {
		t.Fatalf("borrador ilegible: %v", err)
	}
	if len(doc.Edges) == 0 {
		t.Fatal("el borrador guardado no tiene aristas: la prueba no comprueba nada")
	}
	ids := make([]string, 0, len(doc.Edges))
	for _, edge := range doc.Edges {
		ids = append(ids, edge.ID)
	}
	return ids
}

// Fase 1: una organización recién creada trae sus tablas, con sus campos y en el
// orden del fichero de la semilla.
func TestOrganizacionNaceConSusTablas(t *testing.T) {
	pool, ctx := flowTestPool(t)
	org := orgDePrueba(t, ctx, pool, "semilla_org_")

	esperados, err := defaults.Objetos()
	if err != nil {
		t.Fatalf("defaults.Objetos: %v", err)
	}
	objetos, err := ListDataObjectsByOrg(ctx, pool, org.ID)
	if err != nil {
		t.Fatalf("ListDataObjectsByOrg: %v", err)
	}
	if len(objetos) != len(esperados) {
		t.Fatalf("se esperaban %d tablas sembradas y hay %d", len(esperados), len(objetos))
	}
	porClave := map[string]DataObject{}
	for _, objeto := range objetos {
		porClave[objeto.Key] = objeto
	}
	for _, esperado := range esperados {
		objeto, ok := porClave[esperado.Key]
		if !ok {
			t.Fatalf("falta la tabla %q", esperado.Key)
		}
		if objeto.Name != esperado.Name || objeto.PluralName != esperado.PluralName {
			t.Errorf("etiquetas de %q: %q/%q, se esperaba %q/%q",
				esperado.Key, objeto.Name, objeto.PluralName, esperado.Name, esperado.PluralName)
		}
		campos, err := ListDataFieldsByOrg(ctx, pool, org.ID, objeto.ID)
		if err != nil {
			t.Fatalf("ListDataFieldsByOrg(%s): %v", esperado.Key, err)
		}
		if len(campos) != len(esperado.Fields) {
			t.Fatalf("la tabla %q tiene %d campos y se esperaban %d", esperado.Key, len(campos), len(esperado.Fields))
		}
		// El orden importa: es el que ve el dueño en el panel, y ListDataFieldsByOrg
		// ordena por created_at.
		for i, campo := range campos {
			quiere := esperado.Fields[i]
			if campo.Key != quiere.Key || campo.Label != quiere.Label ||
				campo.Type != quiere.Type || campo.Required != quiere.Required {
				t.Errorf("%s.campo[%d] = %+v, se esperaba %+v", esperado.Key, i, campo, quiere)
			}
		}
	}
}

// Si la semilla falla, la organización no llega a existir: una organización a la
// que le falte una tabla que el grafo nombra es peor que ninguna organización.
func TestFalloDeLaSemillaDejaLaOrganizacionSinCrear(t *testing.T) {
	pool, ctx := flowTestPool(t)
	usuario := usuarioDePrueba(t, ctx, pool, "semilla_rb_")

	original := seedObjects
	seedObjects = func() ([]defaults.SeedObject, error) {
		return nil, errors.New("semilla rota a propósito")
	}
	t.Cleanup(func() { seedObjects = original })

	if _, err := CreateOrganization(ctx, pool, usuario, "Org que no debe existir", nil, nil); err == nil {
		t.Fatal("CreateOrganization devolvió éxito con la semilla rota")
	}
	orgs, err := GetUserOrganizations(ctx, pool, usuario)
	if err != nil {
		t.Fatalf("GetUserOrganizations: %v", err)
	}
	if len(orgs) != 0 {
		t.Fatalf("la transacción no se deshizo: quedaron %d organizaciones", len(orgs))
	}
}

// Fase 2 (fontanería): el bot nace con su flujo `principal`, en borrador y sin
// versión publicada — publicar es humano, y el bot recién creado ni siquiera
// tiene canal conectado.
func TestBotNaceConSuFlujoPrincipal(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "semilla_bot_")

	flujos, err := ListFlows(ctx, pool, bot.ID, false)
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	if len(flujos) != 1 {
		t.Fatalf("el bot debía nacer con exactamente un flujo y tiene %d", len(flujos))
	}
	flujo := flujos[0]
	if flujo.Key != "principal" || flujo.TriggerType != "message" || !flujo.IsFallback {
		t.Fatalf("flujo sembrado inesperado: key=%q trigger=%q fallback=%v",
			flujo.Key, flujo.TriggerType, flujo.IsFallback)
	}
	if flujo.Status != "draft" || flujo.PublishedVersionID != nil {
		t.Fatalf("el flujo sembrado no debe nacer publicado: status=%q version=%v",
			flujo.Status, flujo.PublishedVersionID)
	}
	if len(flujo.Draft) == 0 || string(flujo.Draft) == "{}" {
		t.Fatal("el flujo sembrado nació con el borrador vacío")
	}
}

// usuarioDePrueba crea un usuario y lo borra al terminar.
func usuarioDePrueba(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefijo string) string {
	t.Helper()
	u := randID(prefijo)
	mustUser(t, ctx, pool, u, "Semilla Owner", u+"@test.local")
	t.Cleanup(func() { pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, u) })
	return u
}

// orgDePrueba crea usuario + organización y los borra al terminar.
func orgDePrueba(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefijo string) *Organization {
	t.Helper()
	org, err := CreateOrganization(ctx, pool, usuarioDePrueba(t, ctx, pool, prefijo), "Org semilla", nil, nil)
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	t.Cleanup(func() { DeleteOrganization(ctx, pool, org.ID) })
	return org
}

// objetoSembrado devuelve una tabla que la organización trae al nacer. Las
// pruebas que necesitan una de ellas la reutilizan: volver a crearla choca con el
// UNIQUE (org_id, key).
func objetoSembrado(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, key string) *DataObject {
	t.Helper()
	objetos, err := ListDataObjectsByOrg(ctx, pool, orgID)
	if err != nil {
		t.Fatalf("ListDataObjectsByOrg: %v", err)
	}
	for i := range objetos {
		if objetos[i].Key == key {
			return &objetos[i]
		}
	}
	t.Fatalf("la organización no trae la tabla sembrada %q", key)
	return nil
}

// El fragmento comercial se injerta solo si **los dos hechos** están en la base:
// la fila de `negocio` dice que quiere vender y hay conexión `meudim` activa.
// Querer vender sin tienda no permite dibujar la rama —engine.Validate rechaza
// una `connection` vacía y el flujo ni siquiera se podría publicar—, y tener
// tienda sin haberlo pedido no la impone: la conexión sigue ahí para cuando
// cambie de idea.
func TestElInjertoComercialExigeLosDosHechos(t *testing.T) {
	pool, ctx := flowTestPool(t)

	nucleo, err := defaults.FlujoInicial()
	if err != nil {
		t.Fatalf("defaults.FlujoInicial: %v", err)
	}
	nodosDelNucleo := len(idsDeNodos(t, nucleo))

	// 1 · Quiere vender, pero no ha conectado la tienda.
	quiereVender := orgDePrueba(t, ctx, pool, "semilla_vende_")
	declaraNegocio(t, ctx, pool, quiereVender.ID, "vender")
	sinTienda := botDeOrg(t, ctx, pool, quiereVender.ID, "Bot sin tienda")
	if n := nodosDelFlujoSembrado(t, ctx, pool, sinTienda); n != nodosDelNucleo {
		t.Fatalf("sin tienda conectada el bot debe nacer con el núcleo: %d nodos, se esperaban %d", n, nodosDelNucleo)
	}

	// 2 · Con la tienda conectada, el mismo dato de `negocio` sí injerta.
	if _, err := SaveExternalConnection(ctx, pool, ExternalConnectionInput{
		OrgID: quiereVender.ID, Key: "meudim", Driver: "meudim", Label: "Tienda",
		BaseURL: "https://api.meud.im", CredentialEnc: []byte("cred-de-prueba"), Status: "active",
	}); err != nil {
		t.Fatalf("SaveExternalConnection: %v", err)
	}
	conTienda := botDeOrg(t, ctx, pool, quiereVender.ID, "Bot con tienda")
	if n := nodosDelFlujoSembrado(t, ctx, pool, conTienda); n <= nodosDelNucleo {
		t.Fatalf("con tienda y voluntad de vender el bot debe nacer injertado: %d nodos, el núcleo tiene %d", n, nodosDelNucleo)
	}

	// 3 · Tienda conectada pero solo quiere informar: se respeta lo que pidió.
	soloInforma := orgDePrueba(t, ctx, pool, "semilla_informa_")
	declaraNegocio(t, ctx, pool, soloInforma.ID, "informar")
	if _, err := SaveExternalConnection(ctx, pool, ExternalConnectionInput{
		OrgID: soloInforma.ID, Key: "meudim", Driver: "meudim", Label: "Tienda",
		BaseURL: "https://api.meud.im", CredentialEnc: []byte("cred-de-prueba"), Status: "active",
	}); err != nil {
		t.Fatalf("SaveExternalConnection: %v", err)
	}
	conversacional := botDeOrg(t, ctx, pool, soloInforma.ID, "Bot conversacional")
	if n := nodosDelFlujoSembrado(t, ctx, pool, conversacional); n != nodosDelNucleo {
		t.Fatalf("quien solo quiere informar no recibe la rama comercial: %d nodos, se esperaban %d", n, nodosDelNucleo)
	}
}

// declaraNegocio escribe la fila que el cuestionario deja en `negocio`.
func declaraNegocio(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, automatiza string) {
	t.Helper()
	if _, err := MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID: orgID, ObjectKey: "negocio", Operation: "create",
		Values:         map[string]any{"nombre": "Negocio de prueba", "activo": "true", "automatiza": automatiza},
		IdempotencyKey: randID("negocio_"),
	}); err != nil {
		t.Fatalf("MutateDataRecord(negocio): %v", err)
	}
}

func nodosDelFlujoSembrado(t *testing.T, ctx context.Context, pool *pgxpool.Pool, bot *Bot) int {
	t.Helper()
	flujos, err := ListFlows(ctx, pool, bot.ID, false)
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	if len(flujos) != 1 {
		t.Fatalf("el bot debía nacer con un flujo y tiene %d", len(flujos))
	}
	return len(idsDeNodos(t, flujos[0].Draft))
}

// botDeOrg crea un bot en una organización ya existente. botDePrueba crea la suya
// propia, y aquí lo que se prueba es justo lo que la organización trae dentro.
func botDeOrg(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, nombre string) *Bot {
	t.Helper()
	bot, err := CreateBot(ctx, pool, orgID, nombre, "")
	if err != nil {
		t.Fatalf("CreateBot(%s): %v", nombre, err)
	}
	return bot
}

// flujoSembrado devuelve el flujo `principal` con el que nace el bot.
func flujoSembrado(t *testing.T, ctx context.Context, pool *pgxpool.Pool, botID string) *Flow {
	t.Helper()
	flujos, err := ListFlows(ctx, pool, botID, false)
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	for i := range flujos {
		if flujos[i].Key == "principal" {
			return &flujos[i]
		}
	}
	t.Fatal("el bot no trae su flujo `principal`")
	return nil
}
