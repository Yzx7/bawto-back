package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

type field struct {
	key, label, typ string
	required        bool
}

func main() {
	_ = godotenv.Load()
	orgID := flag.String("org-id", "", "organización comercial propietaria")
	flag.Parse()
	if *orgID == "" {
		panic("-org-id es obligatorio")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	must(err)
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	must(err)
	defer tx.Rollback(ctx)

	plansID := ensureObject(ctx, tx, *orgID, "planes_bawto", "Plan Bawto", "Planes Bawto")
	ensureFields(ctx, tx, plansID, []field{
		{"clave", "Clave", "text", true}, {"nombre", "Nombre", "text", true},
		{"precio_mensual", "Precio mensual", "number", true},
		{"precio_trimestral_mensual", "Precio trimestral por mes", "number", true},
		{"monto_trimestral", "Monto trimestral", "number", true},
		{"moneda", "Moneda", "text", true}, {"llamadas", "Llamadas de IA", "text", true},
		{"beneficios", "Beneficios", "text", true}, {"activo", "Activo", "boolean", true},
	})
	plans := []map[string]any{
		{"clave": "inicio", "nombre": "Inicio", "precio_mensual": 22, "precio_trimestral_mensual": 20, "monto_trimestral": 60, "moneda": "PEN", "llamadas": "Aproximadamente 1,000 llamadas de IA", "beneficios": "Agente personalizable|Flujos visuales|Datos y conversaciones", "activo": true},
		{"clave": "base", "nombre": "Base", "precio_mensual": 28, "precio_trimestral_mensual": 26, "monto_trimestral": 78, "moneda": "PEN", "llamadas": "Aproximadamente 1,500 llamadas de IA", "beneficios": "Agente personalizable|Flujos visuales|Panel de uso y costos", "activo": true},
		{"clave": "crece", "nombre": "Crece", "precio_mensual": 35, "precio_trimestral_mensual": 34, "monto_trimestral": 102, "moneda": "PEN", "llamadas": "Aproximadamente 1,900 llamadas de IA", "beneficios": "Agente personalizable|Herramientas y datos|Decisiones trazables", "activo": true},
		{"clave": "pro", "nombre": "Pro", "precio_mensual": 50, "precio_trimestral_mensual": 48, "monto_trimestral": 144, "moneda": "PEN", "llamadas": "Aproximadamente 3,000 llamadas de IA", "beneficios": "Agente personalizable|Operación conectada|Panel de uso y costos", "activo": true},
	}
	for _, plan := range plans {
		upsertRecord(ctx, tx, plansID, "clave", plan["clave"].(string), plan)
	}

	subsID := ensureObject(ctx, tx, *orgID, "suscripciones_bawto", "Suscripción Bawto", "Suscripciones Bawto")
	ensureFields(ctx, tx, subsID, []field{
		{"organizacion_id", "Organización ID", "text", true}, {"codigo_activacion", "Código de activación", "text", true},
		{"plan_clave", "Clave del plan", "text", true}, {"plan_nombre", "Plan", "text", true},
		{"ciclo", "Ciclo", "text", true}, {"monto", "Monto", "number", true}, {"moneda", "Moneda", "text", true},
		{"telefono", "Teléfono comprador", "text", false}, {"cobro_id", "Cobro ID", "text", false},
		{"operacion", "Operación", "text", false}, {"estado", "Estado", "text", true},
		{"vigente_desde", "Vigente desde", "text", true}, {"vigente_hasta", "Vigente hasta", "text", true},
		{"cancelado_en", "Cancelado en", "text", false}, {"motivo_anulacion", "Motivo de anulación", "text", false},
		{"beneficios", "Beneficios", "text", false},
	})

	must(tx.Commit(ctx))
	fmt.Printf("planes_bawto=%s suscripciones_bawto=%s\n", plansID, subsID)
}

func ensureObject(ctx context.Context, tx pgx.Tx, orgID, key, name, plural string) string {
	var id string
	must(tx.QueryRow(ctx, `INSERT INTO data_objects(org_id,key,name,plural_name) VALUES($1::uuid,$2,$3,$4)
		ON CONFLICT(org_id,key) DO UPDATE SET name=EXCLUDED.name,plural_name=EXCLUDED.plural_name,updated_at=NOW()
		RETURNING id::text`, orgID, key, name, plural).Scan(&id))
	return id
}

func ensureFields(ctx context.Context, tx pgx.Tx, objectID string, fields []field) {
	for _, item := range fields {
		_, err := tx.Exec(ctx, `INSERT INTO data_fields(object_id,key,label,type,required) VALUES($1::uuid,$2,$3,$4,$5)
			ON CONFLICT(object_id,key) DO UPDATE SET label=EXCLUDED.label,type=EXCLUDED.type,required=EXCLUDED.required,updated_at=NOW()`,
			objectID, item.key, item.label, item.typ, item.required)
		must(err)
	}
}

func upsertRecord(ctx context.Context, tx pgx.Tx, objectID, key, value string, data map[string]any) {
	raw, err := json.Marshal(data)
	must(err)
	var id string
	err = tx.QueryRow(ctx, `SELECT id::text FROM data_records WHERE object_id=$1::uuid AND data->>$2=$3 ORDER BY created_at DESC LIMIT 1`, objectID, key, value).Scan(&id)
	if err == pgx.ErrNoRows {
		_, err = tx.Exec(ctx, `INSERT INTO data_records(object_id,data) VALUES($1::uuid,$2::jsonb)`, objectID, raw)
	} else if err == nil {
		_, err = tx.Exec(ctx, `UPDATE data_records SET data=$2::jsonb,updated_at=NOW() WHERE id=$1::uuid`, id, raw)
	}
	must(err)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
