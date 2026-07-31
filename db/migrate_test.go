package db

import (
	"strings"
	"testing"
)

// Las migraciones embebidas deben cargarse en orden, sin versiones repetidas y
// con checksum estable: el migrador aborta si un archivo aplicado cambia.
func TestLoadMigrations(t *testing.T) {
	list, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	if len(list) < 4 {
		t.Fatalf("se esperaban al menos 4 migraciones, hay %d", len(list))
	}
	seen := map[int]bool{}
	prev := 0
	for _, m := range list {
		if m.Version <= prev {
			t.Fatalf("migraciones desordenadas: %d después de %d", m.Version, prev)
		}
		if seen[m.Version] {
			t.Fatalf("versión duplicada: %d", m.Version)
		}
		seen[m.Version] = true
		prev = m.Version
		if len(m.Checksum) != 64 {
			t.Fatalf("checksum inesperado en %s: %q", m.File, m.Checksum)
		}
	}
}

// Las tres migraciones históricas se aplicaron a mano y su efecto ya está en
// schema.sql: deben quedar marcadas como baseline o el migrador intentaría
// re-ejecutarlas y fallaría (001 referencia columnas que ella misma borra).
func TestMigracionesHistoricasSonBaseline(t *testing.T) {
	list, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	for _, m := range list {
		if m.Version <= 3 && !m.Baseline {
			t.Fatalf("la migración histórica %s debería ser baseline", m.File)
		}
		if m.Version > 3 && m.Baseline {
			t.Fatalf("una migración nueva (%s) no puede ser baseline", m.File)
		}
	}
}

// El checksum es del contenido: dos archivos distintos no pueden coincidir.
func TestChecksumsDistintosPorArchivo(t *testing.T) {
	list, _ := LoadMigrations()
	sums := map[string]string{}
	for _, m := range list {
		if other, dup := sums[m.Checksum]; dup {
			t.Fatalf("%s y %s comparten checksum", other, m.File)
		}
		sums[m.Checksum] = m.File
	}
}

func TestParseMigrationName(t *testing.T) {
	if v, n, err := parseMigrationName("004_flows.sql"); err != nil || v != 4 || n != "flows" {
		t.Fatalf("004_flows.sql → v=%d n=%q err=%v", v, n, err)
	}
	for _, bad := range []string{"flows.sql", "abc_flows.sql", "000_cero.sql"} {
		if _, _, err := parseMigrationName(bad); err == nil {
			t.Fatalf("%q debería ser inválido", bad)
		}
	}
}

// Una migración que abre su propia transacción rompería el aislamiento por
// archivo: el COMMIT interno cerraría la transacción del migrador.
func TestRejectOwnTransaction(t *testing.T) {
	bad := Migration{Version: 9, Name: "x", SQL: "BEGIN;\nALTER TABLE t ADD COLUMN c INT;\nCOMMIT;"}
	if err := rejectOwnTransaction(bad); err == nil {
		t.Fatal("se esperaba rechazo de BEGIN/COMMIT propios")
	}
	good := Migration{Version: 9, Name: "x", SQL: "ALTER TABLE t ADD COLUMN c INT;"}
	if err := rejectOwnTransaction(good); err != nil {
		t.Fatalf("migración normal rechazada: %v", err)
	}
	// Las migraciones no-baseline vigentes tienen que pasar este filtro, porque
	// son las únicas que el migrador llega a ejecutar.
	list, _ := LoadMigrations()
	for _, m := range list {
		if m.Baseline {
			continue
		}
		if err := rejectOwnTransaction(m); err != nil {
			t.Fatalf("%s: %v", m.File, err)
		}
	}
}

// La migración de la fase 1 tiene dos decisiones razonadas en el plan que un
// refactor podría deshacer sin darse cuenta.
func TestMigracionFlowsRespetaElPlan(t *testing.T) {
	list, _ := LoadMigrations()
	var sql string
	for _, m := range list {
		if m.Version == 4 {
			sql = m.SQL
		}
	}
	if sql == "" {
		t.Fatal("falta la migración 004")
	}
	if !strings.Contains(sql, "uq_flows_bot_key") || !strings.Contains(sql, "archived_at IS NULL") {
		t.Fatal("la key debe liberarse al archivar: índice único parcial uq_flows_bot_key")
	}
	// Se mira solo el DDL: el archivo explica en un comentario por qué ese índice
	// no está, y ese comentario no debe contar como si estuviera.
	var ddl strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			ddl.WriteString(strings.ReplaceAll(line, " ", ""))
		}
	}
	if strings.Contains(ddl.String(), "UNIQUE(flow_id,checksum)") {
		t.Fatal("UNIQUE(flow_id, checksum) impediría restaurar y republicar una versión")
	}
}

func TestMigracionWhatsAppChannelMetadata(t *testing.T) {
	list, err := LoadMigrations()
	if err != nil {
		t.Fatalf("LoadMigrations: %v", err)
	}
	var sql string
	for _, m := range list {
		if m.Version == 5 {
			sql = m.SQL
			break
		}
	}
	if sql == "" {
		t.Fatal("falta la migración 005")
	}
	for _, required := range []string{
		"waba_id",
		"business_id",
		"channel_connected_at",
		"templates_synced_at",
		"idx_bots_waba",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("la migración 005 no contiene %q", required)
		}
	}
}

func TestMigracionSchedulerDurable(t *testing.T) {
	list, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, m := range list {
		if m.Version == 6 {
			sql = m.SQL
		}
	}
	if sql == "" {
		t.Fatal("falta la migración 006")
	}
	for _, required := range []string{
		"flow_version_id", "scheduled_for", "next_attempt_at", "locked_by",
		"provider_message_id", "last_error_class", "uq_flow_runs_run_key",
		"flow_schedule_occurrences", "waba_delivery_state", "last_tick_at",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("la migración 006 no contiene %q", required)
		}
	}
}

func TestMigracionEstadosYCorrelacion(t *testing.T) {
	list, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range list {
		if migration.Version == 7 {
			sql = migration.SQL
		}
	}
	if sql == "" {
		t.Fatal("falta la migración 007")
	}
	for _, required := range []string{
		"provider_status_events", "message_correlations", "delivered_at", "read_at",
		"played_at", "pricing_type", "pricing_category", "uq_flow_runs_provider_message", "applied_at",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("la migración 007 no contiene %q", required)
		}
	}
}

func TestMigracionContabilidadDeConsumo(t *testing.T) {
	list, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var sql string
	for _, migration := range list {
		if migration.Version == 9 {
			sql = migration.SQL
			break
		}
	}
	if sql == "" {
		t.Fatal("falta la migración 009")
	}
	for _, required := range []string{
		"provider_rate_cards", "effective_from", "tier_from",
		"ai_usage_events", "input_tokens", "cache_read_input_tokens",
		"estimated_cost_usd", "uq_ai_usage_provider_request",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("la migración 009 no contiene %q", required)
		}
	}
}
