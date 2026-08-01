package models

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Regresión de dos fallos que solo aparecen contra Postgres de verdad y que se
// descubrieron al montar el piloto D-3 (§19, pasos 5 a 9):
//
//  1. `PrimaryContactForRecord` y `ResolveAudience` seleccionaban la lista de
//     columnas de contacto sin prefijo dentro de un JOIN. `created_at` existe
//     también en `data_records`, `data_objects` y `audience_contacts`, así que
//     Postgres respondía "column reference is ambiguous" en ejecución. Ninguna
//     de las dos había fallado antes porque en producción no había un solo
//     vínculo registro→contacto ni una audiencia manual.
//
//  2. El vínculo usaba `ON CONFLICT DO NOTHING` y luego trataba
//     `RowsAffected() == 0` como error de propiedad: volver a vincular el mismo
//     par decía "registro o contacto no pertenece al bot".
//
//  3. `LinkRecordContactByOrg` —la versión que sobrevivió— se había quedado con
//     un `ON CONFLICT` de dos columnas cuando la clave primaria de
//     `data_record_contacts` es de tres. Postgres rechazaba la sentencia con
//     42P10 ("there is no unique or exclusion constraint matching the ON
//     CONFLICT specification"), así que vincular fallaba desde el primer intento,
//     no solo al repetir.
//
//  4. `ListDataRecordsByOrg` no leía `data_record_contacts`, así que la pantalla
//     de datos mostraba "Contacto vinculado" y al recargar el selector volvía a
//     estar vacío: el vínculo se guardaba y nadie lo devolvía nunca.
//
//  5. La clave primaria es `(record_id, contact_id, role)`, así que elegir otro
//     contacto insertaba un segundo `primary` en vez de reemplazar. Con dos,
//     `PrimaryContactForRecord` elegía uno arbitrariamente y el recordatorio podía
//     irse al contacto viejo.
//
//     DATABASE_URL=... go test ./models -run VinculoRegistroContacto -v
func TestVinculoRegistroContactoResuelveDestinatario(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "link_")
	otro := botDePrueba(t, ctx, pool, "link_otro_")

	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "pedidos_link", "Pedido", "Pedidos")
	if err != nil {
		t.Fatalf("CreateDataObject: %v", err)
	}
	if _, err = UpsertDataFieldByOrg(ctx, pool, bot.OrgID, object.ID, "total", "Total", "text", false); err != nil {
		t.Fatalf("UpsertDataField: %v", err)
	}
	record, err := CreateDataRecordByOrg(ctx, pool, bot.OrgID, object.ID, json.RawMessage(`{"total":"120"}`))
	if err != nil {
		t.Fatalf("CreateDataRecord: %v", err)
	}
	contact, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51900111222", "Destinatario", "active", nil)
	if err != nil {
		t.Fatalf("UpsertContact: %v", err)
	}

	// Vincular dos veces es una operación normal: el operador reabre la ficha y
	// vuelve a guardar. La segunda vez no puede ser un error.
	if err := LinkRecordContactByOrg(ctx, pool, bot.OrgID, record.ID, contact.ID, "primary"); err != nil {
		t.Fatalf("primer vínculo: %v", err)
	}
	if err := LinkRecordContactByOrg(ctx, pool, bot.OrgID, record.ID, contact.ID, "primary"); err != nil {
		t.Fatalf("revincular debería ser idempotente: %v", err)
	}
	// Un registro ajeno sí tiene que fallar: es el aislamiento entre orgs.
	if err := LinkRecordContactByOrg(ctx, pool, otro.OrgID, record.ID, contact.ID, "primary"); err == nil {
		t.Error("vincular desde otra organización debería fallar")
	}

	got, err := PrimaryContactForRecord(ctx, pool, bot.ID, record.ID)
	if err != nil {
		t.Fatalf("PrimaryContactForRecord: %v", err)
	}
	if got == nil || got.ID != contact.ID || got.PhoneNormalized != "51900111222" {
		t.Fatalf("destinatario inesperado: %+v", got)
	}
	if ajeno, err := PrimaryContactForRecord(ctx, pool, otro.ID, record.ID); err != nil || ajeno != nil {
		t.Fatalf("el registro no debería verse desde otro bot: %+v (%v)", ajeno, err)
	}

	// Lo que la pantalla necesita para no perder el vínculo al recargar.
	listado := registroEnLista(t, ctx, pool, bot.OrgID, object.ID, record.ID)
	if listado.ContactID == nil || *listado.ContactID != contact.ID {
		t.Fatalf("la lista no devolvió el contacto vinculado: %+v", listado)
	}
	if listado.ContactPhone == nil || *listado.ContactPhone != "51900111222" {
		t.Fatalf("teléfono del contacto ausente en la lista: %+v", listado)
	}

	// Corregir el contacto reemplaza; no deja dos primarios compitiendo.
	otroContacto, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51900555666", "Corregido", "active", nil)
	if err != nil {
		t.Fatalf("SaveContactByOrg: %v", err)
	}
	if err := LinkRecordContactByOrg(ctx, pool, bot.OrgID, record.ID, otroContacto.ID, "primary"); err != nil {
		t.Fatalf("revincular a otro contacto: %v", err)
	}
	var primarios int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM data_record_contacts
		WHERE record_id=$1::uuid AND role='primary'`, record.ID).Scan(&primarios); err != nil {
		t.Fatalf("contar primarios: %v", err)
	}
	if primarios != 1 {
		t.Fatalf("un registro debe tener un solo contacto primario, hay %d", primarios)
	}
	if got, err := PrimaryContactForRecord(ctx, pool, bot.ID, record.ID); err != nil || got == nil || got.ID != otroContacto.ID {
		t.Fatalf("el destinatario debería ser el contacto corregido: %+v (%v)", got, err)
	}

	// Desvincular deja el registro en la lista, sin contacto: es el estado que el
	// operador tiene que poder ver para saber que le falta vincularlo.
	if err := UnlinkRecordContactByOrg(ctx, pool, bot.OrgID, record.ID, otroContacto.ID); err != nil {
		t.Fatalf("UnlinkRecordContactByOrg: %v", err)
	}
	suelto := registroEnLista(t, ctx, pool, bot.OrgID, object.ID, record.ID)
	if suelto.ContactID != nil {
		t.Fatalf("el vínculo no se eliminó: %+v", suelto)
	}
	if err := UnlinkRecordContactByOrg(ctx, pool, bot.OrgID, record.ID, otroContacto.ID); err == nil {
		t.Error("desvincular lo que ya no está vinculado debería fallar")
	}
}

func registroEnLista(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, objectID, recordID string) DataRecordWithContact {
	t.Helper()
	records, err := ListDataRecordsByOrg(ctx, pool, orgID, objectID)
	if err != nil {
		t.Fatalf("ListDataRecordsByOrg: %v", err)
	}
	for _, r := range records {
		if r.ID == recordID {
			return r
		}
	}
	t.Fatalf("el registro %s desapareció de la lista (%d registros)", recordID, len(records))
	return DataRecordWithContact{}
}

// Misma clase de fallo en la otra consulta con JOIN sobre contactos.
func TestAudienciaManualResuelveContactos(t *testing.T) {
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "aud_")

	audience, err := CreateAudience(ctx, pool, bot.ID, "Piloto", "manual", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CreateAudience: %v", err)
	}
	contact, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51900333444", "Miembro", "active", nil)
	if err != nil {
		t.Fatalf("UpsertContact: %v", err)
	}
	if err := AddAudienceContact(ctx, pool, bot.ID, audience.ID, contact.ID); err != nil {
		t.Fatalf("AddAudienceContact: %v", err)
	}

	miembros, err := ResolveAudience(ctx, pool, bot.ID, audience.ID)
	if err != nil {
		t.Fatalf("ResolveAudience: %v", err)
	}
	if len(miembros) != 1 || miembros[0].ID != contact.ID {
		t.Fatalf("audiencia inesperada: %+v", miembros)
	}
}
