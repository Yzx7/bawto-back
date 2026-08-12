package authoring

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
)

type ArtifactRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type KnowledgeArtifact struct {
	Kind          string   `json:"kind"`
	SchemaVersion string   `json:"schemaVersion"`
	ID            string   `json:"id"`
	Version       string   `json:"version"`
	Summary       string   `json:"summary"`
	Rules         []string `json:"rules"`
}

type PlaybookQuestion struct {
	ID       string `json:"id"`
	Prompt   string `json:"prompt"`
	Required bool   `json:"required,omitempty"`
	When     string `json:"when,omitempty"`
}

type CapabilityRequirement struct {
	NodeKinds    []string `json:"nodeKinds,omitempty"`
	RuntimeTools []string `json:"runtimeTools,omitempty"`
}

type ResourceRequirement struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

type TopologyRole struct {
	Role        string `json:"role"`
	Kind        string `json:"kind"`
	ToolRef     string `json:"toolRef,omitempty"`
	Minimum     int    `json:"minimum,omitempty"`
	Description string `json:"description,omitempty"`
}

type TopologyConnection struct {
	FromRole        string `json:"fromRole"`
	ToRole          string `json:"toRole"`
	Handle          string `json:"handle,omitempty"`
	MustPassThrough string `json:"mustPassThrough,omitempty"`
}

type TopologyPattern struct {
	Roles       []TopologyRole       `json:"roles"`
	Connections []TopologyConnection `json:"connections"`
}

type BindingSlot struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

type AgentPromptContract struct {
	Role             string   `json:"role"`
	Responsibility   string   `json:"responsibility"`
	AllowedSources   []string `json:"allowedSources"`
	RequiredBranches []string `json:"requiredBranches"`
	UnknownPolicy    string   `json:"unknownPolicy"`
	ResponsePolicy   string   `json:"responsePolicy"`
}

// Playbook is knowledge for composing ordinary runtime nodes. It is not a
// runtime flow type and no playbook id is stored in the resulting graph.
type Playbook struct {
	SchemaVersion        string                  `json:"schemaVersion"`
	ID                   string                  `json:"id"`
	Version              string                  `json:"version"`
	Status               string                  `json:"status"`
	IntentTags           []string                `json:"intentTags"`
	Risk                 string                  `json:"risk"`
	TriggerTypes         []string                `json:"triggerTypes"`
	RequiredQuestions    []PlaybookQuestion      `json:"requiredQuestions"`
	ConditionalQuestions []PlaybookQuestion      `json:"conditionalQuestions"`
	RequiredCapabilities []CapabilityRequirement `json:"requiredCapabilities"`
	RequiredResources    []ResourceRequirement   `json:"requiredResources"`
	TopologyPattern      TopologyPattern         `json:"topologyPattern"`
	Bindings             []BindingSlot           `json:"bindings"`
	AgentPromptContracts []AgentPromptContract   `json:"agentPromptContracts"`
	Patterns             []ArtifactRef           `json:"patterns"`
	RequiredChecks       []ArtifactRef           `json:"requiredCheckIds"`
	ForbiddenChecks      []ArtifactRef           `json:"forbiddenCheckIds"`
	Policies             []ArtifactRef           `json:"policyIds"`
	Invariants           []string                `json:"invariants"`
}

type PlaybookSummary struct {
	ID                   string                  `json:"id"`
	Version              string                  `json:"version"`
	Status               string                  `json:"status"`
	IntentTags           []string                `json:"intentTags"`
	Risk                 string                  `json:"risk"`
	TriggerTypes         []string                `json:"triggerTypes"`
	RequiredCapabilities []CapabilityRequirement `json:"requiredCapabilities"`
	BundleHash           string                  `json:"knowledgeBundleHash"`
}

type KnowledgeBundle struct {
	Playbook    Playbook            `json:"playbook"`
	Patterns    []KnowledgeArtifact `json:"patterns"`
	Policies    []KnowledgeArtifact `json:"policies"`
	Checks      []KnowledgeArtifact `json:"checks"`
	CatalogHash string              `json:"catalogHash"`
	BundleHash  string              `json:"knowledgeBundleHash"`
}

//go:embed knowledge/playbooks/*.json
var playbookFiles embed.FS

var artifactRegistry = buildArtifactRegistry([]KnowledgeArtifact{
	{Kind: "pattern", SchemaVersion: "1", ID: "wait_resume", Version: "1.0.0", Summary: "Todo retorno conversacional cruza un wait.", Rules: []string{"Los ciclos conversacionales pausan antes de volver a ejecutar.", "Las rutas de error no terminan mudas."}},
	{Kind: "pattern", SchemaVersion: "1", ID: "data_read", Version: "1.0.0", Summary: "Lecturas sobre recursos fijados por el autor.", Rules: []string{"La tabla y campos no son destinos dinámicos.", "found=false no equivale a error técnico."}},
	{Kind: "pattern", SchemaVersion: "1", ID: "data_write_idempotent", Version: "1.0.0", Summary: "Escrituras deterministas e idempotentes en nodos tool.", Rules: []string{"Toda escritura ocurre en el grafo.", "Las operaciones repetibles declaran idempotencyKey."}},
	{Kind: "pattern", SchemaVersion: "1", ID: "visual_extract", Version: "1.0.0", Summary: "Extracción visual con ramas de calidad.", Rules: []string{"No inventar campos ausentes.", "Separar válido, revisión, ilegible y no comprobante."}},
	{Kind: "pattern", SchemaVersion: "1", ID: "orchestrator_specialists", Version: "1.0.0", Summary: "Orquestador sin tools y especialistas acotados.", Rules: []string{"El orquestador solo clasifica y enruta.", "replyOn evita respuestas duplicadas."}},
	{Kind: "pattern", SchemaVersion: "1", ID: "tool_error_recovery", Version: "1.0.0", Summary: "Rutas explícitas para cero resultados y error.", Rules: []string{"Un fallo técnico no prueba ausencia del recurso.", "Cada tool de grafo tiene rutas ok y error."}},
	{Kind: "pattern", SchemaVersion: "1", ID: "handoff_safe", Version: "1.0.0", Summary: "Derivación humana explícita y silenciosa.", Rules: []string{"El bot no promete resolución automática tras derivar.", "La acción handoff conserva el motivo."}},
	{Kind: "policy", SchemaVersion: "1", ID: "graph_writes_only", Version: "1.0.0", Summary: "Los modelos no ejecutan escrituras.", Rules: []string{"Tools con effect=write solo aparecen como nodos tool.", "El Copilot configura, nunca ejecuta."}},
	{Kind: "policy", SchemaVersion: "1", ID: "payment_not_confirmation", Version: "1.0.0", Summary: "Una captura no confirma un pago.", Rules: []string{"El estado inicial es pendiente o revisión.", "La confirmación pertenece al operador o backend autorizado."}},
	{Kind: "policy", SchemaVersion: "1", ID: "resources_must_exist", Version: "1.0.0", Summary: "No inventar recursos de tenant.", Rules: []string{"Cada tabla, campo y conexión se enlaza a un snapshot real.", "Un faltante se reporta como requisito bloqueante."}},
	{Kind: "check", SchemaVersion: "1", ID: "engine_valid", Version: "1.0.0", Summary: "El candidato pasa engine.Validate.", Rules: []string{"La vista tipada se valida al final de cada lote."}},
	{Kind: "check", SchemaVersion: "1", ID: "no_automatic_cycle", Version: "1.0.0", Summary: "No existen ciclos automáticos sin wait.", Rules: []string{"Todo SCC cíclico requiere una pausa efectiva."}},
	{Kind: "check", SchemaVersion: "1", ID: "explicit_error_routes", Version: "1.0.0", Summary: "Las tools tienen recuperación de error.", Rules: []string{"Cada salida error llega a espera, mensaje o handoff."}},
	{Kind: "check", SchemaVersion: "1", ID: "idempotent_write", Version: "1.0.0", Summary: "Escrituras repetibles llevan idempotencia.", Rules: []string{"Pedidos y mutaciones create/upsert declaran una clave estable."}},
	{Kind: "check", SchemaVersion: "1", ID: "agent_write_tool", Version: "1.0.0", Summary: "Un agente recibe una tool de escritura.", Rules: []string{"Este predicado está prohibido."}},
	{Kind: "check", SchemaVersion: "1", ID: "payment_marked_confirmed", Version: "1.0.0", Summary: "La captura marca el pago confirmado.", Rules: []string{"Este predicado está prohibido."}},
})

var bundledPlaybooks = mustLoadPlaybooks()

// ListPlaybooks returns stable summaries and hashes of the complete transitive
// bundle, not just playbook@version.
func ListPlaybooks() []PlaybookSummary {
	keys := make([]string, 0, len(bundledPlaybooks))
	for key := range bundledPlaybooks {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]PlaybookSummary, 0, len(keys))
	for _, key := range keys {
		bundle := buildKnowledgeBundle(bundledPlaybooks[key])
		playbook := bundle.Playbook
		result = append(result, PlaybookSummary{
			ID: playbook.ID, Version: playbook.Version, Status: playbook.Status,
			IntentTags: append([]string(nil), playbook.IntentTags...), Risk: playbook.Risk,
			TriggerTypes:         append([]string(nil), playbook.TriggerTypes...),
			RequiredCapabilities: cloneCapabilities(playbook.RequiredCapabilities), BundleHash: bundle.BundleHash,
		})
	}
	return result
}

// GetPlaybook gets an exact version. An empty version selects the highest
// bundled version for the id.
func GetPlaybook(id, version string) (KnowledgeBundle, bool) {
	if version != "" {
		playbook, exists := bundledPlaybooks[artifactKey(id, version)]
		if !exists {
			return KnowledgeBundle{}, false
		}
		return buildKnowledgeBundle(playbook), true
	}
	versions := make([]string, 0)
	for _, playbook := range bundledPlaybooks {
		if playbook.ID == id {
			versions = append(versions, playbook.Version)
		}
	}
	if len(versions) == 0 {
		return KnowledgeBundle{}, false
	}
	sort.Strings(versions)
	playbook := bundledPlaybooks[artifactKey(id, versions[len(versions)-1])]
	return buildKnowledgeBundle(playbook), true
}

// ValidatePlaybook checks references against the current engine-derived
// catalogs and the closed artifact registry. It is exported for CI fixtures.
func ValidatePlaybook(playbook Playbook) error {
	if playbook.SchemaVersion != "1" || strings.TrimSpace(playbook.ID) == "" || strings.TrimSpace(playbook.Version) == "" {
		return fmt.Errorf("schemaVersion=1, id y version son obligatorios")
	}
	if playbook.Status != "draft" && playbook.Status != "stable" {
		return fmt.Errorf("status %q inválido", playbook.Status)
	}
	if len(playbook.IntentTags) == 0 || len(playbook.TriggerTypes) == 0 {
		return fmt.Errorf("intentTags y triggerTypes no pueden estar vacíos")
	}
	for _, triggerType := range playbook.TriggerTypes {
		if !containsString([]string{"message", "schedule", "event"}, triggerType) {
			return fmt.Errorf("triggerType %q desconocido", triggerType)
		}
	}
	for _, capability := range playbook.RequiredCapabilities {
		for _, kind := range capability.NodeKinds {
			if _, exists := GetNodeKind(kind); !exists {
				return fmt.Errorf("capability referencia node kind %q inexistente", kind)
			}
		}
		for _, toolRef := range capability.RuntimeTools {
			spec, exists := GetRuntimeTool(toolRef)
			if !exists || !spec.ForGraph {
				return fmt.Errorf("capability referencia runtime tool de grafo %q inexistente", toolRef)
			}
		}
	}
	roles := make(map[string]TopologyRole, len(playbook.TopologyPattern.Roles))
	for _, role := range playbook.TopologyPattern.Roles {
		if strings.TrimSpace(role.Role) == "" {
			return fmt.Errorf("topology role sin nombre")
		}
		if _, duplicate := roles[role.Role]; duplicate {
			return fmt.Errorf("topology role duplicado %q", role.Role)
		}
		if _, exists := GetNodeKind(role.Kind); !exists {
			return fmt.Errorf("topology role %q usa kind %q inexistente", role.Role, role.Kind)
		}
		if role.ToolRef != "" {
			spec, exists := GetRuntimeTool(role.ToolRef)
			if role.Kind != "tool" || !exists || !spec.ForGraph {
				return fmt.Errorf("topology role %q usa toolRef %q inválido", role.Role, role.ToolRef)
			}
		}
		roles[role.Role] = role
	}
	for _, connection := range playbook.TopologyPattern.Connections {
		if _, exists := roles[connection.FromRole]; !exists {
			return fmt.Errorf("conexión referencia fromRole %q inexistente", connection.FromRole)
		}
		if _, exists := roles[connection.ToRole]; !exists {
			return fmt.Errorf("conexión referencia toRole %q inexistente", connection.ToRole)
		}
		if connection.MustPassThrough != "" {
			if _, exists := roles[connection.MustPassThrough]; !exists {
				return fmt.Errorf("conexión referencia mustPassThrough %q inexistente", connection.MustPassThrough)
			}
		}
	}
	for expectedKind, references := range map[string][]ArtifactRef{
		"pattern": playbook.Patterns,
		"policy":  playbook.Policies,
		"check":   append(append([]ArtifactRef(nil), playbook.RequiredChecks...), playbook.ForbiddenChecks...),
	} {
		for _, reference := range references {
			artifact, exists := artifactRegistry[artifactKey(reference.ID, reference.Version)]
			if !exists {
				return fmt.Errorf("artefacto %s@%s inexistente", reference.ID, reference.Version)
			}
			if artifact.Kind != expectedKind {
				return fmt.Errorf("artefacto %s@%s es %s, se esperaba %s", reference.ID, reference.Version, artifact.Kind, expectedKind)
			}
		}
	}
	return nil
}

func mustLoadPlaybooks() map[string]Playbook {
	paths, err := fs.Glob(playbookFiles, "knowledge/playbooks/*.json")
	if err != nil {
		panic(err)
	}
	if len(paths) == 0 {
		panic("authoring: no hay playbooks embebidos")
	}
	registry := make(map[string]Playbook, len(paths))
	for _, path := range paths {
		raw, err := playbookFiles.ReadFile(path)
		if err != nil {
			panic(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		var playbook Playbook
		if err := decoder.Decode(&playbook); err != nil {
			panic(fmt.Sprintf("authoring: %s: %v", path, err))
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			panic(fmt.Sprintf("authoring: %s contiene más de un documento", path))
		}
		if err := ValidatePlaybook(playbook); err != nil {
			panic(fmt.Sprintf("authoring: %s: %v", path, err))
		}
		key := artifactKey(playbook.ID, playbook.Version)
		if _, duplicate := registry[key]; duplicate {
			panic(fmt.Sprintf("authoring: playbook duplicado %s", key))
		}
		registry[key] = playbook
	}
	return registry
}

func buildArtifactRegistry(artifacts []KnowledgeArtifact) map[string]KnowledgeArtifact {
	registry := make(map[string]KnowledgeArtifact, len(artifacts))
	for _, artifact := range artifacts {
		key := artifactKey(artifact.ID, artifact.Version)
		if artifact.SchemaVersion != "1" || artifact.ID == "" || artifact.Version == "" {
			panic("authoring: artefacto de conocimiento inválido")
		}
		if _, duplicate := registry[key]; duplicate {
			panic("authoring: artefacto de conocimiento duplicado " + key)
		}
		registry[key] = artifact
	}
	return registry
}

func buildKnowledgeBundle(playbook Playbook) KnowledgeBundle {
	bundle := KnowledgeBundle{Playbook: playbook, CatalogHash: CatalogHash()}
	bundle.Patterns = resolveArtifacts(playbook.Patterns)
	bundle.Policies = resolveArtifacts(playbook.Policies)
	checkRefs := append(append([]ArtifactRef(nil), playbook.RequiredChecks...), playbook.ForbiddenChecks...)
	bundle.Checks = resolveArtifacts(checkRefs)
	hashPayload := struct {
		Playbook    Playbook            `json:"playbook"`
		Patterns    []KnowledgeArtifact `json:"patterns"`
		Policies    []KnowledgeArtifact `json:"policies"`
		Checks      []KnowledgeArtifact `json:"checks"`
		CatalogHash string              `json:"catalogHash"`
	}{bundle.Playbook, bundle.Patterns, bundle.Policies, bundle.Checks, bundle.CatalogHash}
	raw, err := json.Marshal(hashPayload)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(raw)
	bundle.BundleHash = hex.EncodeToString(sum[:])
	return bundle
}

func resolveArtifacts(references []ArtifactRef) []KnowledgeArtifact {
	keys := make([]string, 0, len(references))
	seen := make(map[string]bool, len(references))
	for _, reference := range references {
		key := artifactKey(reference.ID, reference.Version)
		if !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	sort.Strings(keys)
	result := make([]KnowledgeArtifact, 0, len(keys))
	for _, key := range keys {
		result = append(result, artifactRegistry[key])
	}
	return result
}

func artifactKey(id, version string) string {
	return id + "@" + version
}

func cloneCapabilities(source []CapabilityRequirement) []CapabilityRequirement {
	result := make([]CapabilityRequirement, len(source))
	for index, capability := range source {
		result[index] = CapabilityRequirement{
			NodeKinds:    append([]string(nil), capability.NodeKinds...),
			RuntimeTools: append([]string(nil), capability.RuntimeTools...),
		}
	}
	return result
}
