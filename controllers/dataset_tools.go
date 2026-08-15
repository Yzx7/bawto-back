package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/connectors"
	"github.com/Yzx7/sacs-chatbots/connectors/datasetapi"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Ejecutores de dataset_query — la tool agnóstica del §3 de
// PLAN-HACKATON-MARCA-BLANCA-Y-MCP.md.
//
// Comparte la forma exacta de resultado de data_query (`found`, `count`,
// `first`, la lista completa) para que un Router escrito contra una tabla
// local sirva igual contra un dataset externo, sin reescribir el grafo. Y
// comparte el presupuesto por turno de las herramientas de catálogo, no uno
// propio: sale a la red **dentro del turno**, con el advisory lock del chat
// tomado, así que un bucle agéntico mal instruido no debe poder gastar dos
// cuotas de red distintas en el mismo mensaje. Los bloques del grafo, igual
// que en catálogo, quedan fuera del presupuesto: los ejecuta el autor, no un
// modelo que decide cuándo llamar.

// maxDatasetAgentResults limita lo que ve el modelo por defecto. Cada
// resultado se reenvía en todas las iteraciones siguientes del turno, así que
// un dataset entero encarece el bucle completo, no solo el paso que lo pidió.
const maxDatasetAgentResults = 8

// datasetRecord espeja models.DataQueryRecord y catalogRecord a propósito: es
// lo que permite que un mismo Router lea `found`, `count` y
// `first.data.<campo>` sin que le importe si el dato vino de Postgres, de
// Meudim o de este dataset.
type datasetRecord struct {
	RecordID string         `json:"recordId"`
	Data     map[string]any `json:"data"`
}

type datasetResult struct {
	Found   bool            `json:"found"`
	Count   int             `json:"count"`
	First   *datasetRecord  `json:"first"`
	Records []datasetRecord `json:"records"`
}

// datasetConnection resuelve el dataset de la organización del bot.
//
// La clave la fija el bloque; la organización sale del bot. Ni el modelo ni el
// mensaje del cliente participan en decidir a qué dataset se llama.
func (con *Controller) datasetConnection(ctx context.Context, bot *models.BotChannel, key string) (*datasetapi.Client, *models.ExternalConnection, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil, fmt.Errorf("el bloque no declara conexión")
	}
	connection, err := models.ExternalConnectionByKey(ctx, con.Env.Postgres, bot.OrgID, key)
	if err != nil {
		return nil, nil, err
	}
	if connection == nil {
		return nil, nil, fmt.Errorf("la organización no tiene una conexión %q", key)
	}
	if connection.Driver != connectors.DriverDatasetAPI {
		return nil, nil, fmt.Errorf("la conexión %q usa el driver %q, que no consulta datasets", key, connection.Driver)
	}
	if !connection.Active() {
		return nil, nil, fmt.Errorf("la conexión %q está deshabilitada", key)
	}
	if con.Env.Cipher == nil {
		return nil, nil, fmt.Errorf("no hay clave de cifrado configurada (TOKEN_ENC_KEY)")
	}
	credential, err := con.Env.Cipher.Decrypt(connection.CredentialEnc)
	if err != nil {
		return nil, nil, fmt.Errorf("no se pudo descifrar la credencial de %q", key)
	}
	client, err := datasetapi.New(connection.BaseURL, credential, nil)
	if err != nil {
		return nil, nil, err
	}
	return client, connection, nil
}

// recordDatasetCall anota el resultado sin escribir en cada turno, igual que
// recordCatalogCall: un fallo siempre se guarda, un acierto solo cuando venía
// de un fallo, para limpiarlo.
func (con *Controller) recordDatasetCall(ctx context.Context, connection *models.ExternalConnection, callErr error) {
	if connection == nil {
		return
	}
	if callErr == nil && connection.LastError == nil {
		return
	}
	if err := models.RecordExternalConnectionResult(ctx, con.Env.Postgres, connection.ID, callErr); err != nil {
		con.whatsAppLogger().Warn("dataset: no se pudo anotar el resultado",
			"connection", connection.Key, "err", err.Error())
	}
}

// datasetQueryParams es lo que acaba consultándose, ya fusionado lo que fijó
// el autor con lo que pidió el modelo o el grafo.
type datasetQueryParams struct {
	Connection string
	Resource   string
	Text       string
	Where      []datasetapi.Filter
	Sort       string
	Fields     []string
	Limit      int
}

// queryDataset es el camino común del grafo y del agente.
func (con *Controller) queryDataset(ctx context.Context, bot *models.BotChannel, budget *catalogBudget,
	params datasetQueryParams) (datasetResult, error) {
	client, connection, err := con.datasetConnection(ctx, bot, params.Connection)
	if err != nil {
		return datasetResult{}, err
	}
	if err := budget.consume(); err != nil {
		return datasetResult{}, err
	}
	records, meta, err := client.Query(ctx, datasetapi.Query{
		Resource: params.Resource, Text: params.Text, Where: params.Where,
		Sort: params.Sort, Fields: params.Fields, Limit: params.Limit,
	})
	con.recordDatasetCall(ctx, connection, err)
	if err != nil {
		return datasetResult{}, err
	}
	con.whatsAppLogger().Info("dataset consultado",
		"conexion", connection.Key, "recurso", params.Resource, "total", meta.Total)

	result := datasetResult{Count: len(records), Records: make([]datasetRecord, 0, len(records))}
	for _, record := range records {
		result.Records = append(result.Records, datasetRecord{
			RecordID: datasetRecordID(record),
			Data:     projectDatasetFields(record, params.Fields),
		})
	}
	result.Found = len(result.Records) > 0
	if result.Found {
		first := result.Records[0]
		result.First = &first
	}
	return result, nil
}

// datasetRecordID busca una clave `id` reconocible en el registro. Un dataset
// ajeno no tiene por qué traerla; sin ella el registro sigue siendo válido,
// solo que `recordId` queda vacío en vez de inventarse uno.
func datasetRecordID(record datasetapi.Record) string {
	if raw, ok := record["id"]; ok && raw != nil {
		return strings.TrimSpace(fmt.Sprint(raw))
	}
	return ""
}

// projectDatasetFields aplica la misma regla que data_query: un campo
// declarado en `fields` que el registro no trae sale nulo, no ausente. Con
// `fields` vacío se devuelve el registro tal como lo entregó el dataset —el
// autor no acotó nada, así que la proyección es «todo lo que haya».
func projectDatasetFields(record datasetapi.Record, fields []string) map[string]any {
	if len(fields) == 0 {
		data := make(map[string]any, len(record))
		for key, value := range record {
			data[key] = value
		}
		return data
	}
	data := make(map[string]any, len(fields))
	for _, field := range fields {
		if value, ok := record[field]; ok {
			data[field] = value
		} else {
			data[field] = nil
		}
	}
	return data
}

// ---- Bloque del grafo ----

// execDatasetQuery ejecuta la lectura desde un bloque. Los argumentos llegan
// ya interpolados; el validador garantizó que solo `query` y cada
// `where.<n>.value` podían traer variables. Sin presupuesto: lo consume el
// modelo, no el grafo.
func (con *Controller) execDatasetQuery(ctx context.Context, bot *models.BotChannel, args map[string]string) (string, error) {
	limit := 0
	if raw := strings.TrimSpace(args["limit"]); raw != "" {
		var err error
		if limit, err = strconv.Atoi(raw); err != nil {
			return "", fmt.Errorf("limit debe ser un número")
		}
	}
	result, err := con.queryDataset(ctx, bot, nil, datasetQueryParams{
		Connection: args["connection"], Resource: strings.TrimSpace(args["resource"]),
		Text: args["query"], Where: datasetFiltersFromArgs(args),
		Sort: strings.TrimSpace(args["sort"]), Fields: splitList(args["fields"]),
		Limit: limit,
	})
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}

// datasetFiltersFromArgs recompone las condiciones numeradas del bloque, igual
// que dataQueryRulesFromArgs: por índice y no iterando el mapa, porque el
// orden de un `map` en Go es aleatorio.
func datasetFiltersFromArgs(args map[string]string) []datasetapi.Filter {
	var filters []datasetapi.Filter
	for index := 1; index <= maxDataQueryRules; index++ {
		prefix := "where." + strconv.Itoa(index) + "."
		field := strings.TrimSpace(args[prefix+"field"])
		if field == "" {
			continue
		}
		filters = append(filters, datasetapi.Filter{
			Field: field, Op: defaultString(args[prefix+"op"], "eq"), Value: args[prefix+"value"],
		})
	}
	return filters
}

// ---- Herramienta del agente ----

// execAgentDatasetQuery es la misma lectura que usa el grafo, con la
// diferencia de confianza de data_query: aquí los argumentos los redacta el
// modelo, así que `filterFields` se impone antes de consultar. Un campo que el
// autor no listó no se rechaza con un error técnico: se le dice al modelo por
// qué no vale, que es lo que le permite reintentar bien.
func (con *Controller) execAgentDatasetQuery(ctx context.Context, bot *models.BotChannel, budget *catalogBudget,
	config map[string]string, input json.RawMessage) (string, error) {
	resource := strings.TrimSpace(config["resource"])
	if resource == "" {
		return "", fmt.Errorf("la herramienta no tiene recurso configurado")
	}
	var args struct {
		Query   string `json:"query"`
		Sort    string `json:"sort"`
		Filters []struct {
			Field string `json:"field"`
			Op    string `json:"op"`
			Value string `json:"value"`
		} `json:"filters"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("argumentos inválidos")
	}

	allowed := splitList(config["filterFields"])
	filters := make([]datasetapi.Filter, 0, len(args.Filters))
	for _, filter := range args.Filters {
		field := strings.TrimSpace(filter.Field)
		if !containsString(allowed, field) {
			if len(allowed) == 0 {
				return "Esta herramienta no admite filtros por campo. Vuelve a llamarla usando solo `query`.", nil
			}
			return fmt.Sprintf("No puedes filtrar por %q. Campos permitidos: %s.",
				field, strings.Join(allowed, ", ")), nil
		}
		op := strings.TrimSpace(filter.Op)
		if op != "eq" && op != "contains" && op != "in" {
			return fmt.Sprintf("El operador %q no está permitido. Usa eq, contains o in.", op), nil
		}
		filters = append(filters, datasetapi.Filter{Field: field, Op: op, Value: filter.Value})
	}

	limit := maxDatasetAgentResults
	if raw := strings.TrimSpace(config["limit"]); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	result, err := con.queryDataset(ctx, bot, budget, datasetQueryParams{
		Connection: config["connection"], Resource: resource, Text: args.Query,
		Where: filters, Sort: strings.TrimSpace(args.Sort), Fields: splitList(config["fields"]),
		Limit: limit,
	})
	if err != nil {
		return datasetFailureForModel(err), nil
	}
	if !result.Found {
		// Se le dice explícitamente que no hay nada, igual que en data_query y
		// catalog_search: un resultado en blanco invita a rellenar el hueco
		// inventando.
		return "Sin resultados en el dataset. No hay información registrada sobre eso.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d resultado(s) en el dataset:\n", result.Count)
	for index, record := range result.Records {
		fmt.Fprintf(&b, "\n%d. %s", index+1, renderValues(record.Data))
	}
	return b.String(), nil
}

// datasetFailureForModel convierte el fallo en una instrucción que el modelo
// puede seguir, igual que catalogFailureForModel: un error revienta el nodo y
// el cliente recibe el mensaje genérico de siempre, así que en cambio se le
// entrega texto y el bot puede decir que no puede consultar. Lo que nunca debe
// pasar es que el modelo interprete el fallo como «no hay datos».
func datasetFailureForModel(err error) string {
	var apiErr *datasetapi.Error
	switch {
	case errors.Is(err, errCatalogBudget):
		return "Ya consultaste este dataset demasiadas veces en este mensaje. Responde con lo que obtuviste; no vuelvas a buscar."
	case errors.As(err, &apiErr) && apiErr.RateLimited():
		return "El dataset está saturado ahora mismo. NO afirmes que no hay información: dile al cliente que no puedes consultar en este momento."
	default:
		return "No pude consultar el dataset externo. NO afirmes que no hay información ni inventes datos: dile al cliente que hay un problema para consultar."
	}
}
