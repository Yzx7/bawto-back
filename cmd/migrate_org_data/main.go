// migrate_org_data migra los datos que antes pertenecían a bots
// para que pertenezcan a organizaciones. Ejecutar desde backend:
//
//	go run ./cmd/migrate_org_data
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
)

var affectedTables = []string{"contacts", "contact_fields", "audiences", "data_objects"}

func main() {
	inspectOnly := len(os.Args) == 2 && os.Args[1] == "--inspect"
	initialize := len(os.Args) == 2 && os.Args[1] == "--initialize"
	seedInvoices := len(os.Args) == 2 && os.Args[1] == "--seed-invoices"
	inspectBOTI := len(os.Args) == 2 && os.Args[1] == "--inspect-boti"
	configureBOTI := len(os.Args) == 2 && os.Args[1] == "--configure-boti-invoices"
	_ = godotenv.Load()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		panic("DATABASE_URL no está configurada")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		panic(fmt.Errorf("conectando a la base: %w", err))
	}
	defer conn.Close(ctx)
	if inspectBOTI {
		rows, err := conn.Query(ctx, `SELECT id::text,name,flow FROM bots WHERE lower(name)='boti'`)
		if err != nil {
			panic(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, name string
			var flow []byte
			if err := rows.Scan(&id, &name, &flow); err != nil {
				panic(err)
			}
			fmt.Printf("%s %s %s\n", id, name, flow)
		}
		return
	}
	if configureBOTI {
		configureBOTIInvoices(ctx, conn)
		return
	}
	if initialize {
		sql, err := os.ReadFile(filepath.Join("db", "schema.sql"))
		if err != nil {
			panic(err)
		}
		if _, err := conn.Exec(ctx, strings.TrimSpace(string(sql))); err != nil {
			panic(fmt.Errorf("inicializando esquema: %w", err))
		}
		fmt.Println("Esquema de datos inicializado correctamente.")
		return
	}
	if seedInvoices {
		seedInvoiceData(ctx, conn)
		return
	}

	tablesWithBotID := 0
	for _, table := range affectedTables {
		var hasBotID, hasOrgID bool
		if err = conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name='bot_id'), EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name='org_id')`, table).Scan(&hasBotID, &hasOrgID); err != nil {
			panic(err)
		}
		fmt.Printf("%s: bot_id=%t org_id=%t\n", table, hasBotID, hasOrgID)
		if inspectOnly {
			continue
		}
		if !hasBotID && !hasOrgID {
			panic(fmt.Sprintf("esquema parcial no soportado en %s; no se aplicó ningún cambio", table))
		}
		if hasBotID {
			tablesWithBotID++
		}
	}
	if inspectOnly {
		rows, err := conn.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema='public' ORDER BY table_name`)
		if err != nil {
			panic(err)
		}
		defer rows.Close()
		fmt.Print("tablas públicas: ")
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				panic(err)
			}
			fmt.Print(table + " ")
		}
		fmt.Println()
		return
	}
	if tablesWithBotID == 0 {
		fmt.Println("La migración ya está aplicada; no se hicieron cambios.")
		return
	}
	if tablesWithBotID != len(affectedTables) {
		panic("el esquema está parcialmente migrado; no se aplicó ningún cambio")
	}

	for _, query := range []string{
		`SELECT b.org_id::text, c.phone_normalized, COUNT(*) FROM contacts c JOIN bots b ON b.id=c.bot_id GROUP BY b.org_id,c.phone_normalized HAVING COUNT(*) > 1`,
		`SELECT b.org_id::text, f.key, COUNT(*) FROM contact_fields f JOIN bots b ON b.id=f.bot_id GROUP BY b.org_id,f.key HAVING COUNT(*) > 1`,
		`SELECT b.org_id::text, d.key, COUNT(*) FROM data_objects d JOIN bots b ON b.id=d.bot_id GROUP BY b.org_id,d.key HAVING COUNT(*) > 1`,
	} {
		rows, err := conn.Query(ctx, query)
		if err != nil {
			panic(err)
		}
		if rows.Next() {
			rows.Close()
			panic("hay claves duplicadas entre bots de una misma organización; resuélvelas antes de migrar")
		}
		rows.Close()
	}

	sql, err := os.ReadFile(filepath.Join("db", "migrations", "001_data_ownership_org.sql"))
	if err != nil {
		panic(err)
	}
	if _, err := conn.Exec(ctx, strings.TrimSpace(string(sql))); err != nil {
		panic(fmt.Errorf("la migración falló: %w", err))
	}
	fmt.Println("Migración aplicada correctamente.")
}

func configureBOTIInvoices(ctx context.Context, conn *pgx.Conn) {
	var botID string
	var raw []byte
	if err := conn.QueryRow(ctx, `SELECT id::text,flow FROM bots WHERE lower(name)='boti'`).Scan(&botID, &raw); err != nil {
		panic("no se encontró BOTI")
	}
	var flow map[string]any
	if err := json.Unmarshal(raw, &flow); err != nil {
		panic(err)
	}
	nodes, ok := flow["nodes"].([]any)
	if !ok {
		panic("flujo BOTI inválido")
	}
	for _, node := range nodes {
		if n, ok := node.(map[string]any); ok && n["id"] == "n_tiene_factura" {
			fmt.Println("BOTI ya usa Facturas.")
			return
		}
	}
	flow["nodes"] = append(nodes,
		map[string]any{"id": "n_tiene_factura", "kind": "condition", "expression": "data_facturas_numero", "pos": map[string]any{"x": 230, "y": -150}},
		map[string]any{"id": "n_resumen_factura", "kind": "send", "body": "📄 Encontré tu factura *{data_facturas_numero}* del período {data_facturas_periodo}.\nImporte: *{data_facturas_importe} {data_facturas_moneda}*\nVence: *{data_facturas_vencimiento}*\nEstado: *{data_facturas_estado}*.", "pos": map[string]any{"x": -120, "y": -80}},
	)
	edges, ok := flow["edges"].([]any)
	if !ok {
		panic("flujo BOTI inválido")
	}
	filtered := make([]any, 0, len(edges)+3)
	for _, edge := range edges {
		if e, ok := edge.(map[string]any); !ok || e["source"] != "trigger" {
			filtered = append(filtered, edge)
		}
	}
	flow["edges"] = append(filtered,
		map[string]any{"id": "e_trigger_data", "source": "trigger", "target": "n_tiene_factura"},
		map[string]any{"id": "e_data_yes", "source": "n_tiene_factura", "target": "n_resumen_factura", "sourceHandle": "true"},
		map[string]any{"id": "e_data_no", "source": "n_tiene_factura", "target": "n_bienvenida", "sourceHandle": "false"},
		map[string]any{"id": "e_data_menu", "source": "n_resumen_factura", "target": "n_espera_opcion"},
	)
	updated, err := json.Marshal(flow)
	if err != nil {
		panic(err)
	}
	if _, err := conn.Exec(ctx, `UPDATE bots SET flow=$2::jsonb,updated_at=NOW() WHERE id=$1::uuid`, botID, updated); err != nil {
		panic(err)
	}
	fmt.Println("BOTI ahora consulta Facturas al iniciar una conversación.")
}

func seedInvoiceData(ctx context.Context, conn *pgx.Conn) {
	rows, err := conn.Query(ctx, `SELECT id::text FROM data_objects WHERE lower(key)='facturas' OR lower(name)='facturas' ORDER BY created_at`)
	if err != nil {
		panic(err)
	}
	objectIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		panic(err)
	}
	if len(objectIDs) > 1 {
		panic("hay más de un objeto Facturas; indica cuál debe poblarse")
	}
	objectID := ""
	if len(objectIDs) == 1 {
		objectID = objectIDs[0]
	} else {
		orgRows, err := conn.Query(ctx, `SELECT id::text FROM organizations ORDER BY created_at`)
		if err != nil {
			panic(err)
		}
		orgIDs, err := pgx.CollectRows(orgRows, pgx.RowTo[string])
		if err != nil {
			panic(err)
		}
		if len(orgIDs) != 1 {
			panic("no se encontró Facturas y hay cero o varias organizaciones; selecciona la organización antes de crear datos")
		}
		objectID = "create:" + orgIDs[0]
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		panic(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if strings.HasPrefix(objectID, "create:") {
		orgID := strings.TrimPrefix(objectID, "create:")
		if err := tx.QueryRow(ctx, `INSERT INTO data_objects(org_id,key,name,plural_name) VALUES($1::uuid,'facturas','Factura','Facturas') RETURNING id::text`, orgID).Scan(&objectID); err != nil {
			panic(err)
		}
	}
	fields := []struct {
		key, label, typ string
		required        bool
	}{
		{"numero", "Número de factura", "text", true},
		{"periodo", "Período", "text", true},
		{"vencimiento", "Fecha de vencimiento", "date", true},
		{"importe", "Importe", "number", true},
		{"moneda", "Moneda", "text", true},
		{"estado", "Estado", "text", true},
	}
	for _, field := range fields {
		if _, err := tx.Exec(ctx, `INSERT INTO data_fields(object_id,key,label,type,required) VALUES($1::uuid,$2,$3,$4,$5) ON CONFLICT(object_id,key) DO UPDATE SET label=EXCLUDED.label,type=EXCLUDED.type,required=EXCLUDED.required`, objectID, field.key, field.label, field.typ, field.required); err != nil {
			panic(err)
		}
	}
	records := []map[string]any{
		{"numero": "FAC-2026-001", "periodo": "2026-07", "vencimiento": "2026-07-22", "importe": 89.90, "moneda": "PEN", "estado": "pendiente"},
		{"numero": "FAC-2026-002", "periodo": "2026-07", "vencimiento": "2026-07-25", "importe": 120.00, "moneda": "PEN", "estado": "pendiente"},
		{"numero": "FAC-2026-003", "periodo": "2026-07", "vencimiento": "2026-07-15", "importe": 75.50, "moneda": "PEN", "estado": "pagada"},
	}
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			panic(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO data_records(object_id,data) SELECT $1::uuid,$2::jsonb WHERE NOT EXISTS (SELECT 1 FROM data_records WHERE object_id=$1::uuid AND data->>'numero'=$3)`, objectID, data, record["numero"]); err != nil {
			panic(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		panic(err)
	}
	fmt.Println("Facturas estructurado: 6 campos y 3 registros de ejemplo disponibles.")
}
