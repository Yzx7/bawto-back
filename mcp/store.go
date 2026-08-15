package mcp

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Yzx7/sacs-chatbots/authoring"
	"github.com/Yzx7/sacs-chatbots/copilot"
	"github.com/Yzx7/sacs-chatbots/models"
)

// flowStore es la superficie exacta que este servidor necesita de PostgreSQL.
// Existe para que las pruebas del protocolo y de las cuatro capacidades corran
// sin base: las de integración de `models` tardan minutos y reclaman runs
// ajenos (CLAUDE.md §5), así que una prueba del MCP que las exija no se
// ejecutaría nunca. No hay ninguna operación de publicación aquí, y esa
// ausencia es el límite duro del binario.
type flowStore interface {
	// ResolveKey traduce la cadena de la cabecera a su fila de `mcp_keys`, o nil
	// si no existe, está revocada o caducó. Está en la misma interfaz que el
	// resto a propósito: es una lectura más y debe poder sustituirse en las
	// pruebas junto con las demás.
	ResolveKey(ctx context.Context, presented string) (*models.MCPKey, error)
	// TouchKey anota el uso. Va aparte de ResolveKey porque es una escritura y
	// porque no debe condicionar la autorización: si falla, el acceso ya estaba
	// concedido.
	TouchKey(ctx context.Context, keyID string) error
	ListBots(ctx context.Context, orgID string) ([]models.Bot, error)
	GetBot(ctx context.Context, botID string) (*models.Bot, error)
	ListFlows(ctx context.Context, botID string) ([]models.Flow, error)
	GetFlow(ctx context.Context, botID, flowID string) (*models.Flow, error)
	UpdateDraft(ctx context.Context, botID, flowID string, draft json.RawMessage, expectedChecksum, userID string) (*models.DraftSnapshot, error)
	Resources(ctx context.Context, orgID, botID string) (authoring.AuthoringResourceSnapshot, error)
}

type pgStore struct{ pool *pgxpool.Pool }

func (s pgStore) ResolveKey(ctx context.Context, presented string) (*models.MCPKey, error) {
	return models.ResolveMCPKey(ctx, s.pool, presented)
}

func (s pgStore) TouchKey(ctx context.Context, keyID string) error {
	return models.TouchMCPKey(ctx, s.pool, keyID)
}

func (s pgStore) ListBots(ctx context.Context, orgID string) ([]models.Bot, error) {
	return models.ListBotsByOrg(ctx, s.pool, orgID)
}

func (s pgStore) GetBot(ctx context.Context, botID string) (*models.Bot, error) {
	return models.GetBot(ctx, s.pool, botID)
}

func (s pgStore) ListFlows(ctx context.Context, botID string) ([]models.Flow, error) {
	return models.ListFlows(ctx, s.pool, botID, false)
}

func (s pgStore) GetFlow(ctx context.Context, botID, flowID string) (*models.Flow, error) {
	return models.GetFlow(ctx, s.pool, botID, flowID)
}

func (s pgStore) UpdateDraft(
	ctx context.Context,
	botID, flowID string,
	draft json.RawMessage,
	expectedChecksum, userID string,
) (*models.DraftSnapshot, error) {
	return models.UpdateFlowDraft(ctx, s.pool, botID, flowID, draft, expectedChecksum, userID)
}

// Resources reutiliza el mismo bundle que arma el Copilot: es una proyección
// allowlist de esquemas —tablas, campos, plantillas, conexiones— y nunca filas,
// credenciales ni URLs. Sin él, validar contra la organización obligaría a
// escribir una segunda consulta que podría divergir de aquella allowlist.
func (s pgStore) Resources(ctx context.Context, orgID, botID string) (authoring.AuthoringResourceSnapshot, error) {
	bundle, err := copilot.LoadResourceBundle(ctx, s.pool, orgID, botID)
	if err != nil {
		return authoring.AuthoringResourceSnapshot{}, err
	}
	return bundle.Snapshot, nil
}
