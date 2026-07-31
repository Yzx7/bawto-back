// Package channels define el modelo normalizado (agnóstico de canal) que consume
// el motor. Cada canal (WhatsApp, Telegram, …) traduce hacia/desde estos tipos.
package channels

type MessageType string

const (
	MsgText  MessageType = "text"
	MsgReply MessageType = "reply" // botón/lista interactiva
	MsgImage MessageType = "image"
	MsgOther MessageType = "other"
)

// InboundMessage es un mensaje entrante normalizado.
type InboundMessage struct {
	ChannelID   string      // identificador del canal (phone_number_id en WA)
	WaID        string      // id del mensaje en el canal (idempotencia)
	From        string      // identificador del usuario final
	ContactName string      // nombre del perfil
	Type        MessageType // text | reply | image | other
	Text        string      // texto (o título del reply)
	ReplyID     string      // id del botón/lista seleccionado
	QuotedWaID  string      // id del mensaje del negocio citado por el usuario
	MediaID     string      // id de media del proveedor (imagen/documento)
	MimeType    string      // tipo MIME informado por el canal
	Caption     string      // texto adjunto a la media
}
