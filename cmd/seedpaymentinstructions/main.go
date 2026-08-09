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
	recordKey  = "venta_bawto"
	defaultISO = "PEN"
)

type field struct {
	key, label, typ string
	required        bool
}

func main() {
	_ = godotenv.Load()
	orgID := flag.String("org-id", "", "organización comercial propietaria")
	activate := flag.Bool("activate", false, "crea o reemplaza la instrucción activa")
	medium := flag.String("medium", "", "medio de pago, por ejemplo Yape o BCP")
	destination := flag.String("destination", "", "número, cuenta o destino exacto")
	holder := flag.String("holder", "", "titular exacto mostrado al comprador")
	note := flag.String("note", "", "nota opcional que se añade al mensaje")
	flag.Parse()
	if strings.TrimSpace(*orgID) == "" {
		panic("-org-id es obligatorio")
	}
	if *activate && (strings.TrimSpace(*medium) == "" || strings.TrimSpace(*destination) == "" || strings.TrimSpace(*holder) == "") {
		panic("-activate exige -medium, -destination y -holder")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	must(err)
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	must(err)
	defer tx.Rollback(ctx)

	objectID := ensureObject(ctx, tx, strings.TrimSpace(*orgID))
	ensureFields(ctx, tx, objectID)
	if *activate {
		message := paymentMessage(*medium, *destination, *holder, *note)
		data := map[string]any{
			"clave": recordKey, "medio": strings.TrimSpace(*medium),
			"destino": strings.TrimSpace(*destination), "titular": strings.TrimSpace(*holder),
			"moneda": defaultISO, "mensaje": message, "activo": true,
		}
		upsertActiveRecord(ctx, tx, objectID, data)
	}
	must(tx.Commit(ctx))

	if *activate {
		fmt.Printf("%s configurado con una instrucción activa\n", objectKey)
	} else {
		fmt.Printf("%s creado; falta ejecutar con -activate y datos reales\n", objectKey)
	}
}

func ensureObject(ctx context.Context, tx pgx.Tx, orgID string) string {
	var id string
	must(tx.QueryRow(ctx, `INSERT INTO data_objects(org_id,key,name,plural_name)
		VALUES($1::uuid,$2,'Instrucción de pago Bawto','Instrucciones de pago Bawto')
		ON CONFLICT(org_id,key) DO UPDATE SET name=EXCLUDED.name,plural_name=EXCLUDED.plural_name,updated_at=NOW()
		RETURNING id::text`, orgID, objectKey).Scan(&id))
	return id
}

func ensureFields(ctx context.Context, tx pgx.Tx, objectID string) {
	fields := []field{
		{"clave", "Clave", "text", true},
		{"medio", "Medio de pago", "text", true},
		{"destino", "Número o cuenta", "text", true},
		{"titular", "Titular", "text", true},
		{"moneda", "Moneda", "text", true},
		{"mensaje", "Mensaje exacto", "text", true},
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
}

func upsertActiveRecord(ctx context.Context, tx pgx.Tx, objectID string, data map[string]any) {
	raw, err := json.Marshal(data)
	must(err)
	var id string
	err = tx.QueryRow(ctx, `SELECT id::text FROM data_records
		WHERE object_id=$1::uuid AND data->>'clave'=$2
		ORDER BY updated_at DESC,id DESC LIMIT 1 FOR UPDATE`, objectID, recordKey).Scan(&id)
	if err == pgx.ErrNoRows {
		must(tx.QueryRow(ctx, `INSERT INTO data_records(object_id,data) VALUES($1::uuid,$2::jsonb) RETURNING id::text`, objectID, raw).Scan(&id))
	} else {
		must(err)
		_, err = tx.Exec(ctx, `UPDATE data_records SET data=$2::jsonb,updated_at=NOW() WHERE id=$1::uuid`, id, raw)
		must(err)
	}

	// Un registro duplicado creado desde el panel nunca puede competir con el
	// canónico. Se conserva para auditoría, pero queda inactivo en la misma tx.
	_, err = tx.Exec(ctx, `UPDATE data_records
		SET data=jsonb_set(data,'{activo}','false'::jsonb,true),updated_at=NOW()
		WHERE object_id=$1::uuid AND id<>$2::uuid AND data->>'clave'=$3
			AND COALESCE((data->>'activo')::boolean,false)=true`, objectID, id, recordKey)
	must(err)
}

func paymentMessage(medium, destination, holder, note string) string {
	message := fmt.Sprintf("Para pagar tu plan Bawto usa %s.\n\nDestino: %s\nTitular: %s\nMoneda: %s", strings.TrimSpace(medium), strings.TrimSpace(destination), strings.TrimSpace(holder), defaultISO)
	if trimmed := strings.TrimSpace(note); trimmed != "" {
		message += "\n" + trimmed
	}
	return message + "\n\nCuando termines, envía una captura completa y nítida donde se vean la fecha, el importe y la operación. El acceso se activará sujeto a verificación posterior."
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
