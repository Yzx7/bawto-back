package models

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pertenencia a la audiencia de un flujo, contra Postgres de verdad.
//
// Se prueba aquí y no con un stub porque lo que se está afirmando es cómo se
// comporta `QueryDataRecords` en los bordes —cero coincidencias, objeto que no
// existe— y un stub decidiría por su cuenta qué encuentra, que es justamente lo
// que no queremos dar por supuesto. Es el mismo motivo por el que el filtro de
// `activo` de Sistemuino se comprueba en el Router y no solo en el bloque.

const condicionPiloto = `{"object":"perfiles_test","linkCurrentContact":"true",` +
	`"where.1.field":"piloto","where.1.op":"eq","where.1.value":"si"}`

// audienciaDePrueba monta org + bot + tabla `perfiles_test` con un campo
// `piloto`, y devuelve el bot ya listo para crear registros.
func audienciaDePrueba(t *testing.T, ctx context.Context, pool *pgxpool.Pool, prefijo string) (*Bot, *DataObject) {
	t.Helper()
	bot := botDePrueba(t, ctx, pool, prefijo)
	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "perfiles_test", "Perfil", "Perfiles")
	if err != nil {
		t.Fatalf("CreateDataObjectByOrg: %v", err)
	}
	if _, err := UpsertDataFieldByOrg(ctx, pool, bot.OrgID, object.ID, "piloto", "Piloto", "text", false); err != nil {
		t.Fatalf("UpsertDataFieldByOrg: %v", err)
	}
	return bot, object
}

// contactoConPerfil crea el contacto, su registro y el vínculo entre ambos, que
// es lo que `linkCurrentContact` resuelve en tiempo de despacho.
func contactoConPerfil(t *testing.T, ctx context.Context, pool *pgxpool.Pool, bot *Bot, object *DataObject, phone, piloto string) *Contact {
	t.Helper()
	contact, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", phone, "Contacto", "active", nil)
	if err != nil {
		t.Fatalf("SaveContactByOrg: %v", err)
	}
	record, err := CreateDataRecordByOrg(ctx, pool, bot.OrgID, object.ID,
		json.RawMessage(`{"piloto":"`+piloto+`"}`))
	if err != nil {
		t.Fatalf("CreateDataRecordByOrg: %v", err)
	}
	if err := LinkRecordContactByOrg(ctx, pool, bot.OrgID, record.ID, contact.ID, "primary"); err != nil {
		t.Fatalf("LinkRecordContactByOrg: %v", err)
	}
	return contact
}

func TestAudienciaDecideQuienEntra(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot, object := audienciaDePrueba(t, ctx, pool, "aud1_")

	dentro := contactoConPerfil(t, ctx, pool, bot, object, "51900111001", "si")
	fuera := contactoConPerfil(t, ctx, pool, bot, object, "51900111002", "no")

	casos := []struct {
		nombre string
		phone  string
		raw    string
		serves bool
	}{
		{"sin audiencia atiende a todos", fuera.PhoneNormalized, ``, true},
		{"null es sin audiencia", fuera.PhoneNormalized, `null`, true},
		{"el contacto cumple", dentro.PhoneNormalized, condicionPiloto, true},
		{"el contacto no cumple", fuera.PhoneNormalized, condicionPiloto, false},
		{"contacto sin registro", "51900111999", condicionPiloto, false},
		{"sin teléfono no hay a quién comprobar", "", condicionPiloto, false},
	}
	for _, caso := range casos {
		verdict := ContactMatchesAudience(ctx, pool, bot.OrgID, caso.phone, json.RawMessage(caso.raw))
		if verdict.Serves != caso.serves {
			t.Errorf("%s: serves=%v (esperado %v) motivo=%q", caso.nombre, verdict.Serves, caso.serves, verdict.Reason)
		}
	}
}

// El aislamiento por organización no es opcional aquí: la condición decide qué
// grafo atiende a un cliente, así que una tabla homónima de otra organización no
// puede colarse. `QueryDataRecords` impone el `org_id` desde el backend, y esta
// prueba es la que lo afirma para este camino.
func TestAudienciaNoVeTablasDeOtraOrganizacion(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot, object := audienciaDePrueba(t, ctx, pool, "aud2_")
	ajeno, _ := audienciaDePrueba(t, ctx, pool, "aud3_")

	dentro := contactoConPerfil(t, ctx, pool, bot, object, "51900222001", "si")

	// Mismo teléfono, misma clave de tabla, otra organización: no entra.
	verdict := ContactMatchesAudience(ctx, pool, ajeno.OrgID, dentro.PhoneNormalized, json.RawMessage(condicionPiloto))
	if verdict.Serves {
		t.Fatalf("una organización ajena no puede resolver esta audiencia: %+v", verdict)
	}
}

// **La prueba que protege el modo de fallo caro.**
//
// `data_query` trata cero coincidencias como `ok` con `found=false` a propósito,
// para que un Router distinga «no tiene perfil» de «la lectura falló». Esa
// distinción NO debe propagarse al predicado: si un fallo al evaluar se
// tradujera a «no hay restricción que aplicar», una caída de la base convertiría
// un flujo piloto en un flujo que atiende a toda la organización, cobrando
// plantillas y consumiendo ventanas de 24 h.
//
// Escrita viéndola fallar primero: con `return AudienceVerdict{Serves: true}` en
// la rama de error de ContactMatchesAudience, este test se pone rojo.
func TestAudienciaQueFallaNoAtiendeANadie(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot, object := audienciaDePrueba(t, ctx, pool, "aud4_")
	dentro := contactoConPerfil(t, ctx, pool, bot, object, "51900333001", "si")

	averias := map[string]string{
		"la tabla no existe":      `{"object":"tabla_que_no_existe","linkCurrentContact":"true"}`,
		"el campo no existe":      `{"object":"perfiles_test","linkCurrentContact":"true","where.1.field":"inventado","where.1.op":"eq","where.1.value":"x"}`,
		"condición ilegible":      `{"object":`,
		"condición que no valida": `{"object":"perfiles_test"}`,
		"operador que ya no vale": `{"object":"perfiles_test","linkCurrentContact":"true","where.1.field":"piloto","where.1.op":"regex","where.1.value":"x"}`,
	}
	for nombre, raw := range averias {
		verdict := ContactMatchesAudience(ctx, pool, bot.OrgID, dentro.PhoneNormalized, json.RawMessage(raw))
		if verdict.Serves {
			t.Errorf("%s: una audiencia rota debe dejar el flujo sin atender a nadie, y atendió", nombre)
		}
		if verdict.Reason == "" {
			t.Errorf("%s: el descarte debe explicarse, o se depura a ciegas", nombre)
		}
	}
}

// `contacts` es la tabla más natural para una audiencia y no es un `data_object`:
// se resuelve por el camino propio de `contactMatches`. Sin esto, marcar tres
// contactos como piloto obligaría a crear una tabla paralela y vincular fila por
// fila lo que el campo de contacto ya dice.
func TestAudienciaSobreLosContactos(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "audc_")

	if _, err := UpsertContactFieldByOrg(ctx, pool, bot.OrgID, "piloto", "Piloto", "text", false); err != nil {
		t.Fatalf("UpsertContactFieldByOrg: %v", err)
	}
	dentro, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51901111001", "Dentro", "active",
		json.RawMessage(`{"piloto":"si"}`))
	if err != nil {
		t.Fatalf("SaveContactByOrg dentro: %v", err)
	}
	fuera, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51901111002", "Fuera", "active",
		json.RawMessage(`{"piloto":"no"}`))
	if err != nil {
		t.Fatalf("SaveContactByOrg fuera: %v", err)
	}
	bloqueado, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51901111003", "Bloqueado", "blocked",
		json.RawMessage(`{"piloto":"si"}`))
	if err != nil {
		t.Fatalf("SaveContactByOrg bloqueado: %v", err)
	}

	porCampo := `{"object":"@contacts","linkCurrentContact":"true",` +
		`"where.1.field":"piloto","where.1.op":"eq","where.1.value":"si"}`
	// `status` es columna propia, no campo personalizado: se filtra igual.
	porEstado := `{"object":"@contacts","linkCurrentContact":"true",` +
		`"where.1.field":"piloto","where.1.op":"eq","where.1.value":"si",` +
		`"where.2.field":"status","where.2.op":"eq","where.2.value":"active"}`

	casos := []struct {
		nombre string
		phone  string
		raw    string
		serves bool
	}{
		{"campo personalizado, cumple", dentro.PhoneNormalized, porCampo, true},
		{"campo personalizado, no cumple", fuera.PhoneNormalized, porCampo, false},
		{"bloqueado cumple el campo", bloqueado.PhoneNormalized, porCampo, true},
		{"columna propia lo excluye", bloqueado.PhoneNormalized, porEstado, false},
		{"columna propia lo admite", dentro.PhoneNormalized, porEstado, true},
		{"contacto inexistente", "51901119999", porCampo, false},
	}
	for _, caso := range casos {
		verdict := ContactMatchesAudience(ctx, pool, bot.OrgID, caso.phone, json.RawMessage(caso.raw))
		if verdict.Serves != caso.serves {
			t.Errorf("%s: serves=%v (esperado %v) motivo=%q", caso.nombre, verdict.Serves, caso.serves, verdict.Reason)
		}
	}

	// Un campo que no existe es una avería de configuración, no un "no cumple":
	// pero acaba igual, sin atender a nadie.
	roto := `{"object":"@contacts","linkCurrentContact":"true",` +
		`"where.1.field":"inventado","where.1.op":"eq","where.1.value":"x"}`
	if v := ContactMatchesAudience(ctx, pool, bot.OrgID, dentro.PhoneNormalized, json.RawMessage(roto)); v.Serves {
		t.Error("un campo inexistente no puede hacer que el flujo atienda")
	}
}

// El centinela lleva `@` porque `data_objects.key` no lo admite. Si fuera
// `contacts` a secas, una organización con su propia tabla `contacts` vería al
// runtime consultar otra cosa en silencio.
func TestObjetoContactsNoTapaUnaTablaDelUsuario(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "audd_")

	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "contacts", "Contacto", "Contactos")
	if err != nil {
		t.Fatalf("una organización puede llamar 'contacts' a su tabla: %v", err)
	}
	if _, err := UpsertDataFieldByOrg(ctx, pool, bot.OrgID, object.ID, "piloto", "Piloto", "text", false); err != nil {
		t.Fatalf("UpsertDataFieldByOrg: %v", err)
	}
	contact, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51902222001", "C", "active", nil)
	if err != nil {
		t.Fatalf("SaveContactByOrg: %v", err)
	}
	record, err := CreateDataRecordByOrg(ctx, pool, bot.OrgID, object.ID, json.RawMessage(`{"piloto":"si"}`))
	if err != nil {
		t.Fatalf("CreateDataRecordByOrg: %v", err)
	}
	if err := LinkRecordContactByOrg(ctx, pool, bot.OrgID, record.ID, contact.ID, "primary"); err != nil {
		t.Fatalf("LinkRecordContactByOrg: %v", err)
	}

	// `contacts` (sin @) debe resolverse contra LA TABLA DEL USUARIO.
	suya := `{"object":"contacts","linkCurrentContact":"true",` +
		`"where.1.field":"piloto","where.1.op":"eq","where.1.value":"si"}`
	if v := ContactMatchesAudience(ctx, pool, bot.OrgID, contact.PhoneNormalized, json.RawMessage(suya)); !v.Serves {
		t.Fatalf("la tabla del usuario llamada 'contacts' debe seguir siendo consultable: %+v", v)
	}

	// Y `@contacts` contra los contactos reales, que no tienen campo `piloto`.
	nuestra := `{"object":"@contacts","linkCurrentContact":"true",` +
		`"where.1.field":"piloto","where.1.op":"eq","where.1.value":"si"}`
	if v := ContactMatchesAudience(ctx, pool, bot.OrgID, contact.PhoneNormalized, json.RawMessage(nuestra)); v.Serves {
		t.Fatalf("@contacts no puede resolverse contra la tabla del usuario: %+v", v)
	}
}

// **La vista previa tiene que coincidir con el despacho, contacto por contacto.**
//
// Es la única propiedad que la hace útil: una lista que enseñara un conjunto y un
// bot que atendiera otro daría confianza falsa justo antes de publicar, que es
// peor que no tener vista previa. Por eso no se comprueba «devuelve 2 filas»,
// sino que para CADA contacto de la organización el veredicto del predicado y la
// pertenencia a la vista previa dicen lo mismo.
func TestVistaPreviaCoincideConElDespacho(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot, object := audienciaDePrueba(t, ctx, pool, "prev1_")

	if _, err := UpsertContactFieldByOrg(ctx, pool, bot.OrgID, "piloto", "Piloto", "text", false); err != nil {
		t.Fatalf("UpsertContactFieldByOrg: %v", err)
	}
	// Tres con registro vinculado (dos cumplen), uno suelto sin registro, y uno
	// bloqueado para que el filtro por columna propia tenga a quién excluir.
	contactoConPerfil(t, ctx, pool, bot, object, "51903000001", "si")
	contactoConPerfil(t, ctx, pool, bot, object, "51903000002", "si")
	contactoConPerfil(t, ctx, pool, bot, object, "51903000003", "no")
	if _, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51903000004", "Suelto", "active",
		json.RawMessage(`{"piloto":"si"}`)); err != nil {
		t.Fatalf("SaveContactByOrg suelto: %v", err)
	}
	if _, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51903000005", "Bloqueado", "blocked",
		json.RawMessage(`{"piloto":"si"}`)); err != nil {
		t.Fatalf("SaveContactByOrg bloqueado: %v", err)
	}

	condiciones := map[string]string{
		"tabla de datos": condicionPiloto,
		"contactos por campo": `{"object":"@contacts","linkCurrentContact":"true",` +
			`"where.1.field":"piloto","where.1.op":"eq","where.1.value":"si"}`,
		"contactos por columna propia": `{"object":"@contacts","linkCurrentContact":"true",` +
			`"where.1.field":"status","where.1.op":"eq","where.1.value":"active"}`,
		"sin condición": ``,
	}

	todos, err := ListContactsByOrg(ctx, pool, bot.OrgID)
	if err != nil {
		t.Fatalf("ListContactsByOrg: %v", err)
	}

	for nombre, raw := range condiciones {
		preview, err := PreviewAudience(ctx, pool, bot.OrgID, json.RawMessage(raw))
		if err != nil {
			t.Errorf("%s: PreviewAudience: %v", nombre, err)
			continue
		}
		enPreview := map[string]bool{}
		for _, c := range preview.Contacts {
			enPreview[c.ID] = true
		}
		if preview.Total != len(preview.Contacts) {
			t.Errorf("%s: el caso de prueba cabe entero, total=%d listados=%d",
				nombre, preview.Total, len(preview.Contacts))
		}
		for _, contacto := range todos {
			verdict := ContactMatchesAudience(ctx, pool, bot.OrgID, contacto.PhoneNormalized, json.RawMessage(raw))
			if verdict.Serves != enPreview[contacto.ID] {
				t.Errorf("%s: %s — el despacho dice serves=%v y la vista previa lo %s",
					nombre, contacto.PhoneNormalized, verdict.Serves,
					map[bool]string{true: "incluye", false: "excluye"}[enPreview[contacto.ID]])
			}
		}
	}
}

// Una condición rota se le muestra al autor tal cual. En el despacho un error se
// traduce a «no atiende a nadie» —para no abrir el flujo a todos por una avería—,
// pero aquí ocultarlo dejaría una condición inválida pareciendo una audiencia
// vacía, que es el diagnóstico contrario al correcto.
func TestVistaPreviaDevuelveElErrorDeUnaCondicionRota(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot, _ := audienciaDePrueba(t, ctx, pool, "prev2_")

	rotas := map[string]string{
		"tabla inexistente":        `{"object":"no_existe","linkCurrentContact":"true"}`,
		"campo inexistente":        `{"object":"@contacts","linkCurrentContact":"true","where.1.field":"nada","where.1.op":"eq","where.1.value":"x"}`,
		"sin linkCurrentContact":   `{"object":"@contacts"}`,
		"con variable en el valor": `{"object":"@contacts","linkCurrentContact":"true","where.1.field":"status","where.1.op":"eq","where.1.value":"{x}"}`,
	}
	for nombre, raw := range rotas {
		if _, err := PreviewAudience(ctx, pool, bot.OrgID, json.RawMessage(raw)); err == nil {
			t.Errorf("%s: la vista previa debe devolver el error, no una lista vacía", nombre)
		}
	}
}

// **Cortar con audiencia toca solo a quien contradice la condición.**
//
// Es lo que separa esta acción de un martillo. Mover a UN contacto al flujo
// restringido no puede costar reiniciar la conversación de toda la organización,
// que es lo que pasaba cortando «las del flujo de reserva».
func TestCortarConAudienciaEsQuirurgico(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "cut1_")

	restringido, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "piloto", Name: "Piloto", TriggerType: "message", UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	otro, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "general", Name: "General", TriggerType: "message", UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow general: %v", err)
	}

	if _, err := UpsertContactFieldByOrg(ctx, pool, bot.OrgID, "piloto", "Piloto", "text", false); err != nil {
		t.Fatalf("UpsertContactFieldByOrg: %v", err)
	}
	// `dentro` cumple la condición pero conversa en el general: hay que moverlo.
	// `ajeno` no cumple y también conversa en el general: NO se toca.
	// `saliente` ya no cumple pero está sellado al restringido: hay que sacarlo.
	crea := func(phone, piloto, flowID string) string {
		c, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", phone, "C", "active",
			json.RawMessage(`{"piloto":"`+piloto+`"}`))
		if err != nil {
			t.Fatalf("SaveContactByOrg %s: %v", phone, err)
		}
		var chatID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO chats (bot_id, contact_id, current_layer) VALUES ($1::uuid,$2::uuid,$3::jsonb) RETURNING id::text`,
			bot.ID, c.ID,
			`{"flowId":"`+flowID+`","nodeId":"n_espera"}`).Scan(&chatID); err != nil {
			t.Fatalf("insert chat %s: %v", phone, err)
		}
		return chatID
	}
	chatDentro := crea("51904000001", "si", otro.ID)
	chatAjeno := crea("51904000002", "no", otro.ID)
	chatSaliente := crea("51904000003", "no", restringido.ID)
	chatQuieto := crea("51904000004", "si", restringido.ID) // ya está donde toca

	condicion := json.RawMessage(`{"object":"@contacts","linkCurrentContact":"true",` +
		`"where.1.field":"piloto","where.1.op":"eq","where.1.value":"si"}`)
	if _, err := SetFlowAudience(ctx, pool, bot.ID, restringido.ID, condicion, "tester"); err != nil {
		t.Fatalf("SetFlowAudience: %v", err)
	}

	cortadas, _, err := ResetChatsOfFlow(ctx, pool, bot.ID, restringido.ID, bot.OrgID, condicion)
	if err != nil {
		t.Fatalf("ResetChatsOfFlow: %v", err)
	}
	if cortadas != 2 {
		t.Errorf("debían cortarse 2 (el que entra y el que sale), y fueron %d", cortadas)
	}

	sellado := func(chatID string) string {
		var layer string
		if err := pool.QueryRow(ctx,
			`SELECT COALESCE(current_layer ->> 'flowId','') FROM chats WHERE id=$1::uuid`, chatID).Scan(&layer); err != nil {
			t.Fatalf("leer chat: %v", err)
		}
		return layer
	}
	if sellado(chatDentro) != "" {
		t.Error("el contacto que entra en la audiencia debía quedar libre para que el despacho lo reubique")
	}
	if sellado(chatSaliente) != "" {
		t.Error("el contacto que salió de la audiencia debía quedar libre")
	}
	// La afirmación que de verdad importa: al de al lado no se le tocó.
	if sellado(chatAjeno) != otro.ID {
		t.Error("un contacto ajeno a la audiencia NO puede perder su conversación")
	}
	if sellado(chatQuieto) != restringido.ID {
		t.Error("quien ya estaba donde le toca no necesita que le corten nada")
	}
}

// Sin audiencia no hay condición que aplicar, y la única lectura posible es la
// literal: cortar las conversaciones selladas a ese flujo.
func TestCortarSinAudienciaCortaLasDeEseFlujo(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "cut2_")

	flujo, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "general", Name: "General", TriggerType: "message", UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	otro, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "otro", Name: "Otro", TriggerType: "message", UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow otro: %v", err)
	}
	for _, caso := range []struct{ phone, flowID string }{
		{"51905000001", flujo.ID}, {"51905000002", flujo.ID}, {"51905000003", otro.ID},
	} {
		c, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", caso.phone, "C", "active", nil)
		if err != nil {
			t.Fatalf("SaveContactByOrg: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO chats (bot_id, contact_id, current_layer) VALUES ($1::uuid,$2::uuid,$3::jsonb)`,
			bot.ID, c.ID, `{"flowId":"`+caso.flowID+`","nodeId":"n"}`); err != nil {
			t.Fatalf("insert chat: %v", err)
		}
	}

	cortadas, activas, err := ResetChatsOfFlow(ctx, pool, bot.ID, flujo.ID, bot.OrgID, nil)
	if err != nil {
		t.Fatalf("ResetChatsOfFlow: %v", err)
	}
	if cortadas != 2 {
		t.Errorf("debían cortarse las 2 de ese flujo, y fueron %d", cortadas)
	}
	// El recuento restante es lo que convierte un «no corté nada» en una
	// respuesta útil: dice dónde están las que siguen vivas.
	var enOtro int
	for _, a := range activas {
		if a.FlowID == otro.ID {
			enOtro = a.Count
		}
	}
	if enOtro != 1 {
		t.Errorf("debía quedar 1 conversación viva en el otro flujo, y el recuento dijo %d", enOtro)
	}
}

// El fallback atiende cuando ningún trigger reconoce el mensaje: restringirlo
// dejaría mudo al bot para todos los de fuera. Se rechaza al guardar y no al
// publicar, porque guardar ya es donde el operador cree haber terminado.
func TestFallbackNoAdmiteAudiencia(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot, _ := audienciaDePrueba(t, ctx, pool, "aud5_")

	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "reserva", Name: "Reserva", TriggerType: "message", IsFallback: true, UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	if _, err := SetFlowAudience(ctx, pool, bot.ID, flow.ID, json.RawMessage(condicionPiloto), "tester"); err == nil {
		t.Fatal("el flujo de reserva no puede restringirse por audiencia")
	}
}

// Un `schedule` resuelve su destinatario por `data_view`, no por el contacto de
// un mensaje entrante. Aceptar la audiencia ahí la dejaría guardada y sin
// efecto, que es peor que rechazarla.
func TestScheduleNoAdmiteAudiencia(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot, _ := audienciaDePrueba(t, ctx, pool, "aud6_")

	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "recordatorio", Name: "Recordatorio", TriggerType: "schedule", UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}
	if _, err := SetFlowAudience(ctx, pool, bot.ID, flow.ID, json.RawMessage(condicionPiloto), "tester"); err == nil {
		t.Fatal("un flujo schedule no puede tener audiencia en este MVP")
	}
}

// Asignar y retirar es una operación de ida y vuelta, y retirar tiene que
// devolver el flujo exactamente al comportamiento de siempre: sin restricción,
// no con un `{}` que el dispatcher tuviera que interpretar.
func TestAsignarYRetirarAudiencia(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot, _ := audienciaDePrueba(t, ctx, pool, "aud7_")

	flow, err := CreateFlow(ctx, pool, bot.ID, NewFlow{
		Key: "piloto", Name: "Piloto", TriggerType: "message", UserID: "tester",
	})
	if err != nil {
		t.Fatalf("CreateFlow: %v", err)
	}

	conAudiencia, err := SetFlowAudience(ctx, pool, bot.ID, flow.ID, json.RawMessage(condicionPiloto), "tester")
	if err != nil {
		t.Fatalf("SetFlowAudience: %v", err)
	}
	if len(conAudiencia.Audience) == 0 {
		t.Fatal("la audiencia no se guardó")
	}

	sinAudiencia, err := SetFlowAudience(ctx, pool, bot.ID, flow.ID, nil, "tester")
	if err != nil {
		t.Fatalf("retirar la audiencia: %v", err)
	}
	if len(sinAudiencia.Audience) != 0 {
		t.Fatalf("retirar debe dejar la columna nula, y dejó %s", sinAudiencia.Audience)
	}

	// Una condición inválida no se guarda a medias: el flujo se queda como estaba.
	if _, err := SetFlowAudience(ctx, pool, bot.ID, flow.ID, json.RawMessage(`{"object":"perfiles_test"}`), "tester"); err == nil {
		t.Fatal("sin linkCurrentContact no debe poder asignarse")
	}
}

// Un contacto sin teléfono —el cliente que adoptó un nombre de usuario de
// WhatsApp— no puede tumbar la vista previa de la audiencia. Rompió el panel el
// 2026-08-15, en cuanto la migración 030 permitió `phone_normalized` nulo: la
// pantalla entera respondía «cannot scan NULL into *string» aunque el contacto
// sin número ni siquiera fuese el que el autor estaba buscando.
func TestVistaPreviaAdmiteContactoSinTelefono(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "audsin_")

	sinTelefono, err := EnsureInboundContact(ctx, pool, bot.ID, ChannelIdentity{
		UserID: randID("PE.15074507378"), Name: "Angelo",
	})
	if err != nil {
		t.Fatalf("EnsureInboundContact: %v", err)
	}
	if sinTelefono.PhoneNormalized != "" {
		t.Fatalf("el contacto no debería tener teléfono: %q", sinTelefono.PhoneNormalized)
	}

	preview, err := PreviewAudience(ctx, pool, bot.OrgID, nil)
	if err != nil {
		t.Fatalf("PreviewAudience: %v", err)
	}
	var visto *AudienceContact
	for i := range preview.Contacts {
		if preview.Contacts[i].ID == sinTelefono.ID {
			visto = &preview.Contacts[i]
		}
	}
	if visto == nil {
		t.Fatalf("el contacto sin teléfono no aparece en la vista previa (%d contactos)", len(preview.Contacts))
	}
	// Sin número, la identidad mostrable es el BSUID: una celda vacía no
	// permitiría distinguir a dos clientes con el mismo nombre.
	if visto.Phone != sinTelefono.ChannelUserID {
		t.Fatalf("identidad mostrable equivocada: %q, esperaba %q", visto.Phone, sinTelefono.ChannelUserID)
	}
}
