package models

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Requiere DATABASE_URL. Cubre lo que el motor no puede comprobar sin base:
// aislamiento por organización, tipado de los operadores, proyección de campos y
// que cero coincidencias sea un resultado y no un fallo.

type queryFixture struct {
	bot     *Bot
	contact *Contact
}

// randPhone evita que dos ejecuciones choquen: `contacts.phone_normalized` es
// único por organización, pero un teléfono constante hace que la fila anterior
// sobreviva a la limpieza y contamine el conteo de la siguiente.
func randPhone() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "519" + fmt.Sprintf("%08d", int(b[0])<<16|int(b[1])<<8|int(b[2]))
}

func seedQueryFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) queryFixture {
	t.Helper()
	bot := botDePrueba(t, ctx, pool, "query_")
	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "segmentos", "Segmento", "Segmentos")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct {
		key, label, typ string
	}{
		{"clave", "Clave", "text"},
		{"contexto", "Contexto", "text"},
		{"prioridad", "Prioridad", "number"},
		{"activo", "Activo", "boolean"},
		{"secreto", "Secreto", "text"},
	} {
		if _, err = UpsertDataFieldByOrg(ctx, pool, bot.OrgID, object.ID, field.key, field.label, field.typ, false); err != nil {
			t.Fatal(err)
		}
	}
	for _, values := range []map[string]any{
		{"clave": "ventas_b2b", "contexto": "Opera varios locales", "prioridad": "10", "activo": "true", "secreto": "no mostrar"},
		{"clave": "retail", "contexto": "Compra al detalle", "prioridad": "5", "activo": "true", "secreto": "no mostrar"},
		{"clave": "pausado", "contexto": "Sin atención", "prioridad": "1", "activo": "false", "secreto": "no mostrar"},
	} {
		if _, err = MutateDataRecord(ctx, pool, DataMutationInput{
			OrgID: bot.OrgID, ObjectKey: "segmentos", Operation: "create", Values: values,
			IdempotencyKey: randID("seed_"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	contact, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", randPhone(), "Ana", "active", nil)
	if err != nil {
		t.Fatal(err)
	}
	return queryFixture{bot: bot, contact: contact}
}

func TestQueryDataRecordsIntegration(t *testing.T) {
	pool, ctx := flowTestPool(t)
	fx := seedQueryFixture(t, ctx, pool)

	t.Run("filtra por campo y proyecta solo lo autorizado", func(t *testing.T) {
		result, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			Fields: []string{"clave", "contexto"},
			Where:  []DataQueryRule{{Field: "clave", Op: "eq", Value: "ventas_b2b"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !result.Found || result.Count != 1 {
			t.Fatalf("found=%v count=%d", result.Found, result.Count)
		}
		if _, leaked := result.First.Data["secreto"]; leaked {
			t.Fatal("el campo no autorizado llegó al resultado")
		}
		if result.First.Data["clave"] != "ventas_b2b" {
			t.Fatalf("data=%v", result.First.Data)
		}
	})

	t.Run("un campo declarado que el registro no trae sale nulo", func(t *testing.T) {
		// Omitirlo dejaría el marcador `{segmento.first.data.tono}` literal dentro
		// del prompt del agente.
		objects, err := ListDataObjectsByOrg(ctx, pool, fx.bot.OrgID)
		if err != nil {
			t.Fatal(err)
		}
		var objectID string
		for _, object := range objects {
			if object.Key == "segmentos" {
				objectID = object.ID
			}
		}
		if _, err = UpsertDataFieldByOrg(ctx, pool, fx.bot.OrgID, objectID, "tono", "Tono", "text", false); err != nil {
			t.Fatal(err)
		}
		result, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			Fields: []string{"clave", "tono"},
			Where:  []DataQueryRule{{Field: "clave", Op: "eq", Value: "ventas_b2b"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, present := result.First.Data["tono"]; !present {
			t.Fatalf("el campo declarado debe existir aunque el registro no lo traiga: %v", result.First.Data)
		}
	})

	t.Run("cero coincidencias es un resultado, no un error", func(t *testing.T) {
		result, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			Where: []DataQueryRule{{Field: "clave", Op: "eq", Value: "no_existe"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Found || result.Count != 0 || result.First != nil {
			t.Fatalf("esperaba vacío: %+v", result)
		}
	})

	t.Run("boolean y number se comparan por tipo", func(t *testing.T) {
		result, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			Where: []DataQueryRule{
				{Field: "activo", Op: "eq", Value: "true"},
				{Field: "prioridad", Op: "gte", Value: "6"},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Count != 1 || result.First.Data["clave"] != "ventas_b2b" {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("orden estable por número descendente", func(t *testing.T) {
		result, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			OrderBy: "prioridad", OrderDesc: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if result.Count != 3 || result.First.Data["clave"] != "ventas_b2b" {
			t.Fatalf("first=%v", result.First)
		}
		// Sin desempate el orden podría alternar entre ejecuciones.
		repeat, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			OrderBy: "prioridad", OrderDesc: true,
		})
		if err != nil || repeat.First.RecordID != result.First.RecordID {
			t.Fatalf("el orden no es reproducible: %v", err)
		}
	})

	t.Run("in y contains", func(t *testing.T) {
		result, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			Where: []DataQueryRule{{Field: "clave", Op: "in", Value: "retail, pausado"}},
		})
		if err != nil || result.Count != 2 {
			t.Fatalf("in: count=%d err=%v", result.Count, err)
		}
		result, err = QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			Where: []DataQueryRule{{Field: "contexto", Op: "contains", Value: "LOCALES"}},
		})
		if err != nil || result.Count != 1 {
			t.Fatalf("contains: count=%d err=%v", result.Count, err)
		}
	})

	t.Run("limit acota", func(t *testing.T) {
		result, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos", Limit: 2,
		})
		if err != nil || result.Count != 2 {
			t.Fatalf("count=%d err=%v", result.Count, err)
		}
	})

	t.Run("campo y objeto inexistentes son error", func(t *testing.T) {
		if _, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos",
			Where: []DataQueryRule{{Field: "data->>'x'; DROP TABLE", Op: "eq", Value: "1"}},
		}); err == nil {
			t.Fatal("un campo inventado debe fallar antes de consultar")
		}
		if _, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "tabla_que_no_existe",
		}); err == nil {
			t.Fatal("un objeto inexistente debe fallar")
		}
		if _, err := QueryDataRecords(ctx, pool, DataQueryInput{
			OrgID: fx.bot.OrgID, ObjectKey: "segmentos", Fields: []string{"no_existe"},
		}); err == nil {
			t.Fatal("un campo proyectado inexistente debe fallar")
		}
	})
}

// Una organización no puede leer los registros de otra aunque el objeto se
// llame igual. Es la garantía que sostiene todo el multiempresa.
func TestQueryDataRecordsAislaOrganizacion(t *testing.T) {
	pool, ctx := flowTestPool(t)
	seedQueryFixture(t, ctx, pool)
	intruso := botDePrueba(t, ctx, pool, "query_otro_")

	if _, err := QueryDataRecords(ctx, pool, DataQueryInput{
		OrgID: intruso.OrgID, ObjectKey: "segmentos",
	}); err == nil {
		t.Fatal("la otra organización no tiene ese objeto y debe fallar")
	}

	object, err := CreateDataObjectByOrg(ctx, pool, intruso.OrgID, "segmentos", "Segmento", "Segmentos")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = UpsertDataFieldByOrg(ctx, pool, intruso.OrgID, object.ID, "clave", "Clave", "text", false); err != nil {
		t.Fatal(err)
	}
	result, err := QueryDataRecords(ctx, pool, DataQueryInput{
		OrgID: intruso.OrgID, ObjectKey: "segmentos",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 0 {
		t.Fatalf("leyó %d registros de otra organización", result.Count)
	}
}

// El filtro por contacto vinculado es el que hace posible «el perfil de quien
// escribe». Debe devolver solo lo suyo y nada cuando el contacto no tiene perfil.
func TestQueryDataRecordsPorContactoVinculado(t *testing.T) {
	pool, ctx := flowTestPool(t)
	fx := seedQueryFixture(t, ctx, pool)

	object, err := CreateDataObjectByOrg(ctx, pool, fx.bot.OrgID, "perfiles_contacto", "Perfil", "Perfiles")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = UpsertDataFieldByOrg(ctx, pool, fx.bot.OrgID, object.ID, "segmento_key", "Segmento", "text", false); err != nil {
		t.Fatal(err)
	}
	if _, err = MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID: fx.bot.OrgID, ObjectKey: "perfiles_contacto", Operation: "create",
		Values: map[string]any{"segmento_key": "ventas_b2b"}, IdempotencyKey: randID("perfil_"),
		CurrentContactPhone: fx.contact.PhoneNormalized, LinkCurrentContact: true,
	}); err != nil {
		t.Fatal(err)
	}
	// Un perfil de otro contacto que nunca debe aparecer.
	otro, err := SaveContactByOrg(ctx, pool, fx.bot.OrgID, "", randPhone(), "Beto", "active", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID: fx.bot.OrgID, ObjectKey: "perfiles_contacto", Operation: "create",
		Values: map[string]any{"segmento_key": "retail"}, IdempotencyKey: randID("perfil_"),
		CurrentContactPhone: otro.PhoneNormalized, LinkCurrentContact: true,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := QueryDataRecords(ctx, pool, DataQueryInput{
		OrgID: fx.bot.OrgID, ObjectKey: "perfiles_contacto",
		LinkedContactPhone: fx.contact.PhoneNormalized,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 || result.First.Data["segmento_key"] != "ventas_b2b" {
		t.Fatalf("result=%+v", result)
	}

	sinPerfil, err := QueryDataRecords(ctx, pool, DataQueryInput{
		OrgID: fx.bot.OrgID, ObjectKey: "perfiles_contacto",
		LinkedContactPhone: "51900000000",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sinPerfil.Found {
		t.Fatalf("un contacto sin perfil no puede recibir el de otro: %+v", sinPerfil.First)
	}
}

// El validador del grafo y el ejecutor mantienen dos listas de operadores porque
// `engine` no puede importar `models`. Esta prueba vive aquí, que es el único
// paquete que ve las dos, y falla en cuanto una crece sin la otra.
func TestOperadoresDeDataQueryCoincidenConElValidador(t *testing.T) {
	for _, op := range DataQueryOperators {
		if !ValidDataQueryOperator(op) {
			t.Fatalf("%q está en la lista pero el ejecutor no lo admite", op)
		}
		flow := &engine.Flow{ID: "f", Name: "F",
			Trigger: engine.Trigger{Type: "message", Match: "any"},
			Nodes: []engine.Node{
				{ID: "read", Kind: "tool", ToolRef: "data_query", Args: map[string]string{
					"object": "segmentos", "where.1.field": "clave", "where.1.op": op, "where.1.value": "x",
				}},
				{ID: "ok", Kind: "action", Action: "end"},
				{ID: "error", Kind: "action", Action: "end"},
			},
			Edges: []engine.Edge{
				{ID: "e0", Source: "trigger", Target: "read"},
				{ID: "e1", Source: "read", SourceHandle: "ok", Target: "ok"},
				{ID: "e2", Source: "read", SourceHandle: "error", Target: "error"},
			},
		}
		if err := engine.Validate(flow); err != nil {
			t.Fatalf("el validador rechaza el operador %q que el ejecutor implementa: %v", op, err)
		}
	}
	if ValidDataQueryOperator("regex") {
		t.Fatal("un operador inventado no puede pasar")
	}
}
