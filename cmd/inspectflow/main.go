// Command inspectflow consulta y, con control de concurrencia explícito,
// reemplaza el borrador de un flujo directamente en PostgreSQL. Nunca publica.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/models"
)

type nodeSummary struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Accepts []string `json:"accepts,omitempty"`
	Outputs []string `json:"outputs,omitempty"`
	SaveAs  string   `json:"saveAs,omitempty"`
	ToolRef string   `json:"toolRef,omitempty"`
}

type summary struct {
	FlowID             string        `json:"flowId"`
	BotID              string        `json:"botId"`
	Key                string        `json:"key"`
	Status             string        `json:"status"`
	DraftChecksum      string        `json:"draftChecksum"`
	PublishedVersionID *string       `json:"publishedVersionId,omitempty"`
	PublishedChecksum  string        `json:"publishedChecksum,omitempty"`
	UnpublishedChanges bool          `json:"unpublishedChanges"`
	UpdatedAt          time.Time     `json:"updatedAt"`
	Nodes              []nodeSummary `json:"nodes"`
	ComparedFile       string        `json:"comparedFile,omitempty"`
	ComparedChecksum   string        `json:"comparedChecksum,omitempty"`
	ComparedEqual      *bool         `json:"comparedEqual,omitempty"`
}

var compareFile string
var exportDraft string

func main() {
	_ = godotenv.Load()
	botID := flag.String("bot-id", "", "bot dueño del flujo")
	flowID := flag.String("flow-id", "", "flujo a consultar")
	writeFile := flag.String("write-file", "", "archivo JSON que reemplaza el borrador; no publica")
	flag.StringVar(&compareFile, "compare-file", "", "archivo local que se compara canónicamente con el borrador")
	flag.StringVar(&exportDraft, "export-draft", "", "ruta donde exportar una copia del borrador consultado")
	expected := flag.String("expected-checksum", "", "checksum actual obligatorio al escribir")
	actor := flag.String("actor", "inspectflow", "marca de auditoría updated_by cuando sea un user id válido")
	flag.Parse()
	if *botID == "" || *flowID == "" {
		fail("-bot-id y -flow-id son obligatorios")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fail("DATABASE_URL no está definida")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fail("PostgreSQL: %v", err)
	}
	defer pool.Close()

	flow, err := models.GetFlow(ctx, pool, *botID, *flowID)
	if err != nil {
		fail("consultar flujo: %v", err)
	}
	if flow == nil {
		fail("flujo no encontrado para ese bot")
	}
	if exportDraft != "" {
		var formatted bytes.Buffer
		if err := json.Indent(&formatted, flow.Draft, "", "  "); err != nil {
			fail("formatear borrador exportado: %v", err)
		}
		formatted.WriteByte('\n')
		if err := os.WriteFile(exportDraft, formatted.Bytes(), 0o600); err != nil {
			fail("exportar borrador: %v", err)
		}
	}
	_, currentChecksum, err := engine.CanonicalChecksum(flow.Draft)
	if err != nil {
		fail("checksum del borrador actual: %v", err)
	}
	if *writeFile != "" {
		if *expected == "" {
			fail("-expected-checksum es obligatorio al escribir")
		}
		if currentChecksum != *expected {
			fail("el borrador cambió: checksum actual %s, esperado %s", currentChecksum, *expected)
		}
		raw, err := os.ReadFile(*writeFile)
		if err != nil {
			fail("leer borrador: %v", err)
		}
		var definition engine.Flow
		if err := json.Unmarshal(raw, &definition); err != nil {
			fail("JSON del borrador: %v", err)
		}
		if err := engine.Validate(&definition); err != nil {
			fail("borrador inválido: %v", err)
		}
		if definition.Trigger.Type != flow.TriggerType {
			fail("trigger %q no coincide con %q", definition.Trigger.Type, flow.TriggerType)
		}
		flow, err = models.UpdateFlowDraft(ctx, pool, *botID, *flowID, raw, *actor)
		if err != nil {
			fail("actualizar borrador: %v", err)
		}
		if flow == nil {
			fail("el flujo dejó de estar disponible antes de escribir")
		}
	}
	printSummary(ctx, pool, flow)
}

func printSummary(ctx context.Context, pool *pgxpool.Pool, flow *models.Flow) {
	var definition engine.Flow
	if err := json.Unmarshal(flow.Draft, &definition); err != nil {
		fail("leer definición guardada: %v", err)
	}
	_, draftChecksum, err := engine.CanonicalChecksum(flow.Draft)
	if err != nil {
		fail("checksum guardado: %v", err)
	}
	result := summary{
		FlowID: flow.ID, BotID: flow.BotID, Key: flow.Key, Status: flow.Status,
		DraftChecksum: draftChecksum, PublishedVersionID: flow.PublishedVersionID,
		UnpublishedChanges: flow.UnpublishedChanges, UpdatedAt: flow.UpdatedAt,
		Nodes: make([]nodeSummary, 0, len(definition.Nodes)),
	}
	if compareFile != "" {
		raw, err := os.ReadFile(compareFile)
		if err != nil {
			fail("leer archivo comparado: %v", err)
		}
		_, comparedChecksum, err := engine.CanonicalChecksum(raw)
		if err != nil {
			fail("checksum del archivo comparado: %v", err)
		}
		equal := comparedChecksum == draftChecksum
		result.ComparedFile = compareFile
		result.ComparedChecksum = comparedChecksum
		result.ComparedEqual = &equal
	}
	if flow.PublishedVersionID != nil {
		version, err := models.GetFlowVersion(ctx, pool, flow.BotID, flow.ID, *flow.PublishedVersionID)
		if err != nil {
			fail("consultar versión publicada: %v", err)
		}
		if version != nil {
			result.PublishedChecksum = version.Checksum
		}
	}
	for _, node := range definition.Nodes {
		result.Nodes = append(result.Nodes, nodeSummary{
			ID: node.ID, Kind: node.Kind, Accepts: node.Accepts,
			Outputs: node.Outputs, SaveAs: node.SaveAs, ToolRef: node.ToolRef,
		})
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fail("imprimir resumen: %v", err)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
