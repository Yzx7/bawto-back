// Command minimaxprobe ejecuta una sonda manual y sin efectos secundarios del
// contrato visual del nodo agent. No escribe en PostgreSQL ni registra cobros:
// sirve para comparar lo que extrae MiniMax-M3 de un conjunto conocido de
// comprobantes antes de habilitar el modelo en un flujo publicado.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/engine"
	"github.com/Yzx7/sacs-chatbots/engine/ai"
)

const extractionInstruction = `Analiza únicamente la imagen actual como posible comprobante de pago peruano.
Selecciona "comprobante" si muestra una operación exitosa y se distinguen datos suficientes; "no_comprobante" si no es un comprobante; o "revision" si la imagen es ambigua, ilegible o no permite confirmar el éxito.
Extrae solo datos visibles. No inventes ni completes datos por intuición.
Usa provider como identificador canónico en minúsculas y elige exactamente uno de estos valores: yape, plin, bcp, interbank u other.
El proveedor es el canal del comprobante, no la red del destinatario: si aparece el logo de Plin y dice "Pago exitoso", usa plin aunque la app sea de Interbank; usa bcp para una transferencia emitida por BCP e interbank para una transferencia bancaria de Interbank que no sea Plin.
Usa amount para el importe transferido, sin sumar comisiones; currency en código ISO 4217; occurredAt en RFC3339 con la zona de Perú (-05:00); operationCode como texto conservando ceros iniciales; recipient para el destinatario visible; y confidence entre 0 y 1.`

var receiptFields = []engine.AgentOutputField{
	{Key: "provider", Type: "string", Description: "Canal canónico del comprobante; exactamente yape, plin, bcp, interbank u other."},
	{Key: "amount", Type: "number", Description: "Importe transferido, sin comisión."},
	{Key: "currency", Type: "string", Description: "Moneda en código ISO 4217, por ejemplo PEN."},
	{Key: "occurredAt", Type: "datetime", Description: "Fecha y hora visibles de la operación, en RFC3339 y zona de Perú (-05:00)."},
	{Key: "operationCode", Type: "string", Description: "Número o código de operación, conservando ceros iniciales."},
	{Key: "recipient", Type: "string", Description: "Nombre del destinatario visible en el comprobante."},
	{Key: "confidence", Type: "number", Description: "Confianza global de la extracción entre 0 y 1."},
}

type result struct {
	File         string         `json:"file"`
	Branch       string         `json:"branch,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
	Model        string         `json:"model,omitempty"`
	RequestID    string         `json:"requestId,omitempty"`
	InputTokens  int64          `json:"inputTokens,omitempty"`
	OutputTokens int64          `json:"outputTokens,omitempty"`
	CostUSD      float64        `json:"estimatedCostUsd,omitempty"`
	DurationMS   int64          `json:"durationMs"`
	ErrorCode    string         `json:"errorCode,omitempty"`
	Error        string         `json:"error,omitempty"`
}

func main() {
	_ = godotenv.Load()

	directory := flag.String("dir", filepath.Join("..", "assets", "recibos"), "directorio de imágenes")
	model := flag.String("model", "MiniMax-M3", "modelo de MiniMax")
	baseURL := flag.String("base-url", "https://api.minimax.io/anthropic", "endpoint Anthropic compatible")
	timeout := flag.Duration("timeout", 90*time.Second, "timeout por imagen")
	flag.Parse()

	apiKey := strings.TrimSpace(os.Getenv("MINIMAX_M3_API_KEY"))
	if apiKey == "" {
		fail("MINIMAX_M3_API_KEY no está definida")
	}
	files, err := imageFiles(*directory)
	if err != nil {
		fail("listar imágenes: %v", err)
	}
	if len(files) == 0 {
		fail("no hay imágenes compatibles en %s", *directory)
	}

	agent := ai.New(apiKey, *baseURL, *model)
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	failed := false
	for _, path := range files {
		probe := run(agent, path, *timeout)
		if probe.Error != "" {
			failed = true
		}
		if err := encoder.Encode(probe); err != nil {
			fail("imprimir resultado: %v", err)
		}
	}
	if failed {
		os.Exit(1)
	}
}

func run(agent *ai.Agent, path string, timeout time.Duration) result {
	started := time.Now()
	probe := result{File: filepath.Base(path)}
	raw, err := os.ReadFile(path)
	if err != nil {
		probe.ErrorCode = "read_error"
		probe.Error = err.Error()
		probe.DurationMS = time.Since(started).Milliseconds()
		return probe
	}
	mimeType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if separator := strings.IndexByte(mimeType, ';'); separator >= 0 {
		mimeType = mimeType[:separator]
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	agentResult, usage, err := agent.RunStructuredWithHistoryUsage(
		ctx,
		extractionInstruction,
		map[string]string{"input_type": "image"},
		[]string{"comprobante", "no_comprobante", "revision"},
		nil,
		true,
		receiptFields,
		&engine.AgentMedia{Data: raw, MIMEType: mimeType},
	)
	probe.DurationMS = time.Since(started).Milliseconds()
	probe.Model = usage.Model
	probe.RequestID = usage.RequestID
	probe.InputTokens = usage.InputTokens
	probe.OutputTokens = usage.OutputTokens
	probe.CostUSD = usage.EstimatedCostUSD()
	if err != nil {
		probe.ErrorCode = ai.OutputErrorCode(err)
		if errors.Is(err, context.DeadlineExceeded) {
			probe.ErrorCode = "timeout"
		}
		probe.Error = err.Error()
		return probe
	}
	probe.Branch = agentResult.Branch
	probe.Data = agentResult.Data
	return probe
}

func imageFiles(directory string) ([]string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
			files = append(files, filepath.Join(directory, entry.Name()))
		}
	}
	sort.Strings(files)
	return files, nil
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}
