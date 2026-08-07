package whatsapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Severidades de un evento de cuenta. Se clasifica en conservador: ante la duda,
// Warning. Un falso Critical entrena a ignorar las alertas, que es peor que no
// tenerlas.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// AccountFields son los campos de webhook que describen la salud del canal.
// Estaban suscritos en Meta sin receptor: llegaban y se descartaban.
var AccountFields = map[string]bool{
	"account_update":              true,
	"phone_number_quality_update": true,
	"account_alerts":              true,
	"account_review_update":       true,
	"security":                    true,
	"phone_number_name_update":    true,
	"business_capability_update":  true,
}

// AccountEvent es un evento de salud de cuenta ya normalizado. Los valores de
// Meta (Event, MessagingLimit, Decision…) se conservan **como cadenas crudas**:
// no se traducen a un vocabulario propio porque sus enums todavía no se han
// confirmado contra payloads reales, y equivocarse ahí significaría pausar los
// envíos de un cliente por una restricción que no existe —o no pausarlos por una
// que sí—. Payload guarda el evento entero para poder revisar esa decisión.
type AccountEvent struct {
	WabaID        string
	PhoneNumberID string
	// DisplayPhone es el número tal como Meta lo escribe (`phone_number` o
	// `display_phone_number`). **Los payloads reales no traen `phone_number_id`**
	// —comprobado el 2026-08-07 contra los eventos de prueba del panel—, así que
	// sin esto todos los avisos de un número acabarían en la fila de la cuenta y
	// dos números distintos colisionarían en la misma.
	DisplayPhone   string
	Field          string
	Event          string
	MessagingLimit string
	PreviousLimit  string
	Decision       string
	Severity       string
	OccurredAt     time.Time
	EventKey       string
	Payload        json.RawMessage
}

type accountPayload struct {
	Entry []struct {
		ID      string `json:"id"`
		Time    int64  `json:"time"`
		Changes []struct {
			Field string          `json:"field"`
			Value json.RawMessage `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// ParseAccountEvents extrae los eventos de salud de cuenta. Tolera lo
// desconocido: un elemento ilegible no invalida el lote, y un campo que no
// reconocemos simplemente no sale de aquí (lo advierte el despacho).
func ParseAccountEvents(body []byte) ([]AccountEvent, error) {
	var payload accountPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	var out []AccountEvent
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if !AccountFields[change.Field] {
				continue
			}
			event, ok := parseAccountValue(entry.ID, entry.Time, change.Field, change.Value)
			if !ok {
				continue
			}
			out = append(out, event)
		}
	}
	return out, nil
}

func parseAccountValue(wabaID string, entryTime int64, field string, raw json.RawMessage) (AccountEvent, bool) {
	// Los nombres varían entre campos y Meta añade claves sin avisar, así que se
	// leen todas las variantes conocidas y se conserva el resto en Payload.
	var value struct {
		PhoneNumberID string `json:"phone_number_id"`
		// Meta usa un nombre distinto según el campo, y en los payloads observados
		// nunca manda `phone_number_id`: account_update trae `phone_number` y
		// phone_number_quality_update trae `display_phone_number`.
		PhoneNumber        string `json:"phone_number"`
		DisplayPhoneNumber string `json:"display_phone_number"`
		Event              string `json:"event"`
		CurrentLimit       string `json:"current_limit"`
		OldLimit           string `json:"old_limit"`
		Decision           string `json:"decision"`
		AlertSeverity      string `json:"alert_severity"`
		AlertStatus        string `json:"alert_status"`
		AlertType          string `json:"alert_type"`

		// Su **presencia** es estructural, no un enum que haya que adivinar: si
		// Meta adjunta información de baneo, infracción o restricción, el evento
		// es grave con independencia de los valores que traiga dentro.
		BanInfo         json.RawMessage `json:"ban_info"`
		ViolationInfo   json.RawMessage `json:"violation_info"`
		RestrictionInfo json.RawMessage `json:"restriction_info"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return AccountEvent{}, false
	}

	occurredAt := time.Unix(entryTime, 0).UTC()
	if entryTime == 0 {
		occurredAt = time.Now().UTC()
	}

	event := AccountEvent{
		WabaID:         wabaID,
		PhoneNumberID:  value.PhoneNumberID,
		DisplayPhone:   firstNonEmpty(value.DisplayPhoneNumber, value.PhoneNumber),
		Field:          field,
		Event:          firstNonEmpty(value.Event, value.AlertType),
		MessagingLimit: value.CurrentLimit,
		PreviousLimit:  value.OldLimit,
		Decision:       value.Decision,
		OccurredAt:     occurredAt,
		Severity: accountSeverity(field, value.Event, value.AlertSeverity,
			value.BanInfo, value.ViolationInfo, value.RestrictionInfo),
		Payload: append(json.RawMessage(nil), raw...),
	}

	keyMaterial := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s",
		event.WabaID, event.Field, event.PhoneNumberID, event.DisplayPhone, entryTime, string(raw))
	sum := sha256.Sum256([]byte(keyMaterial))
	event.EventKey = hex.EncodeToString(sum[:])
	return event, true
}

// benignEvents son valores de `event` **observados en payloads reales** (panel de
// Meta → Probar, 2026-08-07) que describen buenas noticias o rutina. No son
// suposiciones: sin esta lista, una cuenta que se verifica o un número que
// termina su onboarding se pintaban en ámbar como si algo fuera mal.
// Cualquier valor no observado sigue cayendo en Warning.
var benignEvents = map[string]bool{
	"VERIFIED_ACCOUNT": true, // account_update: la cuenta quedó verificada
	"ONBOARDING":       true, // phone_number_quality_update: alta del número
}

// accountSeverity clasifica sin inventar enums. Las señales fiables son: la
// severidad que Meta declara en account_alerts, la presencia de estructuras de
// baneo/infracción/restricción, y los pocos valores de `event` confirmados
// contra payloads reales. Todo lo demás queda en Warning, visible sin ser
// alarmista.
func accountSeverity(field, event, declared string, ban, violation, restriction json.RawMessage) string {
	if len(ban) > 0 && !isJSONNull(ban) {
		return SeverityCritical
	}
	if len(violation) > 0 && !isJSONNull(violation) {
		return SeverityCritical
	}
	if len(restriction) > 0 && !isJSONNull(restriction) {
		return SeverityCritical
	}
	switch strings.ToUpper(strings.TrimSpace(declared)) {
	case "CRITICAL":
		return SeverityCritical
	case "WARNING":
		return SeverityWarning
	case "INFO", "INFORMATIONAL":
		return SeverityInfo
	}
	if benignEvents[strings.ToUpper(strings.TrimSpace(event))] {
		return SeverityInfo
	}
	// business_capability_update informa de límites de la cuenta; no describe una
	// degradación. Es el único que se considera rutinario por defecto.
	if field == "business_capability_update" {
		return SeverityInfo
	}
	return SeverityWarning
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
