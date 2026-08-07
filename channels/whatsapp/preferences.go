package whatsapp

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PreferenceField es el campo de webhook que transporta la voluntad del usuario
// sobre los mensajes de marketing.
const PreferenceField = "user_preferences"

// Valores de `value` observados en la documentación de Meta. Se comparan en
// minúsculas y **no** se traducen a un vocabulario propio: se guardan crudos,
// igual que en la salud de cuenta. Aquí solo se usan para decidir si hay que
// dejar de enviar.
const (
	PreferenceStop   = "stop"
	PreferenceResume = "resume"
)

// PreferenceCategoryMarketing es la única categoría que Meta define hoy. Se
// compara pero no se asume: una categoría desconocida se guarda igual, y el
// bloqueo de envío solo mira esta.
const PreferenceCategoryMarketing = "marketing_messages"

// UserPreference es un cambio de preferencia ya normalizado. Category y Value
// conservan el texto de Meta.
type UserPreference struct {
	ChannelID  string // phone_number_id del negocio que recibió el evento
	WaID       string // teléfono del usuario
	Category   string
	Value      string
	Detail     string
	OccurredAt time.Time
	EventKey   string
	Payload    json.RawMessage
}

// OptedOut indica si esta preferencia significa "no me mandes marketing".
func (p UserPreference) OptedOut() bool {
	return strings.EqualFold(strings.TrimSpace(p.Value), PreferenceStop)
}

// IsMarketing indica si la preferencia habla de mensajes de marketing.
func (p UserPreference) IsMarketing() bool {
	return strings.EqualFold(strings.TrimSpace(p.Category), PreferenceCategoryMarketing)
}

type preferencePayload struct {
	Entry []struct {
		Time    int64 `json:"time"`
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				UserPreferences []struct {
					WaID      string          `json:"wa_id"`
					Category  string          `json:"category"`
					Value     string          `json:"value"`
					Detail    string          `json:"detail"`
					Timestamp json.RawMessage `json:"timestamp"`
				} `json:"user_preferences"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// ParseUserPreferences extrae los cambios de preferencia. Como con la salud de
// cuenta, se conserva el payload entero: la forma exacta de este evento no se ha
// confirmado todavía contra uno real, y sin el original una interpretación
// equivocada sería indiagnosticable.
func ParseUserPreferences(body []byte) ([]UserPreference, error) {
	var payload preferencePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	var out []UserPreference
	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			if change.Field != PreferenceField {
				continue
			}
			for _, pref := range change.Value.UserPreferences {
				if strings.TrimSpace(pref.WaID) == "" || strings.TrimSpace(pref.Value) == "" {
					continue
				}
				occurredAt := preferenceTime(pref.Timestamp, entry.Time)
				item := UserPreference{
					ChannelID:  change.Value.Metadata.PhoneNumberID,
					WaID:       pref.WaID,
					Category:   pref.Category,
					Value:      pref.Value,
					Detail:     pref.Detail,
					OccurredAt: occurredAt,
				}
				raw, _ := json.Marshal(pref)
				item.Payload = raw
				key := fmt.Sprintf("%s\x00%s\x00%s\x00%d",
					item.ChannelID, item.WaID, item.Category, occurredAt.Unix())
				sum := sha256.Sum256([]byte(key))
				item.EventKey = hex.EncodeToString(sum[:])
				out = append(out, item)
			}
		}
	}
	return out, nil
}

// preferenceTime acepta el timestamp como número o como cadena: Meta manda unos
// campos de una forma y otros de otra, y equivocarse aquí desordenaría la guarda
// que impide que un `resume` viejo pise un `stop` nuevo.
func preferenceTime(raw json.RawMessage, entryTime int64) time.Time {
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text != "" && text != "null" {
		var seconds int64
		if _, err := fmt.Sscanf(text, "%d", &seconds); err == nil && seconds > 0 {
			return time.Unix(seconds, 0).UTC()
		}
		if parsed, err := time.Parse(time.RFC3339, text); err == nil {
			return parsed.UTC()
		}
	}
	if entryTime > 0 {
		return time.Unix(entryTime, 0).UTC()
	}
	return time.Now().UTC()
}
