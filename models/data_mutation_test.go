package models

import "testing"

func TestNormalizeMutationValuesUsesDeclaredTypes(t *testing.T) {
	fields := []DataField{
		{Key: "monto", Label: "Monto", Type: "number"},
		{Key: "activo", Label: "Activo", Type: "boolean"},
		{Key: "nota", Label: "Nota", Type: "text"},
	}
	values, err := normalizeMutationValues(fields, map[string]any{
		"monto": "125.00", "activo": "true", "nota": "visible",
	})
	if err != nil {
		t.Fatal(err)
	}
	if values["monto"] != float64(125) || values["activo"] != true || values["nota"] != "visible" {
		t.Fatalf("valores normalizados inesperados: %#v", values)
	}

	empty, err := normalizeMutationValues(fields, map[string]any{"monto": "", "activo": nil})
	if err != nil || empty["monto"] != "" || empty["activo"] != nil {
		t.Fatalf("los opcionales vacíos deben conservarse: %#v err=%v", empty, err)
	}
	if _, err := normalizeMutationValues(fields, map[string]any{"monto": "ciento"}); err == nil {
		t.Fatal("se esperaba rechazar un número inválido")
	}
}

// Requiere DATABASE_URL con migración 014. Comprueba la frontera que no cubre
// el motor: ownership, schema, vínculo y ledger idempotente en una transacción.
func TestMutateDataRecordIntegration(t *testing.T) {
	pool, ctx := flowTestPool(t)
	var table *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.data_record_mutations')::text`).Scan(&table); err != nil || table == nil {
		t.Skip("migración 014_data_record_mutations no aplicada")
	}
	bot := botDePrueba(t, ctx, pool, "mutate_")
	object, err := CreateDataObjectByOrg(ctx, pool, bot.OrgID, "cobros", "Cobro", "Cobros")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []struct {
		key, label, typ string
		required        bool
	}{
		{"operacion", "Operación", "text", true},
		{"monto", "Monto", "number", false},
		{"estado", "Estado", "text", true},
	} {
		if _, err = UpsertDataFieldByOrg(ctx, pool, bot.OrgID, object.ID, field.key, field.label, field.typ, field.required); err != nil {
			t.Fatal(err)
		}
	}
	contact, err := SaveContactByOrg(ctx, pool, bot.OrgID, "", "51900888777", "Ana", "active", nil)
	if err != nil {
		t.Fatal(err)
	}

	input := DataMutationInput{
		OrgID: bot.OrgID, ObjectKey: "cobros", Operation: "create",
		Values:              map[string]any{"operacion": "OP-1", "monto": "10.50", "estado": "pendiente"},
		CurrentContactPhone: contact.PhoneNormalized, LinkCurrentContact: true, IdempotencyKey: "message:1",
	}
	created, err := MutateDataRecord(ctx, pool, input)
	if err != nil || !created.Created || created.Idempotent {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	repeated, err := MutateDataRecord(ctx, pool, input)
	if err != nil || repeated.RecordID != created.RecordID || !repeated.Idempotent {
		t.Fatalf("repeat=%+v err=%v", repeated, err)
	}

	upserted, err := MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID: bot.OrgID, ObjectKey: "cobros", Operation: "upsert",
		MatchField: "operacion", MatchValue: "OP-1", Values: map[string]any{"monto": "20.00"},
		IdempotencyKey: "message:2",
	})
	if err != nil || upserted.Created || upserted.RecordID != created.RecordID {
		t.Fatalf("upsert=%+v err=%v", upserted, err)
	}

	var amount, amountType, linkedContact string
	if err = pool.QueryRow(ctx, `SELECT r.data->>'monto',jsonb_typeof(r.data->'monto'),rc.contact_id::text FROM data_records r
		JOIN data_record_contacts rc ON rc.record_id=r.id AND rc.role='primary' WHERE r.id=$1::uuid`,
		created.RecordID).Scan(&amount, &amountType, &linkedContact); err != nil || amount != "20" || amountType != "number" || linkedContact != contact.ID {
		t.Fatalf("monto=%s type=%s contact=%s err=%v", amount, amountType, linkedContact, err)
	}

	input.IdempotencyKey = "message:3"
	input.Values["campo_inventado"] = "x"
	if _, err = MutateDataRecord(ctx, pool, input); err == nil {
		t.Fatal("se esperaba rechazar un campo fuera del schema")
	}
}
