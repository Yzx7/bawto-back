package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

const (
	objectKey  = "instrucciones_pago_bawto"
	defaultISO = "PEN"
)

type field struct {
	key, label, typ string
	required        bool
}

func main() {
	_ = godotenv.Load()
	orgID := flag.String("org-id", "", "organización comercial propietaria")
	mode := flag.String("mode", "schema", "schema, upsert o deactivate")
	key := flag.String("key", "", "clave estable del método, por ejemplo yape o bcp_cuenta")
	medium := flag.String("medium", "", "nombre visible, por ejemplo Yape o Cuenta BCP")
	destination := flag.String("destination", "", "número, cuenta o CCI exacto")
	holder := flag.String("holder", "", "titular exacto mostrado al comprador")
	currency := flag.String("currency", defaultISO, "moneda ISO 4217")
	note := flag.String("note", "", "nota opcional para este método")
	priority := flag.Int("priority", 100, "orden ascendente; el menor aparece primero")
	flag.Parse()

	*orgID = strings.TrimSpace(*orgID)
	*mode = strings.TrimSpace(*mode)
	*key = strings.TrimSpace(*key)
	if *orgID == "" {
		panic("-org-id es obligatorio")
	}
	if *mode != "schema" && *mode != "upsert" && *mode != "deactivate" {
		panic("-mode debe ser schema, upsert o deactivate")
	}
	if (*mode == "upsert" || *mode == "deactivate") && !validMethodKey(*key) {
		panic("-key debe usar minúsculas, números o guion bajo y empezar con letra")
	}
	if *mode == "upsert" && (strings.TrimSpace(*medium) == "" || strings.TrimSpace(*destination) == "" || strings.TrimSpace(*holder) == "") {
		panic("-mode upsert exige -medium, -destination y -holder")
	}
	if *priority < 0 || *priority > 9999 {
		panic("-priority debe estar entre 0 y 9999")
	}
	*currency = strings.ToUpper(strings.TrimSpace(*currency))
	if len(*currency) != 3 {
		panic("-currency debe ser un código ISO 4217 de tres letras")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	must(err)
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	must(err)
	defer tx.Rollback(ctx)

	objectID := ensureObject(ctx, tx, *orgID)
	ensureFields(ctx, tx, objectID)
	switch *mode {
	case "upsert":
		data := map[string]any{
			"clave": *key, "medio": strings.TrimSpace(*medium),
			"destino": strings.TrimSpace(*destination), "titular": strings.TrimSpace(*holder),
			"moneda": *currency, "nota": strings.TrimSpace(*note),
			"prioridad": *priority, "activo": true,
		}
		upsertMethod(ctx, tx, objectID, *key, data)
	case "deactivate":
		deactivateMethod(ctx, tx, objectID, *key)
	}
	must(tx.Commit(ctx))
	fmt.Printf("object=%s id=%s mode=%s", objectKey, objectID, *mode)
	if *key != "" {
		fmt.Printf(" method=%s", *key)
	}
	fmt.Println()
}

func ensureObject(ctx context.Context, tx pgx.Tx, orgID string) string {
	var id string
	must(tx.QueryRow(ctx, `INSERT INTO data_objects(org_id,key,name,plural_name)
		VALUES($1::uuid,$2,'Método de pago Bawto','Métodos de pago Bawto')
		ON CONFLICT(org_id,key) DO UPDATE SET name=EXCLUDED.name,plural_name=EXCLUDED.plural_name,updated_at=NOW()
		RETURNING id::text`, orgID, objectKey).Scan(&id))
	return id
}

func ensureFields(ctx context.Context, tx pgx.Tx, objectID string) {
	fields := []field{
		{"clave", "Clave", "text", true},
		{"medio", "Medio de pago", "text", true},
		{"destino", "Número, cuenta o CCI", "text", true},
		{"titular", "Titular", "text", true},
		{"moneda", "Moneda", "text", true},
		{"nota", "Nota", "text", false},
		{"prioridad", "Prioridad", "number", true},
		{"activo", "Activo", "boolean", true},
	}
	for _, item := range fields {
		_, err := tx.Exec(ctx, `INSERT INTO data_fields(object_id,key,label,type,required)
			VALUES($1::uuid,$2,$3,$4,$5)
			ON CONFLICT(object_id,key) DO UPDATE
			SET label=EXCLUDED.label,type=EXCLUDED.type,required=EXCLUDED.required,updated_at=NOW()`,
			objectID, item.key, item.label, item.typ, item.required)
		must(err)
	}
	// La primera versión guardaba un único mensaje agregado. Solo se retira ese
	// campo si la tabla sigue vacía; nunca se destruye configuración del dueño.
	_, err := tx.Exec(ctx, `DELETE FROM data_fields WHERE object_id=$1::uuid AND key='mensaje'
		AND NOT EXISTS (SELECT 1 FROM data_records WHERE object_id=$1::uuid)`, objectID)
	must(err)
}

func upsertMethod(ctx context.Context, tx pgx.Tx, objectID, key string, data map[string]any) {
	raw, err := json.Marshal(data)
	must(err)
	var id string
	err = tx.QueryRow(ctx, `SELECT id::text FROM data_records
		WHERE object_id=$1::uuid AND data->>'clave'=$2
		ORDER BY updated_at DESC,id DESC LIMIT 1 FOR UPDATE`, objectID, key).Scan(&id)
	if err == pgx.ErrNoRows {
		must(tx.QueryRow(ctx, `INSERT INTO data_records(object_id,data) VALUES($1::uuid,$2::jsonb) RETURNING id::text`, objectID, raw).Scan(&id))
	} else {
		must(err)
		_, err = tx.Exec(ctx, `UPDATE data_records SET data=$2::jsonb,updated_at=NOW() WHERE id=$1::uuid`, id, raw)
		must(err)
	}
	// Duplicados de la misma clave se conservan para auditoría, pero no compiten.
	_, err = tx.Exec(ctx, `UPDATE data_records
		SET data=jsonb_set(data,'{activo}','false'::jsonb,true),updated_at=NOW()
		WHERE object_id=$1::uuid AND id<>$2::uuid AND data->>'clave'=$3
			AND COALESCE((data->>'activo')::boolean,false)=true`, objectID, id, key)
	must(err)
}

func deactivateMethod(ctx context.Context, tx pgx.Tx, objectID, key string) {
	tag, err := tx.Exec(ctx, `UPDATE data_records
		SET data=jsonb_set(data,'{activo}','false'::jsonb,true),updated_at=NOW()
		WHERE object_id=$1::uuid AND data->>'clave'=$2
			AND COALESCE((data->>'activo')::boolean,false)=true`, objectID, key)
	must(err)
	if tag.RowsAffected() == 0 {
		panic("no existe un método activo con clave " + key)
	}
}

func validMethodKey(value string) bool {
	if value == "" || value[0] < 'a' || value[0] > 'z' || len(value) > 63 {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
