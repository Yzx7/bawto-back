package models

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Catálogo de variables que un flujo puede interpolar (`{variable}`) o evaluar
// en una condición. Vive junto a FlowContext a propósito: la regla de nombres
// (`contact_<campo>`, `data_<objeto>_<campo>`) se define una sola vez, y el
// editor la consume en vez de replicarla.

type FlowVariable struct {
	Name      string `json:"name"`
	Group     string `json:"group"`               // mensaje | contacto | datos | registro
	Label     string `json:"label"`               // descripción legible
	Type      string `json:"type,omitempty"`      // text | number | date… (campos definidos)
	Source    string `json:"source,omitempty"`    // nombre visible del objeto
	ObjectKey string `json:"objectKey,omitempty"` // clave técnica del objeto de datos
	FieldKey  string `json:"fieldKey,omitempty"`  // clave técnica del campo
}

// fixedVariables son las que inyecta el runtime en cada ejecución.
var fixedVariables = []FlowVariable{
	{Name: "input", Group: "mensaje", Label: "Texto del mensaje recibido"},
	{Name: "input_type", Group: "mensaje", Label: "Tipo del mensaje (text, image…)"},
	{Name: "input_media_id", Group: "mensaje", Label: "Id del archivo recibido"},
	{Name: "input_wa_id", Group: "mensaje", Label: "Id del mensaje en WhatsApp"},
	{Name: "input.id", Group: "mensaje", Label: "Id del evento recibido"},
	{Name: "input.eventType", Group: "mensaje", Label: "Tipo de evento"},
	{Name: "input.contentType", Group: "mensaje", Label: "Formato del contenido"},
	{Name: "input.text", Group: "mensaje", Label: "Texto principal"},
	{Name: "input.caption", Group: "mensaje", Label: "Texto adjunto a la media"},
	{Name: "input.media.id", Group: "mensaje", Label: "Id del archivo recibido"},
	{Name: "input.media.mimeType", Group: "mensaje", Label: "MIME del archivo recibido"},
	{Name: "input.media.sha256", Group: "mensaje", Label: "Hash SHA-256 informado por el canal"},
	{Name: "input.replyTo", Group: "mensaje", Label: "Id del mensaje citado"},
	{Name: "input.forwarded", Group: "mensaje", Label: "El mensaje fue reenviado"},
	{Name: "input.frequentlyForwarded", Group: "mensaje", Label: "El mensaje fue reenviado muchas veces"},
	{Name: "input.reaction.emoji", Group: "mensaje", Label: "Emoji de la reacción"},
	{Name: "input.reaction.targetMessageId", Group: "mensaje", Label: "Mensaje al que se reaccionó"},
	{Name: "input.reaction.removed", Group: "mensaje", Label: "La reacción fue retirada"},
	{Name: "contact_id", Group: "contacto", Label: "Id interno del contacto"},
	{Name: "contact_name", Group: "contacto", Label: "Nombre del contacto"},
	{Name: "contact_phone", Group: "contacto", Label: "Teléfono normalizado"},
	{Name: "contact_status", Group: "contacto", Label: "Estado del contacto"},
	{Name: "record_id", Group: "registro", Label: "Id del registro que dispara el flujo"},
	{Name: "record_object_id", Group: "registro", Label: "Objeto de ese registro"},
}

// FlowVariables devuelve el catálogo real del bot: las fijas del runtime, los
// campos personalizados del contacto y los campos de cada objeto de datos.
func FlowVariables(ctx context.Context, pool *pgxpool.Pool, botID string) ([]FlowVariable, error) {
	out := append([]FlowVariable(nil), fixedVariables...)

	fields, err := ListContactFields(ctx, pool, botID)
	if err != nil {
		return nil, err
	}
	for _, f := range fields {
		out = append(out, FlowVariable{
			Name:  "contact_" + f.Key,
			Group: "contacto",
			Label: f.Label,
			Type:  f.Type,
		})
	}

	rows, err := pool.Query(ctx, `
		SELECT o.key AS object_key, o.name AS object_name, f.key, f.label, f.type
		  FROM data_fields f
		  JOIN data_objects o ON o.id = f.object_id
		 WHERE o.org_id = (SELECT org_id FROM bots WHERE id = $1::uuid)
		 ORDER BY o.key, f.key`, botID)
	if err != nil {
		return nil, err
	}
	type row struct {
		ObjectKey  string `db:"object_key"`
		ObjectName string `db:"object_name"`
		Key        string `db:"key"`
		Label      string `db:"label"`
		Type       string `db:"type"`
	}
	dataFields, err := pgx.CollectRows(rows, pgx.RowToStructByName[row])
	if err != nil {
		return nil, err
	}
	for _, f := range dataFields {
		out = append(out, FlowVariable{
			Name:      "data_" + f.ObjectKey + "_" + f.Key,
			Group:     "datos",
			Label:     f.Label,
			Type:      f.Type,
			Source:    f.ObjectName,
			ObjectKey: f.ObjectKey,
			FieldKey:  f.Key,
		})
	}
	return out, nil
}
