// Migrador versionado del backend.
//
//	go run ./cmd/migrate -status     → qué hay aplicado y qué falta
//	go run ./cmd/migrate             → aplica las migraciones pendientes
//	go run ./cmd/migrate -publish-flow-file waa.json -bot-id UUID -flow-key flow_waa_isp
//	                                 → valida el archivo y publica una versión nueva
//
// Se ejecuta a mano, no en el arranque del servidor: dos instancias del backend
// comparten la misma base (ver DEPLOY.md) y una de ellas es la PC de desarrollo.
// Migrar en el arranque dejaría que un binario a medio hacer aplique DDL a
// producción por el simple hecho de encenderse.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/db"
	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
)

func main() {
	_ = godotenv.Load()

	status := flag.Bool("status", false, "muestra el estado de las migraciones sin aplicar nada")
	publishFlowFile := flag.String("publish-flow-file", "", "archivo JSON que se validará y publicará")
	botID := flag.String("bot-id", "", "bot dueño del flujo que se publicará")
	flowKey := flag.String("flow-key", "", "clave del flujo que se publicará")
	author := flag.String("author", "migrate-cli", "quién queda registrado como autor de la publicación")
	flag.Parse()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fail("DATABASE_URL no está definida")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		fail("no se pudo abrir el pool: %v", err)
	}
	defer pool.Close()
	if err := db.Ping(ctx, pool); err != nil {
		fail("%v", err)
	}

	// Que quede claro contra qué base se está operando antes de tocar nada.
	fmt.Printf("base de datos: %s\n", pool.Config().ConnConfig.Host+"/"+pool.Config().ConnConfig.Database)

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	switch {
	case *status:
		printStatus(ctx, pool)
	case *publishFlowFile != "":
		runPublishFlowFile(ctx, pool, *botID, *flowKey, *publishFlowFile, *author)
	default:
		applied, err := db.Migrate(ctx, pool, log)
		if err != nil {
			fail("%v", err)
		}
		if applied == 0 {
			fmt.Println("sin migraciones pendientes")
			return
		}
		fmt.Printf("OK: %d migración(es) aplicada(s)\n", applied)
	}
}

func runPublishFlowFile(ctx context.Context, pool *pgxpool.Pool, botID, flowKey, path, author string) {
	if botID == "" || flowKey == "" {
		fail("-publish-flow-file requiere -bot-id y -flow-key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fail("leer flujo: %v", err)
	}
	var definition engine.Flow
	if err := json.Unmarshal(raw, &definition); err != nil {
		fail("decodificar flujo: %v", err)
	}
	if err := engine.Validate(&definition); err != nil {
		fail("validar flujo: %v", err)
	}
	canonical, checksum, err := engine.CanonicalChecksum(raw)
	if err != nil {
		fail("normalizar flujo: %v", err)
	}
	flows, err := models.ListFlows(ctx, pool, botID, false)
	if err != nil {
		fail("listar flujos: %v", err)
	}
	var target *models.Flow
	for i := range flows {
		if flows[i].Key == flowKey {
			target = &flows[i]
			break
		}
	}
	if target == nil {
		fail("flujo %q no encontrado en bot %s", flowKey, botID)
	}
	if target.TriggerType != definition.Trigger.Type {
		fail("trigger del archivo %q no coincide con el registrado %q", definition.Trigger.Type, target.TriggerType)
	}
	current, err := models.DraftSnapshotFromFlow(target)
	if err != nil {
		fail("checksum del borrador actual: %v", err)
	}
	snapshot, err := models.UpdateFlowDraft(ctx, pool, botID, target.ID, canonical, current.Checksum, author)
	if err != nil {
		fail("actualizar borrador: %v", err)
	}
	result, err := models.PublishFlow(ctx, pool, botID, target.ID, snapshot.Checksum, author, true)
	if err != nil {
		fail("publicar flujo: %v", err)
	}
	if result == nil {
		fail("el flujo dejó de existir durante la publicación")
	}
	// La versión publicada es lo que ejecuta el webhook: se relee de la base en
	// vez de confiar en lo que se acaba de escribir.
	var live bool
	if err := pool.QueryRow(ctx, `SELECT v.definition = $3::jsonb
		FROM flows f JOIN flow_versions v ON v.id = f.published_version_id
		WHERE f.bot_id = $1::uuid AND f.id = $2::uuid`,
		botID, target.ID, string(canonical)).Scan(&live); err != nil {
		fail("verificar publicación: %v", err)
	}
	if !live {
		fail("la versión publicada no coincide con el archivo")
	}
	fmt.Printf("flujo=%s versión=%d creada=%t checksum=%s\n",
		target.Key, result.Version.Version, result.Created, checksum)
}

func printStatus(ctx context.Context, pool *pgxpool.Pool) {
	list, err := db.Status(ctx, pool)
	if err != nil {
		fail("%v", err)
	}
	drift := false
	for _, m := range list {
		state := "PENDIENTE"
		if m.Applied {
			state = "aplicada"
		}
		if m.Baseline {
			state += " (baseline)"
		}
		if m.Drift {
			state = "CONTENIDO CAMBIADO TRAS APLICARSE"
			drift = true
		}
		fmt.Printf("  %03d %-40s %s\n", m.Version, m.Name, state)
	}
	if drift {
		fail("hay migraciones aplicadas cuyo archivo cambió: una migración aplicada es inmutable")
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERR: "+format+"\n", args...)
	os.Exit(1)
}
