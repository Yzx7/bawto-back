package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TemplateInfo es la definición que Meta devuelve para un WABA. Components se
// conserva completo para que el catálogo no pierda capacidades que el sender
// actual todavía no sabe renderizar.
type TemplateInfo struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Language         string          `json:"language"`
	Status           string          `json:"status"`
	Category         string          `json:"category"`
	ParameterFormat  string          `json:"parameter_format"`
	Components       json.RawMessage `json:"components"`
	RejectedReason   string          `json:"rejected_reason"`
	CorrectCategory  string          `json:"correct_category"`
	PreviousCategory string          `json:"previous_category"`
	QualityScore     struct {
		Score string          `json:"score"`
		Date  json.RawMessage `json:"date"`
	} `json:"quality_score"`
	LastUpdatedTime json.RawMessage `json:"last_updated_time"`
}

// TemplateParameter describe el orden explícito que espera Meta. Por ahora el
// sender soporta parámetros de texto posicionales en BODY.
type TemplateParameter struct {
	Component string `json:"component"`
	Kind      string `json:"kind"`
	Position  int    `json:"position,omitempty"`
	Name      string `json:"name,omitempty"`
}

var (
	positionalParameter = regexp.MustCompile(`\{\{([1-9][0-9]*)\}\}`)
	namedParameter      = regexp.MustCompile(`\{\{([a-zA-Z_][a-zA-Z0-9_]*)\}\}`)
)

// AnalyzeTemplateComponents produce el contrato que se valida contra el nodo
// send. unsupported=true evita publicar un flujo que el sender simplificado
// enviaría de forma incompleta (header dinámico, botón dinámico o parámetros
// con nombre).
func AnalyzeTemplateComponents(raw json.RawMessage, parameterFormat string) ([]TemplateParameter, int, bool) {
	var components []struct {
		Type    string `json:"type"`
		Format  string `json:"format"`
		Text    string `json:"text"`
		Buttons []struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"buttons"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &components) != nil {
		return nil, 0, len(raw) > 0 && string(raw) != "[]"
	}

	format := strings.ToUpper(strings.TrimSpace(parameterFormat))
	var schema []TemplateParameter
	unsupported := format == "NAMED"
	bodyPositions := map[int]bool{}

	addTextParameters := func(component, text string) {
		for _, match := range positionalParameter.FindAllStringSubmatch(text, -1) {
			position, _ := strconv.Atoi(match[1])
			schema = append(schema, TemplateParameter{Component: component, Kind: "text", Position: position})
			if component == "BODY" {
				bodyPositions[position] = true
			} else {
				unsupported = true
			}
		}
		for _, match := range namedParameter.FindAllStringSubmatch(text, -1) {
			schema = append(schema, TemplateParameter{Component: component, Kind: "text", Name: match[1]})
			unsupported = true
		}
	}

	for _, component := range components {
		typ := strings.ToUpper(component.Type)
		addTextParameters(typ, component.Text)
		if typ == "HEADER" {
			headerFormat := strings.ToUpper(component.Format)
			if headerFormat != "" && headerFormat != "TEXT" {
				// Los headers multimedia requieren un parámetro al enviar aun
				// cuando su definición no contiene {{n}}.
				schema = append(schema, TemplateParameter{Component: typ, Kind: strings.ToLower(headerFormat)})
				unsupported = true
			}
		}
		if typ == "BUTTONS" {
			for _, button := range component.Buttons {
				before := len(schema)
				addTextParameters("BUTTON", button.URL)
				if len(schema) > before {
					unsupported = true
				}
			}
		}
	}

	sort.SliceStable(schema, func(i, j int) bool {
		if schema[i].Component != schema[j].Component {
			return schema[i].Component < schema[j].Component
		}
		if schema[i].Position != schema[j].Position {
			return schema[i].Position < schema[j].Position
		}
		return schema[i].Name < schema[j].Name
	})
	bodyCount := 0
	for position := 1; bodyPositions[position]; position++ {
		bodyCount++
	}
	// {{1}}, {{3}} es un contrato inconsistente para el sender posicional.
	if bodyCount != len(bodyPositions) {
		unsupported = true
	}
	return schema, bodyCount, unsupported
}

func templateHTTPClient(cfg SendConfig) *http.Client {
	if cfg.HTTP != nil {
		return cfg.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// ListTemplates recorre todas las páginas del catálogo del WABA. No acepta una
// sola página silenciosamente: con más de 200 plantillas eso produciría falsos
// "no existe" al publicar.
func ListTemplates(ctx context.Context, cfg SendConfig, wabaID string) ([]TemplateInfo, error) {
	if strings.TrimSpace(wabaID) == "" {
		return nil, fmt.Errorf("waba_id vacío")
	}
	fields := "id,name,language,status,category,components,parameter_format,quality_score,rejected_reason,correct_category,previous_category,last_updated_time"
	next := fmt.Sprintf("%s/%s/%s/message_templates?fields=%s&limit=200",
		strings.TrimRight(cfg.APIBase, "/"), cfg.Version, url.QueryEscape(wabaID), url.QueryEscape(fields))
	var all []TemplateInfo
	for page := 0; next != ""; page++ {
		if page >= 100 {
			return nil, fmt.Errorf("Meta devolvió demasiadas páginas de plantillas")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		resp, err := templateHTTPClient(cfg).Do(req)
		if err != nil {
			return nil, err
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 300 {
			return nil, &APIError{StatusCode: resp.StatusCode, Body: string(body),
				RetryAfter: retryAfter(resp.Header.Get("Retry-After"), time.Now())}
		}
		var payload struct {
			Data   []TemplateInfo `json:"data"`
			Paging struct {
				Next string `json:"next"`
			} `json:"paging"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, fmt.Errorf("respuesta de templates inválida: %w", err)
		}
		all = append(all, payload.Data...)
		next = payload.Paging.Next
	}
	return all, nil
}

// TemplateEvent normaliza los tres webhooks operativos del catálogo:
// estado, calidad y recategorización.
type TemplateEvent struct {
	EventKey         string
	WabaID           string
	Field            string
	TemplateID       string
	Name             string
	Language         string
	OccurredAt       time.Time
	Status           string
	Category         string
	PreviousCategory string
	PendingCategory  string
	CategoryChangeAt *time.Time
	QualityScore     string
	PreviousQuality  string
	Reason           string
	RejectionReason  string
	Recommendation   string
	Payload          json.RawMessage
}

// ParseTemplateEvents usa entry.id como WABA ID; estos cambios no incluyen
// metadata.phone_number_id porque las plantillas son activos de la cuenta.
func ParseTemplateEvents(body []byte) ([]TemplateEvent, error) {
	var payload struct {
		Object string `json:"object"`
		Entry  []struct {
			ID      string `json:"id"`
			Time    int64  `json:"time"`
			Changes []struct {
				Field string          `json:"field"`
				Value json.RawMessage `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	var out []TemplateEvent
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != "message_template_status_update" &&
				change.Field != "message_template_quality_update" &&
				change.Field != "template_category_update" {
				continue
			}
			var value struct {
				Event                   string          `json:"event"`
				MessageTemplateID       json.RawMessage `json:"message_template_id"`
				MessageTemplateName     string          `json:"message_template_name"`
				MessageTemplateLanguage string          `json:"message_template_language"`
				MessageTemplateCategory string          `json:"message_template_category"`
				Reason                  string          `json:"reason"`
				PreviousQualityScore    string          `json:"previous_quality_score"`
				NewQualityScore         string          `json:"new_quality_score"`
				PreviousCategory        string          `json:"previous_category"`
				NewCategory             string          `json:"new_category"`
				CorrectCategory         string          `json:"correct_category"`
				CategoryUpdateTimestamp int64           `json:"category_update_timestamp"`
				RejectionInfo           struct {
					Reason         string `json:"reason"`
					Recommendation string `json:"recommendation"`
				} `json:"rejection_info"`
			}
			if json.Unmarshal(change.Value, &value) != nil {
				continue
			}
			templateID := strings.Trim(string(value.MessageTemplateID), `"`)
			occurredAt := time.Unix(entry.Time, 0).UTC()
			if entry.Time == 0 {
				occurredAt = time.Now().UTC()
			}
			event := TemplateEvent{
				WabaID: entry.ID, Field: change.Field, TemplateID: templateID,
				Name: value.MessageTemplateName, Language: value.MessageTemplateLanguage,
				OccurredAt: occurredAt, Status: value.Event,
				PreviousCategory: value.PreviousCategory,
				QualityScore:     value.NewQualityScore, PreviousQuality: value.PreviousQualityScore,
				Reason: value.Reason, RejectionReason: value.RejectionInfo.Reason,
				Recommendation: value.RejectionInfo.Recommendation,
				Payload:        append(json.RawMessage(nil), change.Value...),
			}
			switch change.Field {
			case "message_template_status_update":
				event.Category = value.MessageTemplateCategory
			case "template_category_update":
				if value.CorrectCategory != "" {
					// En el aviso previo, new_category aún es la categoría
					// vigente y correct_category es la que entrará en vigor.
					event.Category = value.NewCategory
					event.PendingCategory = value.CorrectCategory
					if value.CategoryUpdateTimestamp > 0 {
						at := time.Unix(value.CategoryUpdateTimestamp, 0).UTC()
						event.CategoryChangeAt = &at
					}
				} else {
					event.Category = value.NewCategory
				}
			}
			keyMaterial := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s",
				event.WabaID, event.Field, event.TemplateID, event.Language, entry.Time, string(change.Value))
			sum := sha256.Sum256([]byte(keyMaterial))
			event.EventKey = hex.EncodeToString(sum[:])
			out = append(out, event)
		}
	}
	return out, nil
}
