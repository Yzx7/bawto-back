// Package channels define el modelo normalizado (agnóstico de canal) que consume
// el motor. Cada canal (WhatsApp, Telegram, …) traduce hacia/desde estos tipos.
package channels

import "encoding/json"

// EventType distingue un mensaje con contenido de un evento relacionado con
// otro mensaje. WhatsApp transporta ambos dentro de messages[], pero para el
// motor no significan lo mismo: una reacción no es texto ni media y siempre
// apunta a un mensaje anterior.
type EventType string

const (
	EventMessage       EventType = "message"
	EventReaction      EventType = "reaction"
	EventMessageEdit   EventType = "message_edit"
	EventMessageRevoke EventType = "message_revoke"
	EventSystem        EventType = "system"
)

type MessageType string

const (
	MsgText        MessageType = "text"
	MsgReply       MessageType = "reply" // alias legado; los payloads nuevos usan interactive
	MsgImage       MessageType = "image"
	MsgAudio       MessageType = "audio"
	MsgDocument    MessageType = "document"
	MsgVideo       MessageType = "video"
	MsgSticker     MessageType = "sticker"
	MsgLocation    MessageType = "location"
	MsgContacts    MessageType = "contacts"
	MsgInteractive MessageType = "interactive"
	MsgOrder       MessageType = "order"
	MsgReaction    MessageType = "reaction"
	MsgUnsupported MessageType = "unsupported"
	MsgOther       MessageType = "other"
)

// Location es una ubicación compartida por el usuario. Los punteros conservan
// coordenadas cero válidas sin confundirlas con un campo ausente.
type Location struct {
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	Name      string   `json:"name,omitempty"`
	Address   string   `json:"address,omitempty"`
}

// InboundEvent es el sobre normalizado y agnóstico de canal que consume el
// motor. Type describe el contenido principal; EventType describe lo ocurrido.
// Caption permanece separado de Text aunque, por compatibilidad, los adapters
// pueden proyectarlo también como Text para los flujos antiguos.
type InboundEvent struct {
	ChannelID           string      // identificador del canal (phone_number_id en WA)
	WaID                string      // id del mensaje en el canal (idempotencia)
	From                string      // teléfono del usuario final; Meta puede omitirlo
	FromUserID          string      // identidad estable en el canal (BSUID en WA); siempre presente
	Username            string      // nombre de usuario del canal, si lo tiene
	ContactName         string      // nombre del perfil
	EventType           EventType   // message | reaction | message_edit | message_revoke | system
	Type                MessageType // tipo normalizado del contenido
	Text                string      // texto (o título del reply)
	ReplyID             string      // id del botón/lista seleccionado
	QuotedWaID          string      // id del mensaje del negocio citado por el usuario
	MediaID             string      // id de media del proveedor
	MimeType            string      // tipo MIME informado por el canal
	MediaSHA256         string      // hash informado por el proveedor
	MediaURL            string      // URL temporal informada por el proveedor, si existe
	FileName            string      // nombre original de un documento
	Caption             string      // texto adjunto a imagen/video/documento
	Voice               bool        // audio grabado como nota de voz
	Animated            bool        // sticker animado
	Forwarded           bool
	FrequentlyForwarded bool
	Location            *Location
	Contacts            json.RawMessage // payload estructurado del canal
	Order               json.RawMessage // payload estructurado del canal
	ReactionEmoji       string
	ReactionMessageID   string
	ReactionRemoved     bool
	Raw                 json.RawMessage // evento original para unsupported/auditoría
}

// InboundMessage conserva el nombre usado por los controladores existentes.
// Al ser alias, no crea conversiones ni dos contratos paralelos.
type InboundMessage = InboundEvent

// HasMedia indica que el evento referencia un activo descargable.
func (m InboundEvent) HasMedia() bool {
	if m.MediaID == "" {
		return false
	}
	switch m.Type {
	case MsgImage, MsgAudio, MsgDocument, MsgVideo, MsgSticker:
		return true
	default:
		return false
	}
}
