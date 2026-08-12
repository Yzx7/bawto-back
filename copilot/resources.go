package copilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/authoring"
	"github.com/Yzx7/sacs-chatbots/connectors"
	"github.com/Yzx7/sacs-chatbots/models"
)

// ResourceBundle conserva únicamente metadatos aptos para autoría. Snapshot es
// lo que consumen los validadores; Connections mantiene label/lastOKAt para la
// function call de lectura sin volver a consultar una vista con secretos.
type ResourceBundle struct {
	Snapshot    authoring.AuthoringResourceSnapshot `json:"snapshot"`
	Connections []SafeConnectionView                `json:"connections,omitempty"`
	Hash        string                              `json:"hash"`
}

// SafeConnectionView es exactamente la salida permitida de
// list_connections_safe. Agregar un dato nuevo requiere hacerlo explícitamente
// aquí y en su prueba de no filtración.
type SafeConnectionView struct {
	Key          string     `json:"key"`
	Label        string     `json:"label"`
	Driver       string     `json:"driver"`
	Capabilities []string   `json:"capabilities,omitempty"`
	Status       string     `json:"status"`
	LastOKAt     *time.Time `json:"lastOKAt,omitempty"`
}

// LoadResourceBundle arma una fotografía completa y aislada por organización.
// No lee data_records, mensajes, URLs ni credenciales.
func LoadResourceBundle(
	ctx context.Context,
	pool *pgxpool.Pool,
	organizationID, botID string,
) (*ResourceBundle, error) {
	safe, err := models.LoadFlowCopilotResourcesSafe(ctx, pool, organizationID, botID)
	if err != nil {
		return nil, fmt.Errorf("cargar recursos para autoría: %w", err)
	}
	return BuildResourceBundle(safe)
}

// BuildResourceBundle convierte el DTO neutral de models al contrato puro de
// authoring. Apply puede usar la misma función con un snapshot leído dentro de
// su pgx.Tx, sin abrir otra consulta ni duplicar reglas de capabilities.
func BuildResourceBundle(safe *models.FlowCopilotResourceSnapshotSafe) (*ResourceBundle, error) {
	if safe == nil {
		return nil, fmt.Errorf("snapshot de recursos ausente")
	}
	dataObjects := make([]authoring.DataObjectResource, 0, len(safe.DataObjects))
	for _, object := range safe.DataObjects {
		fieldResources := make([]authoring.DataFieldResource, 0, len(object.Fields))
		for _, field := range object.Fields {
			fieldResources = append(fieldResources, authoring.DataFieldResource{Key: field.Key, Type: field.Type, Required: field.Required})
		}
		sort.Slice(fieldResources, func(i, j int) bool { return fieldResources[i].Key < fieldResources[j].Key })
		dataObjects = append(dataObjects, authoring.DataObjectResource{Key: object.Key, Fields: fieldResources})
	}
	sort.Slice(dataObjects, func(i, j int) bool { return dataObjects[i].Key < dataObjects[j].Key })

	connectionResources := make([]authoring.ConnectionResource, 0, len(safe.Connections))
	connectionViews := make([]SafeConnectionView, 0, len(safe.Connections))
	for _, connection := range safe.Connections {
		capabilities, toolRefs := connectionAuthoringCapabilities(connection.Driver)
		connectionViews = append(connectionViews, SafeConnectionView{
			Key: connection.Key, Label: connection.Label, Driver: connection.Driver,
			Capabilities: capabilities, Status: strings.ToLower(connection.Status), LastOKAt: connection.LastOKAt,
		})
		connectionResources = append(connectionResources, authoring.ConnectionResource{
			Key: connection.Key, Driver: connection.Driver,
			Status:       strings.ToLower(connection.Status),
			Capabilities: capabilities, ToolRefs: toolRefs,
		})
	}
	sort.Slice(connectionResources, func(i, j int) bool { return connectionResources[i].Key < connectionResources[j].Key })
	sort.Slice(connectionViews, func(i, j int) bool { return connectionViews[i].Key < connectionViews[j].Key })

	templateResources := make([]authoring.TemplateResource, 0, len(safe.Templates))
	for _, template := range safe.Templates {
		templateResources = append(templateResources, authoring.TemplateResource{
			Name: template.Name, Language: template.Language,
			Status: strings.ToLower(template.Status),
		})
	}
	sort.Slice(templateResources, func(i, j int) bool {
		if templateResources[i].Name == templateResources[j].Name {
			return templateResources[i].Language < templateResources[j].Language
		}
		return templateResources[i].Name < templateResources[j].Name
	})

	variableResources := make([]authoring.VariableResource, 0, len(safe.Variables))
	for _, variable := range safe.Variables {
		variableResources = append(variableResources, authoring.VariableResource{
			Name: variable.Name, Type: variable.Type,
		})
	}
	sort.Slice(variableResources, func(i, j int) bool { return variableResources[i].Name < variableResources[j].Name })

	snapshot := authoring.AuthoringResourceSnapshot{
		DataObjects: dataObjects, Connections: connectionResources,
		Templates: templateResources, Variables: variableResources,
	}
	hash, err := resourceSnapshotHash(snapshot)
	if err != nil {
		return nil, err
	}
	return &ResourceBundle{Snapshot: snapshot, Connections: connectionViews, Hash: hash}, nil
}

func connectionAuthoringCapabilities(driver string) (capabilities, toolRefs []string) {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case connectors.DriverMeudim:
		return []string{"catalog_read", "order_write", "manual_payment"}, []string{
			"catalog_product", "catalog_search", "order_create", "payment_intent_create", "payment_submit",
		}
	default:
		return nil, nil
	}
}

func resourceSnapshotHash(snapshot authoring.AuthoringResourceSnapshot) (string, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("serializar snapshot de recursos: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
