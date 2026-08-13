package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// CreditsPerUSD es la tasa fija inmutable: 1 USD equivale a 400 créditos.
	CreditsPerUSD = 400.0
	// CreditsPerSol es la tasa de recarga comercial: 5 Soles = 100 créditos (1 Sol = 20 créditos).
	CreditsPerSol = 20.0

	CreditTxRecharge         = "recharge"
	CreditTxPlanAllowance    = "plan_allowance"
	CreditTxAIRuntimeUsage   = "ai_runtime_usage"
	CreditTxAICopilotUsage   = "ai_copilot_usage"
	CreditTxManualAdjustment = "manual_adjustment"
	CreditTxRefund           = "refund"

	CreditRefAIUsageEvents  = "ai_usage_events"
	CreditRefSubscriptions  = "suscripciones_bawto"
	CreditRefPaymentRecords = "payment_records"
	CreditRefManual         = "manual"
)

var (
	ErrInsufficientCredits = errors.New("saldo de créditos insuficiente")
	ErrInvalidCreditAmount = errors.New("el monto de créditos debe ser estrictamente positivo")
)

type CreditWallet struct {
	OrganizationID      string    `json:"organizationId"`
	Balance             float64   `json:"balance"`
	LifetimeCredited    float64   `json:"lifetimeCredited"`
	LifetimeConsumed    float64   `json:"lifetimeConsumed"`
	LowBalanceThreshold float64   `json:"lowBalanceThreshold"`
	AllowOverage        bool      `json:"allowOverage"`
	OverageLimit        float64   `json:"overageLimit"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type CreditTransaction struct {
	ID             string          `json:"id"`
	OrganizationID string          `json:"organizationId"`
	Amount         float64         `json:"amount"`
	BalanceAfter   float64         `json:"balanceAfter"`
	Type           string          `json:"type"`
	ReferenceType  string          `json:"referenceType,omitempty"`
	ReferenceID    string          `json:"referenceId,omitempty"`
	Notes          string          `json:"notes,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
}

type CreditOverview struct {
	Wallet                 CreditWallet        `json:"wallet"`
	EstimatedCallsLeft     int64               `json:"estimatedCallsLeft"`
	AverageCreditsPerCall  float64             `json:"averageCreditsPerCall"`
	IsLowBalance           bool                `json:"isLowBalance"`
	IsOutOfCredits         bool                `json:"isOutOfCredits"`
	RecentTransactions     []CreditTransaction `json:"recentTransactions"`
}

type DeductCreditsInput struct {
	OrgID         string
	Credits       float64
	Type          string
	ReferenceType string
	ReferenceID   string
	Notes         string
	Metadata      map[string]any
	// If AllowExceed is true, will deduct even if balance goes below zero (used for essential turn completion).
	AllowExceed   bool
}

type AddCreditsInput struct {
	OrgID         string
	Credits       float64
	Type          string
	ReferenceType string
	ReferenceID   string
	Notes         string
	Metadata      map[string]any
}

// CostUSDToCredits convierte un costo en USD a créditos exactos usando la tasa de 400 créditos por USD.
func CostUSDToCredits(costUSD float64) float64 {
	if costUSD <= 0 {
		return 0
	}
	return roundCredits(costUSD * CreditsPerUSD)
}

// SolesToCredits convierte un monto en Soles (PEN) a créditos comerciales (5 PEN = 100 créditos -> 1 PEN = 20 cr).
func SolesToCredits(soles float64) float64 {
	if soles <= 0 {
		return 0
	}
	return roundCredits(soles * CreditsPerSol)
}

// CreditsToSoles convierte una cantidad de créditos a su valor equivalente en Soles (PEN).
func CreditsToSoles(credits float64) float64 {
	if credits <= 0 {
		return 0
	}
	return roundCredits(credits / CreditsPerSol)
}

func roundCredits(val float64) float64 {
	return math.Round(val*1_000_000) / 1_000_000
}

// GetOrCreateCreditWallet recupera el monedero de la organización o crea uno con saldo 0 si no existe.
func GetOrCreateCreditWallet(ctx context.Context, pool *pgxpool.Pool, orgID string) (*CreditWallet, error) {
	var w CreditWallet
	err := pool.QueryRow(ctx, `
		INSERT INTO organization_credit_wallets (organization_id)
		VALUES ($1::uuid)
		ON CONFLICT (organization_id) DO UPDATE SET updated_at = organization_credit_wallets.updated_at
		RETURNING organization_id::text, balance::float8, lifetime_credited::float8,
		          lifetime_consumed::float8, low_balance_threshold::float8,
		          allow_overage, overage_limit::float8, created_at, updated_at`,
		orgID).Scan(
		&w.OrganizationID, &w.Balance, &w.LifetimeCredited,
		&w.LifetimeConsumed, &w.LowBalanceThreshold,
		&w.AllowOverage, &w.OverageLimit, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("obtener monedero: %w", err)
	}
	return &w, nil
}

// DeductCredits descuenta créditos de forma atómica y registra la transacción en el ledger.
func DeductCredits(ctx context.Context, pool *pgxpool.Pool, in DeductCreditsInput) (*CreditTransaction, error) {
	if in.Credits <= 0 {
		return nil, ErrInvalidCreditAmount
	}
	credits := roundCredits(in.Credits)
	if in.Type == "" {
		in.Type = CreditTxAIRuntimeUsage
	}

	metaBytes := json.RawMessage(`{}`)
	if len(in.Metadata) > 0 {
		if b, err := json.Marshal(in.Metadata); err == nil {
			metaBytes = b
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Lock pesimista en la fila del monedero
	var w CreditWallet
	err = tx.QueryRow(ctx, `
		INSERT INTO organization_credit_wallets (organization_id)
		VALUES ($1::uuid)
		ON CONFLICT (organization_id) DO UPDATE SET updated_at = NOW()
		RETURNING organization_id::text, balance::float8, lifetime_credited::float8,
		          lifetime_consumed::float8, low_balance_threshold::float8,
		          allow_overage, overage_limit::float8, created_at, updated_at`,
		in.OrgID).Scan(
		&w.OrganizationID, &w.Balance, &w.LifetimeCredited,
		&w.LifetimeConsumed, &w.LowBalanceThreshold,
		&w.AllowOverage, &w.OverageLimit, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("bloquear monedero: %w", err)
	}

	newBalance := roundCredits(w.Balance - credits)
	maxAllowedDebt := 0.0
	if w.AllowOverage {
		maxAllowedDebt = w.OverageLimit
	}
	if !in.AllowExceed && newBalance < -maxAllowedDebt {
		return nil, ErrInsufficientCredits
	}

	newLifetimeConsumed := roundCredits(w.LifetimeConsumed + credits)

	// Actualizar monedero
	_, err = tx.Exec(ctx, `
		UPDATE organization_credit_wallets
		   SET balance = $2, lifetime_consumed = $3, updated_at = NOW()
		 WHERE organization_id = $1::uuid`,
		in.OrgID, newBalance, newLifetimeConsumed)
	if err != nil {
		return nil, fmt.Errorf("actualizar saldo monedero: %w", err)
	}

	// Registrar transacción inmutable
	var txRecord CreditTransaction
	err = tx.QueryRow(ctx, `
		INSERT INTO organization_credit_transactions
		    (organization_id, amount, balance_after, type, reference_type, reference_id, notes, metadata)
		VALUES ($1::uuid, $2, $3, $4, NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), $8::jsonb)
		RETURNING id::text, organization_id::text, amount::float8, balance_after::float8,
		          type, COALESCE(reference_type,''), COALESCE(reference_id,''),
		          COALESCE(notes,''), metadata, created_at`,
		in.OrgID, -credits, newBalance, in.Type, in.ReferenceType, in.ReferenceID, in.Notes, metaBytes,
	).Scan(
		&txRecord.ID, &txRecord.OrganizationID, &txRecord.Amount, &txRecord.BalanceAfter,
		&txRecord.Type, &txRecord.ReferenceType, &txRecord.ReferenceID,
		&txRecord.Notes, &txRecord.Metadata, &txRecord.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("registrar transaccion credito: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &txRecord, nil
}

// AddCredits acredita créditos a la organización y registra la transacción en el ledger.
func AddCredits(ctx context.Context, pool *pgxpool.Pool, in AddCreditsInput) (*CreditTransaction, error) {
	if in.Credits <= 0 {
		return nil, ErrInvalidCreditAmount
	}
	credits := roundCredits(in.Credits)
	if in.Type == "" {
		in.Type = CreditTxRecharge
	}

	metaBytes := json.RawMessage(`{}`)
	if len(in.Metadata) > 0 {
		if b, err := json.Marshal(in.Metadata); err == nil {
			metaBytes = b
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var w CreditWallet
	err = tx.QueryRow(ctx, `
		INSERT INTO organization_credit_wallets (organization_id)
		VALUES ($1::uuid)
		ON CONFLICT (organization_id) DO UPDATE SET updated_at = NOW()
		RETURNING organization_id::text, balance::float8, lifetime_credited::float8,
		          lifetime_consumed::float8, low_balance_threshold::float8,
		          allow_overage, overage_limit::float8, created_at, updated_at`,
		in.OrgID).Scan(
		&w.OrganizationID, &w.Balance, &w.LifetimeCredited,
		&w.LifetimeConsumed, &w.LowBalanceThreshold,
		&w.AllowOverage, &w.OverageLimit, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("bloquear monedero para abono: %w", err)
	}

	newBalance := roundCredits(w.Balance + credits)
	newLifetimeCredited := roundCredits(w.LifetimeCredited + credits)

	_, err = tx.Exec(ctx, `
		UPDATE organization_credit_wallets
		   SET balance = $2, lifetime_credited = $3, updated_at = NOW()
		 WHERE organization_id = $1::uuid`,
		in.OrgID, newBalance, newLifetimeCredited)
	if err != nil {
		return nil, fmt.Errorf("actualizar saldo monedero abono: %w", err)
	}

	var txRecord CreditTransaction
	err = tx.QueryRow(ctx, `
		INSERT INTO organization_credit_transactions
		    (organization_id, amount, balance_after, type, reference_type, reference_id, notes, metadata)
		VALUES ($1::uuid, $2, $3, $4, NULLIF($5,''), NULLIF($6,''), NULLIF($7,''), $8::jsonb)
		RETURNING id::text, organization_id::text, amount::float8, balance_after::float8,
		          type, COALESCE(reference_type,''), COALESCE(reference_id,''),
		          COALESCE(notes,''), metadata, created_at`,
		in.OrgID, credits, newBalance, in.Type, in.ReferenceType, in.ReferenceID, in.Notes, metaBytes,
	).Scan(
		&txRecord.ID, &txRecord.OrganizationID, &txRecord.Amount, &txRecord.BalanceAfter,
		&txRecord.Type, &txRecord.ReferenceType, &txRecord.ReferenceID,
		&txRecord.Notes, &txRecord.Metadata, &txRecord.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("registrar abono de credito: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &txRecord, nil
}

// UpdateWalletSettings actualiza los umbrales de alerta y límites de sobregiro.
func UpdateWalletSettings(ctx context.Context, pool *pgxpool.Pool, orgID string, lowBalanceThreshold float64, allowOverage bool, overageLimit float64) (*CreditWallet, error) {
	if lowBalanceThreshold < 0 || overageLimit < 0 {
		return nil, fmt.Errorf("los umbrales no pueden ser negativos")
	}
	var w CreditWallet
	err := pool.QueryRow(ctx, `
		UPDATE organization_credit_wallets
		   SET low_balance_threshold = $2,
		       allow_overage = $3,
		       overage_limit = $4,
		       updated_at = NOW()
		 WHERE organization_id = $1::uuid
		 RETURNING organization_id::text, balance::float8, lifetime_credited::float8,
		           lifetime_consumed::float8, low_balance_threshold::float8,
		           allow_overage, overage_limit::float8, created_at, updated_at`,
		orgID, lowBalanceThreshold, allowOverage, overageLimit,
	).Scan(
		&w.OrganizationID, &w.Balance, &w.LifetimeCredited,
		&w.LifetimeConsumed, &w.LowBalanceThreshold,
		&w.AllowOverage, &w.OverageLimit, &w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("actualizar ajustes monedero: %w", err)
	}
	return &w, nil
}

// ListCreditTransactions lista los movimientos de créditos paginados por fecha descendente.
func ListCreditTransactions(ctx context.Context, pool *pgxpool.Pool, orgID string, limit int, beforeCreatedAt *time.Time) ([]CreditTransaction, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var rows pgx.Rows
	var err error
	if beforeCreatedAt != nil && !beforeCreatedAt.IsZero() {
		rows, err = pool.Query(ctx, `
			SELECT id::text, organization_id::text, amount::float8, balance_after::float8,
			       type, COALESCE(reference_type,''), COALESCE(reference_id,''),
			       COALESCE(notes,''), metadata, created_at
			  FROM organization_credit_transactions
			 WHERE organization_id = $1::uuid AND created_at < $2
			 ORDER BY created_at DESC LIMIT $3`,
			orgID, beforeCreatedAt, limit)
	} else {
		rows, err = pool.Query(ctx, `
			SELECT id::text, organization_id::text, amount::float8, balance_after::float8,
			       type, COALESCE(reference_type,''), COALESCE(reference_id,''),
			       COALESCE(notes,''), metadata, created_at
			  FROM organization_credit_transactions
			 WHERE organization_id = $1::uuid
			 ORDER BY created_at DESC LIMIT $2`,
			orgID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("listar transacciones de credito: %w", err)
	}
	defer rows.Close()

	out := make([]CreditTransaction, 0)
	for rows.Next() {
		var tx CreditTransaction
		if err := rows.Scan(
			&tx.ID, &tx.OrganizationID, &tx.Amount, &tx.BalanceAfter,
			&tx.Type, &tx.ReferenceType, &tx.ReferenceID,
			&tx.Notes, &tx.Metadata, &tx.CreatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, tx)
	}
	return out, rows.Err()
}

// GetCreditOverview reúne el estado del monedero, cálculos de llamadas restantes e historial reciente.
func GetCreditOverview(ctx context.Context, pool *pgxpool.Pool, orgID string) (*CreditOverview, error) {
	wallet, err := GetOrCreateCreditWallet(ctx, pool, orgID)
	if err != nil {
		return nil, err
	}

	recent, err := ListCreditTransactions(ctx, pool, orgID, 10, nil)
	if err != nil {
		return nil, err
	}

	// Calcular costo promedio por llamada de IA en los últimos 30 días para esta org
	var avgCostUSD float64
	err = pool.QueryRow(ctx, `
		SELECT COALESCE(AVG(estimated_cost_usd), 0)::float8
		  FROM ai_usage_events
		 WHERE organization_id = $1::uuid
		   AND purpose = 'flow_runtime'
		   AND occurred_at >= NOW() - INTERVAL '30 days'`,
		orgID).Scan(&avgCostUSD)
	if err != nil {
		avgCostUSD = 0
	}

	avgCreditsPerCall := CostUSDToCredits(avgCostUSD)
	if avgCreditsPerCall <= 0 {
		// Default conservador: ≈ DeepSeek v4-flash llamada típica (0.1456 créditos)
		avgCreditsPerCall = 0.1456
	}

	var callsLeft int64
	if wallet.Balance > 0 && avgCreditsPerCall > 0 {
		callsLeft = int64(wallet.Balance / avgCreditsPerCall)
	}

	isLow := wallet.Balance <= wallet.LowBalanceThreshold && wallet.Balance > 0
	isOut := wallet.Balance <= 0

	return &CreditOverview{
		Wallet:                *wallet,
		EstimatedCallsLeft:    callsLeft,
		AverageCreditsPerCall: avgCreditsPerCall,
		IsLowBalance:          isLow,
		IsOutOfCredits:        isOut,
		RecentTransactions:    recent,
	}, nil
}

type RechargePlatformCreditsInput struct {
	SellerOrgID     string
	ActivationCode  string
	Credits         float64
	Phone           string
	PaymentRecordID string
	Notes           string
	IdempotencyKey  string
}

// RechargePlatformCredits valida la procedencia comercial y acredita créditos a la organización identificada por su código de activación.
func RechargePlatformCredits(ctx context.Context, pool *pgxpool.Pool, input RechargePlatformCreditsInput) (*CreditWallet, error) {
	input.ActivationCode = strings.ToUpper(strings.TrimSpace(input.ActivationCode))
	input.Phone = NormalizePhone(input.Phone)
	if input.Credits <= 0 {
		return nil, ErrInvalidCreditAmount
	}
	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		return nil, err
	}
	if sellerOrgID != input.SellerOrgID {
		return nil, fmt.Errorf("el bot no pertenece a la organización comercial")
	}
	var targetOrgID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM organizations WHERE upper(activation_code)=$1`, input.ActivationCode).Scan(&targetOrgID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("código de activación inválido")
		}
		return nil, err
	}

	operation := ""
	if input.PaymentRecordID != "" {
		operation, err = paymentOperation(ctx, pool, sellerOrgID, input.PaymentRecordID)
		if err != nil {
			return nil, err
		}
	}

	notes := input.Notes
	if notes == "" {
		if operation != "" {
			notes = fmt.Sprintf("Recarga por comprobante op=%s tel=%s", operation, input.Phone)
		} else {
			notes = fmt.Sprintf("Recarga comercial por bot tel=%s", input.Phone)
		}
	}

	_, err = AddCredits(ctx, pool, AddCreditsInput{
		OrgID:         targetOrgID,
		Credits:       input.Credits,
		Type:          CreditTxRecharge,
		ReferenceType: CreditRefPaymentRecords,
		ReferenceID:   input.PaymentRecordID,
		Notes:         notes,
		Metadata: map[string]any{
			"activation_code":   input.ActivationCode,
			"phone":             input.Phone,
			"payment_record_id": input.PaymentRecordID,
			"operation":         operation,
			"idempotency_key":   input.IdempotencyKey,
		},
	})
	if err != nil {
		return nil, err
	}

	return GetOrCreateCreditWallet(ctx, pool, targetOrgID)
}

// EnsurePlatformCobrosObject asegura que el objeto cobros y sus campos existan en la organización comercial.
func EnsurePlatformCobrosObject(ctx context.Context, pool *pgxpool.Pool, sellerOrgID string) (string, error) {
	var objectID string
	err := pool.QueryRow(ctx, `
		INSERT INTO data_objects(org_id, key, name, plural_name)
		VALUES ($1::uuid, 'cobros', 'Cobro', 'Cobros')
		ON CONFLICT (org_id, key) DO UPDATE SET updated_at = NOW()
		RETURNING id::text`, sellerOrgID).Scan(&objectID)
	if err != nil {
		return "", fmt.Errorf("asegurar objeto cobros: %w", err)
	}

	fields := []struct {
		key, label, typ string
		required        bool
	}{
		{"monto", "Monto", "number", true},
		{"moneda", "Moneda", "text", true},
		{"creditos", "Créditos", "number", true},
		{"operacion", "Operación", "text", false},
		{"estado", "Estado", "text", true},
		{"origen", "Origen", "text", true},
		{"organizacion_id", "Organización ID", "text", true},
		{"codigo_activacion", "Código de activación", "text", true},
		{"organizacion_nombre", "Nombre de organización", "text", false},
		{"telefono", "Teléfono", "text", false},
		{"solicitado_por", "Solicitado por", "text", false},
		{"notas", "Notas", "text", false},
	}
	for _, f := range fields {
		_, _ = pool.Exec(ctx, `
			INSERT INTO data_fields(object_id, key, label, type, required)
			VALUES ($1::uuid, $2, $3, $4, $5)
			ON CONFLICT (object_id, key) DO UPDATE SET label = EXCLUDED.label, type = EXCLUDED.type, required = EXCLUDED.required`,
			objectID, f.key, f.label, f.typ, f.required)
	}
	return objectID, nil
}

type RequestCreditRechargeInput struct {
	OrgID          string
	AmountPen      float64
	Credits        float64
	Operation      string
	PayerPhone     string
	Notes          string
	UserEmail      string
	IdempotencyKey string
}

type PlatformRechargeRequest struct {
	RecordID         string  `json:"recordId"`
	OrganizationID   string  `json:"organizationId"`
	OrganizationName string  `json:"organizationName"`
	ActivationCode   string  `json:"activationCode"`
	AmountPen        float64 `json:"amountPen"`
	Credits          float64 `json:"credits"`
	Operation        string  `json:"operation"`
	Status           string  `json:"status"` // 'pendiente', 'confirmado', 'rechazado'
	Origen           string  `json:"origen"` // 'plataforma'
	Notes            string  `json:"notes"`
	CreatedAt        string  `json:"createdAt"`
}

// RequestPlatformCreditRecharge registra un cobro en estado pendiente en la tabla cobros de Sistemuino sin acreditar saldo aún.
func RequestPlatformCreditRecharge(ctx context.Context, pool *pgxpool.Pool, input RequestCreditRechargeInput) (*PlatformRechargeRequest, error) {
	if input.AmountPen <= 0 && input.Credits <= 0 {
		return nil, fmt.Errorf("monto en soles o créditos inválido")
	}
	if input.Credits <= 0 {
		input.Credits = SolesToCredits(input.AmountPen)
	}
	if input.AmountPen <= 0 {
		input.AmountPen = CreditsToSoles(input.Credits)
	}

	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		return nil, err
	}

	org, err := GetOrganization(ctx, pool, input.OrgID)
	if err != nil || org == nil {
		return nil, fmt.Errorf("organización cliente no encontrada")
	}

	if _, err := EnsurePlatformCobrosObject(ctx, pool, sellerOrgID); err != nil {
		return nil, err
	}

	idempotencyKey := strings.TrimSpace(input.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = fmt.Sprintf("recharge_req:%s:%d", input.OrgID, time.Now().UnixNano())
	}

	res, err := MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID:     sellerOrgID,
		ObjectKey: "cobros",
		Operation: "create",
		Values: map[string]any{
			"organizacion_id":     org.ID,
			"organizacion_nombre": org.Name,
			"codigo_activacion":   org.ActivationCode,
			"monto":               input.AmountPen,
			"moneda":              "PEN",
			"creditos":            input.Credits,
			"operacion":           strings.TrimSpace(input.Operation),
			"telefono":            NormalizePhone(input.PayerPhone),
			"estado":              "pendiente",
			"origen":              "plataforma",
			"notas":               strings.TrimSpace(input.Notes),
			"solicitado_por":      strings.TrimSpace(input.UserEmail),
		},
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return nil, fmt.Errorf("registrar cobro pendiente: %w", err)
	}

	return &PlatformRechargeRequest{
		RecordID:         res.RecordID,
		OrganizationID:   org.ID,
		OrganizationName: org.Name,
		ActivationCode:   org.ActivationCode,
		AmountPen:        input.AmountPen,
		Credits:          input.Credits,
		Operation:        input.Operation,
		Status:           "pendiente",
		Origen:           "plataforma",
		Notes:            input.Notes,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// ApprovePlatformCreditRecharge valida y confirma un cobro pendiente en la tabla cobros de Sistemuino y acredita los créditos.
func ApprovePlatformCreditRecharge(ctx context.Context, pool *pgxpool.Pool, recordID string, approverEmail string) (*CreditWallet, *CreditTransaction, error) {
	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		return nil, nil, err
	}

	var raw json.RawMessage
	err = pool.QueryRow(ctx, `
		SELECT r.data FROM data_records r
		JOIN data_objects o ON o.id = r.object_id
		WHERE r.id = $1::uuid AND o.org_id = $2::uuid AND o.key = 'cobros'`,
		recordID, sellerOrgID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("el cobro no existe en la organización comercial")
	}
	if err != nil {
		return nil, nil, err
	}

	var cobro struct {
		OrgID          string  `json:"organizacion_id"`
		ActivationCode string  `json:"codigo_activacion"`
		Monto          float64 `json:"monto"`
		Creditos       float64 `json:"creditos"`
		Operacion      string  `json:"operacion"`
		Estado         string  `json:"estado"`
		Origen         string  `json:"origen"`
		SolicitadoPor  string  `json:"solicitado_por"`
	}
	if err := json.Unmarshal(raw, &cobro); err != nil {
		return nil, nil, fmt.Errorf("datos del cobro inválidos: %w", err)
	}

	if cobro.Estado != "pendiente" {
		return nil, nil, fmt.Errorf("el cobro ya no está pendiente (estado actual: %s)", cobro.Estado)
	}
	if cobro.Creditos <= 0 {
		cobro.Creditos = SolesToCredits(cobro.Monto)
	}

	now := time.Now().UTC()
	_, err = MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID:          sellerOrgID,
		ObjectKey:      "cobros",
		Operation:      "update",
		RecordID:       recordID,
		IdempotencyKey: fmt.Sprintf("approve:%s:%d", recordID, now.UnixNano()),
		Values: map[string]any{
			"estado":       "confirmado",
			"aprobado_por": approverEmail,
			"aprobado_en":  now.Format(time.RFC3339),
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("confirmar cobro: %w", err)
	}

	notes := fmt.Sprintf("Recarga web aprobada (S/ %.2f) op=%s", cobro.Monto, cobro.Operacion)
	if cobro.SolicitadoPor != "" {
		notes += fmt.Sprintf(" solicitada por %s", cobro.SolicitadoPor)
	}

	txRecord, err := AddCredits(ctx, pool, AddCreditsInput{
		OrgID:         cobro.OrgID,
		Credits:       cobro.Creditos,
		Type:          CreditTxRecharge,
		ReferenceType: CreditRefPaymentRecords,
		ReferenceID:   recordID,
		Notes:         notes,
		Metadata: map[string]any{
			"payment_record_id": recordID,
			"operation":         cobro.Operacion,
			"amount_pen":        cobro.Monto,
			"approver_email":    approverEmail,
			"approved_at":       now.Format(time.RFC3339),
		},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("abonar créditos: %w", err)
	}

	wallet, err := GetOrCreateCreditWallet(ctx, pool, cobro.OrgID)
	return wallet, txRecord, err
}

// RejectPlatformCreditRecharge rechaza un cobro pendiente en la organización comercial.
func RejectPlatformCreditRecharge(ctx context.Context, pool *pgxpool.Pool, recordID string, reason string, rejecterEmail string) error {
	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID:          sellerOrgID,
		ObjectKey:      "cobros",
		Operation:      "update",
		RecordID:       recordID,
		IdempotencyKey: fmt.Sprintf("reject:%s:%d", recordID, now.UnixNano()),
		Values: map[string]any{
			"estado":         "rechazado",
			"motivo_rechazo": strings.TrimSpace(reason),
			"rechazado_por":  rejecterEmail,
			"rechazado_en":   now.Format(time.RFC3339),
		},
	})
	return err
}

// ListPlatformCreditRecharges lista las solicitudes de recarga de créditos registradas en cobros.
func ListPlatformCreditRecharges(ctx context.Context, pool *pgxpool.Pool, status string, orgID string) ([]PlatformRechargeRequest, error) {
	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		return nil, err
	}

	query := `
		SELECT r.id::text, r.data, r.created_at
		  FROM data_records r
		  JOIN data_objects o ON o.id = r.object_id
		 WHERE o.org_id = $1::uuid AND o.key = 'cobros'`
	args := []any{sellerOrgID}

	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND r.data->>'estado' = $%d", len(args))
	}
	if orgID != "" {
		args = append(args, orgID)
		query += fmt.Sprintf(" AND r.data->>'organizacion_id' = $%d", len(args))
	}
	query += " ORDER BY r.created_at DESC LIMIT 100"

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]PlatformRechargeRequest, 0)
	for rows.Next() {
		var id string
		var raw json.RawMessage
		var createdAt time.Time
		if err := rows.Scan(&id, &raw, &createdAt); err != nil {
			return nil, err
		}
		var item struct {
			OrgID          string  `json:"organizacion_id"`
			OrgName        string  `json:"organizacion_nombre"`
			ActivationCode string  `json:"codigo_activacion"`
			Monto          float64 `json:"monto"`
			Creditos       float64 `json:"creditos"`
			Operacion      string  `json:"operacion"`
			Estado         string  `json:"estado"`
			Origen         string  `json:"origen"`
			Notas          string  `json:"notas"`
		}
		_ = json.Unmarshal(raw, &item)
		if item.Creditos <= 0 && item.Monto > 0 {
			item.Creditos = SolesToCredits(item.Monto)
		}
		out = append(out, PlatformRechargeRequest{
			RecordID:         id,
			OrganizationID:   item.OrgID,
			OrganizationName: item.OrgName,
			ActivationCode:   item.ActivationCode,
			AmountPen:        item.Monto,
			Credits:          item.Creditos,
			Operation:        item.Operacion,
			Status:           item.Estado,
			Origen:           item.Origen,
			Notes:            item.Notas,
			CreatedAt:        createdAt.Format(time.RFC3339),
		})
	}
	return out, rows.Err()
}
