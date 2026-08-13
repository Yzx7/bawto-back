package models

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// CreditsPerUSD es la tasa fija inmutable: 1 USD equivale a 400 créditos.
	CreditsPerUSD = 400.0
	// CreditsPerSol es la tasa de recarga comercial: 5 Soles = 100 créditos (1 Sol = 20 créditos).
	CreditsPerSol = 20.0
	// Los comprobantes fuera de esta ventana no se acreditan automáticamente:
	// quedan pendientes para que una persona determine si aún corresponden.
	automaticPaymentMaxAge     = 72 * time.Hour
	automaticPaymentFutureSkew = 10 * time.Minute

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
	ErrCreditIdempotency   = errors.New("la clave de idempotencia ya identifica otra operación")
)

type CreditWallet struct {
	OrganizationID      string    `json:"organizationId"`
	Balance             float64   `json:"balance"`
	LifetimeCredited    float64   `json:"lifetimeCredited"`
	LifetimeConsumed    float64   `json:"lifetimeConsumed"`
	LowBalanceThreshold float64   `json:"lowBalanceThreshold"`
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
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	CreatedAt      time.Time       `json:"createdAt"`
}

type CreditOverview struct {
	Wallet                CreditWallet        `json:"wallet"`
	EstimatedCallsLeft    int64               `json:"estimatedCallsLeft"`
	AverageCreditsPerCall float64             `json:"averageCreditsPerCall"`
	IsLowBalance          bool                `json:"isLowBalance"`
	IsOutOfCredits        bool                `json:"isOutOfCredits"`
	RecentTransactions    []CreditTransaction `json:"recentTransactions"`
}

type DeductCreditsInput struct {
	OrgID          string
	Credits        float64
	Type           string
	ReferenceType  string
	ReferenceID    string
	Notes          string
	Metadata       map[string]any
	IdempotencyKey string
	// ClampToBalance cobra como máximo el saldo disponible. El costo real sigue
	// en ai_usage_events; el ledger nunca convierte el prepago en deuda.
	ClampToBalance bool
}

type AddCreditsInput struct {
	OrgID          string
	Credits        float64
	Type           string
	ReferenceType  string
	ReferenceID    string
	Notes          string
	Metadata       map[string]any
	IdempotencyKey string
}

// CostUSDToCredits convierte un costo en USD a créditos exactos usando la tasa de 400 créditos por USD.
func CostUSDToCredits(costUSD float64) float64 {
	if !finitePositive(costUSD) {
		return 0
	}
	return roundCredits(costUSD * CreditsPerUSD)
}

// SolesToCredits convierte un monto en Soles (PEN) a créditos comerciales (5 PEN = 100 créditos -> 1 PEN = 20 cr).
func SolesToCredits(soles float64) float64 {
	if !finitePositive(soles) {
		return 0
	}
	return roundCredits(soles * CreditsPerSol)
}

// CreditsToSoles convierte una cantidad de créditos a su valor equivalente en Soles (PEN).
func CreditsToSoles(credits float64) float64 {
	if !finitePositive(credits) {
		return 0
	}
	return roundCredits(credits / CreditsPerSol)
}

func roundCredits(val float64) float64 {
	return math.Round(val*1_000_000) / 1_000_000
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func normalizedCreditMutation(credits float64, idempotencyKey string) (float64, string, error) {
	if !finitePositive(credits) {
		return 0, "", ErrInvalidCreditAmount
	}
	credits = roundCredits(credits)
	if credits <= 0 {
		return 0, "", ErrInvalidCreditAmount
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 220 {
		return 0, "", fmt.Errorf("la clave de idempotencia es obligatoria y admite hasta 220 caracteres")
	}
	return credits, idempotencyKey, nil
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
		          created_at, updated_at`,
		orgID).Scan(
		&w.OrganizationID, &w.Balance, &w.LifetimeCredited,
		&w.LifetimeConsumed, &w.LowBalanceThreshold,
		&w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("obtener monedero: %w", err)
	}
	return &w, nil
}

// DeductCredits descuenta créditos de forma atómica y registra la transacción en el ledger.
func DeductCredits(ctx context.Context, pool *pgxpool.Pool, in DeductCreditsInput) (*CreditTransaction, error) {
	credits, idempotencyKey, err := normalizedCreditMutation(in.Credits, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	in.Credits = credits
	in.IdempotencyKey = idempotencyKey
	if in.Type == "" {
		in.Type = CreditTxAIRuntimeUsage
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	txRecord, err := deductCreditsTx(ctx, tx, in)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return txRecord, nil
}

// AddCredits acredita créditos a la organización y registra la transacción en el ledger.
func AddCredits(ctx context.Context, pool *pgxpool.Pool, in AddCreditsInput) (*CreditTransaction, error) {
	credits, idempotencyKey, err := normalizedCreditMutation(in.Credits, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	in.Credits = credits
	in.IdempotencyKey = idempotencyKey
	if in.Type == "" {
		in.Type = CreditTxRecharge
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	txRecord, err := addCreditsTx(ctx, tx, in)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return txRecord, nil
}

func deductCreditsTx(ctx context.Context, tx pgx.Tx, in DeductCreditsInput) (*CreditTransaction, error) {
	credits, idempotencyKey, err := normalizedCreditMutation(in.Credits, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	in.Credits = credits
	in.IdempotencyKey = idempotencyKey
	if in.Type == "" {
		in.Type = CreditTxAIRuntimeUsage
	}
	if err := lockCreditIdempotency(ctx, tx, idempotencyKey); err != nil {
		return nil, err
	}
	if existing, err := creditTransactionByKey(ctx, tx, idempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.OrganizationID != in.OrgID || existing.Type != in.Type || roundCredits(-existing.Amount) != credits {
			return nil, ErrCreditIdempotency
		}
		return existing, nil
	}

	wallet, err := lockCreditWallet(ctx, tx, in.OrgID)
	if err != nil {
		return nil, err
	}
	charged := credits
	if wallet.Balance < charged {
		if !in.ClampToBalance || wallet.Balance <= 0 {
			return nil, ErrInsufficientCredits
		}
		charged = roundCredits(wallet.Balance)
	}
	newBalance := roundCredits(wallet.Balance - charged)
	newLifetimeConsumed := roundCredits(wallet.LifetimeConsumed + charged)
	if _, err := tx.Exec(ctx, `UPDATE organization_credit_wallets
		SET balance=$2, lifetime_consumed=$3, updated_at=NOW()
		WHERE organization_id=$1::uuid`, in.OrgID, newBalance, newLifetimeConsumed); err != nil {
		return nil, fmt.Errorf("actualizar saldo monedero: %w", err)
	}
	metadata := cloneCreditMetadata(in.Metadata)
	if charged != credits {
		metadata["requested_credits"] = credits
		metadata["charged_credits"] = charged
		metadata["partial_charge"] = true
	}
	return insertCreditTransaction(ctx, tx, in.OrgID, -charged, newBalance, in.Type,
		in.ReferenceType, in.ReferenceID, in.Notes, metadata, idempotencyKey)
}

func addCreditsTx(ctx context.Context, tx pgx.Tx, in AddCreditsInput) (*CreditTransaction, error) {
	credits, idempotencyKey, err := normalizedCreditMutation(in.Credits, in.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	in.Credits = credits
	in.IdempotencyKey = idempotencyKey
	if in.Type == "" {
		in.Type = CreditTxRecharge
	}
	if err := lockCreditIdempotency(ctx, tx, idempotencyKey); err != nil {
		return nil, err
	}
	if existing, err := creditTransactionByKey(ctx, tx, idempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.OrganizationID != in.OrgID || existing.Type != in.Type || roundCredits(existing.Amount) != credits {
			return nil, ErrCreditIdempotency
		}
		return existing, nil
	}

	wallet, err := lockCreditWallet(ctx, tx, in.OrgID)
	if err != nil {
		return nil, err
	}
	newBalance := roundCredits(wallet.Balance + credits)
	newLifetimeCredited := roundCredits(wallet.LifetimeCredited + credits)
	if _, err := tx.Exec(ctx, `UPDATE organization_credit_wallets
		SET balance=$2, lifetime_credited=$3, updated_at=NOW()
		WHERE organization_id=$1::uuid`, in.OrgID, newBalance, newLifetimeCredited); err != nil {
		return nil, fmt.Errorf("actualizar saldo monedero abono: %w", err)
	}
	return insertCreditTransaction(ctx, tx, in.OrgID, credits, newBalance, in.Type,
		in.ReferenceType, in.ReferenceID, in.Notes, in.Metadata, idempotencyKey)
}

func lockCreditIdempotency(ctx context.Context, tx pgx.Tx, idempotencyKey string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "credit:"+idempotencyKey)
	return err
}

func creditTransactionByKey(ctx context.Context, tx pgx.Tx, idempotencyKey string) (*CreditTransaction, error) {
	var item CreditTransaction
	err := tx.QueryRow(ctx, `SELECT id::text, organization_id::text, amount::float8,
		balance_after::float8, type, COALESCE(reference_type,''), COALESCE(reference_id,''),
		COALESCE(notes,''), metadata, COALESCE(idempotency_key,''), created_at
		FROM organization_credit_transactions WHERE idempotency_key=$1`, idempotencyKey).Scan(
		&item.ID, &item.OrganizationID, &item.Amount, &item.BalanceAfter, &item.Type,
		&item.ReferenceType, &item.ReferenceID, &item.Notes, &item.Metadata,
		&item.IdempotencyKey, &item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &item, nil
}

func lockCreditWallet(ctx context.Context, tx pgx.Tx, orgID string) (*CreditWallet, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO organization_credit_wallets (organization_id)
		VALUES ($1::uuid) ON CONFLICT (organization_id) DO NOTHING`, orgID); err != nil {
		return nil, fmt.Errorf("crear monedero: %w", err)
	}
	var wallet CreditWallet
	err := tx.QueryRow(ctx, `SELECT organization_id::text, balance::float8,
		lifetime_credited::float8, lifetime_consumed::float8, low_balance_threshold::float8,
		created_at, updated_at
		FROM organization_credit_wallets WHERE organization_id=$1::uuid FOR UPDATE`, orgID).Scan(
		&wallet.OrganizationID, &wallet.Balance, &wallet.LifetimeCredited,
		&wallet.LifetimeConsumed, &wallet.LowBalanceThreshold,
		&wallet.CreatedAt, &wallet.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("bloquear monedero: %w", err)
	}
	return &wallet, nil
}

func insertCreditTransaction(ctx context.Context, tx pgx.Tx, orgID string, amount, balanceAfter float64,
	typeName, referenceType, referenceID, notes string, metadata map[string]any, idempotencyKey string) (*CreditTransaction, error) {
	metaBytes := json.RawMessage(`{}`)
	if len(metadata) > 0 {
		encoded, err := json.Marshal(metadata)
		if err != nil {
			return nil, fmt.Errorf("serializar metadata del ledger: %w", err)
		}
		metaBytes = encoded
	}
	var item CreditTransaction
	err := tx.QueryRow(ctx, `INSERT INTO organization_credit_transactions
		(organization_id,amount,balance_after,type,reference_type,reference_id,notes,metadata,idempotency_key)
		VALUES($1::uuid,$2,$3,$4,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),$8::jsonb,$9)
		RETURNING id::text, organization_id::text, amount::float8, balance_after::float8,
		type, COALESCE(reference_type,''), COALESCE(reference_id,''), COALESCE(notes,''),
		metadata, COALESCE(idempotency_key,''), created_at`, orgID, amount, balanceAfter,
		typeName, referenceType, referenceID, notes, metaBytes, idempotencyKey).Scan(
		&item.ID, &item.OrganizationID, &item.Amount, &item.BalanceAfter, &item.Type,
		&item.ReferenceType, &item.ReferenceID, &item.Notes, &item.Metadata,
		&item.IdempotencyKey, &item.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("registrar transacción de créditos: %w", err)
	}
	return &item, nil
}

func cloneCreditMetadata(source map[string]any) map[string]any {
	out := make(map[string]any, len(source)+3)
	for key, value := range source {
		out[key] = value
	}
	return out
}

// UpdateWalletSettings actualiza únicamente la alerta. El prepago puro no
// expone sobregiro como una preferencia del cliente.
func UpdateWalletSettings(ctx context.Context, pool *pgxpool.Pool, orgID string, lowBalanceThreshold float64) (*CreditWallet, error) {
	if lowBalanceThreshold < 0 || math.IsNaN(lowBalanceThreshold) || math.IsInf(lowBalanceThreshold, 0) {
		return nil, fmt.Errorf("el umbral no puede ser negativo ni infinito")
	}
	var w CreditWallet
	err := pool.QueryRow(ctx, `
		INSERT INTO organization_credit_wallets (organization_id, low_balance_threshold)
		VALUES ($1::uuid, $2)
		ON CONFLICT (organization_id) DO UPDATE
		SET low_balance_threshold = EXCLUDED.low_balance_threshold, updated_at = NOW()
		 RETURNING organization_id::text, balance::float8, lifetime_credited::float8,
		           lifetime_consumed::float8, low_balance_threshold::float8,
		           created_at, updated_at`,
		orgID, roundCredits(lowBalanceThreshold),
	).Scan(
		&w.OrganizationID, &w.Balance, &w.LifetimeCredited,
		&w.LifetimeConsumed, &w.LowBalanceThreshold,
		&w.CreatedAt, &w.UpdatedAt,
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
		       COALESCE(notes,''), metadata, COALESCE(idempotency_key,''), created_at
			  FROM organization_credit_transactions
			 WHERE organization_id = $1::uuid AND created_at < $2
			 ORDER BY created_at DESC LIMIT $3`,
			orgID, beforeCreatedAt, limit)
	} else {
		rows, err = pool.Query(ctx, `
		SELECT id::text, organization_id::text, amount::float8, balance_after::float8,
		       type, COALESCE(reference_type,''), COALESCE(reference_id,''),
		       COALESCE(notes,''), metadata, COALESCE(idempotency_key,''), created_at
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
			&tx.Notes, &tx.Metadata, &tx.IdempotencyKey, &tx.CreatedAt,
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
	Phone           string
	PaymentRecordID string
	Notes           string
}

// RechargePlatformCredits valida la procedencia comercial y acredita créditos a la organización identificada por su código de activación.
func RechargePlatformCredits(ctx context.Context, pool *pgxpool.Pool, input RechargePlatformCreditsInput) (*CreditWallet, error) {
	wallet, _, err := activatePlatformCreditRecharge(ctx, pool, platformCreditRechargeInput{
		SellerOrgID: input.SellerOrgID, ActivationCode: input.ActivationCode,
		PaymentRecordID: input.PaymentRecordID, Phone: input.Phone,
		Notes: input.Notes, ExpectedStatus: "aceptado", Source: "whatsapp_bot",
	})
	return wallet, err
}

type platformCreditRechargeInput struct {
	SellerOrgID     string
	ActivationCode  string
	PaymentRecordID string
	Phone           string
	Notes           string
	ExpectedStatus  string
	Source          string
	ApprovedBy      string
}

type creditPaymentRecord struct {
	OrganizationID string  `json:"organizacion_id"`
	ActivationCode string  `json:"codigo_activacion"`
	AmountPen      float64 `json:"monto"`
	Currency       string  `json:"moneda"`
	Credits        float64 `json:"creditos"`
	Operation      string  `json:"operacion"`
	Provider       string  `json:"proveedor"`
	OccurredAt     string  `json:"fecha"`
	Recipient      string  `json:"destinatario"`
	Status         string  `json:"estado"`
}

func activatePlatformCreditRecharge(ctx context.Context, pool *pgxpool.Pool, input platformCreditRechargeInput) (*CreditWallet, *CreditTransaction, error) {
	input.ActivationCode = strings.ToUpper(strings.TrimSpace(input.ActivationCode))
	input.PaymentRecordID = strings.TrimSpace(input.PaymentRecordID)
	input.Phone = NormalizePhone(input.Phone)
	if input.PaymentRecordID == "" {
		return nil, nil, fmt.Errorf("el comprobante es obligatorio")
	}
	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		return nil, nil, err
	}
	if input.SellerOrgID != "" && sellerOrgID != input.SellerOrgID {
		return nil, nil, fmt.Errorf("el bot no pertenece a la organización comercial")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	var raw json.RawMessage
	err = tx.QueryRow(ctx, `SELECT r.data FROM data_records r
		JOIN data_objects o ON o.id=r.object_id
		WHERE r.id=$1::uuid AND o.org_id=$2::uuid AND o.key='cobros'
		FOR UPDATE OF r`, input.PaymentRecordID, sellerOrgID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("el cobro no existe en la organización comercial")
	}
	if err != nil {
		return nil, nil, err
	}
	var payment creditPaymentRecord
	if err := json.Unmarshal(raw, &payment); err != nil {
		return nil, nil, fmt.Errorf("datos del cobro inválidos: %w", err)
	}
	payment.Status = strings.ToLower(strings.TrimSpace(payment.Status))
	payment.Currency = strings.ToUpper(strings.TrimSpace(payment.Currency))
	payment.Operation = strings.TrimSpace(payment.Operation)
	if validationErr := validatePaymentForRecharge(payment); validationErr != nil {
		if input.ExpectedStatus == "aceptado" {
			return nil, nil, deferAutomaticPaymentReview(ctx, tx, input.PaymentRecordID, validationErr)
		}
		return nil, nil, validationErr
	}
	if input.ExpectedStatus == "aceptado" {
		if validationErr := validateAutomaticPaymentReceipt(payment, time.Now()); validationErr != nil {
			return nil, nil, deferAutomaticPaymentReview(ctx, tx, input.PaymentRecordID, validationErr)
		}
	}

	targetOrgID, activationCode, err := rechargeTargetOrganization(ctx, tx, input.ActivationCode, payment)
	if err != nil {
		return nil, nil, err
	}
	if input.ExpectedStatus == "aceptado" {
		authorized, err := paymentRecipientAuthorized(ctx, tx, sellerOrgID, payment.Recipient)
		if err != nil {
			return nil, nil, err
		}
		if !authorized {
			return nil, nil, fmt.Errorf("el destinatario del comprobante no coincide con un método de pago activo")
		}
	}

	credits := SolesToCredits(payment.AmountPen)
	idempotencyKey := paymentCreditIdempotencyKey(sellerOrgID, payment)
	if err := lockCreditIdempotency(ctx, tx, idempotencyKey); err != nil {
		return nil, nil, err
	}
	if existing, err := creditTransactionByKey(ctx, tx, idempotencyKey); err != nil {
		return nil, nil, err
	} else if existing != nil {
		if existing.OrganizationID != targetOrgID || existing.Type != CreditTxRecharge || roundCredits(existing.Amount) != credits {
			return nil, nil, ErrCreditIdempotency
		}
		wallet, err := creditWalletTx(ctx, tx, targetOrgID)
		if err != nil {
			return nil, nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, err
		}
		return wallet, existing, nil
	}
	if payment.Status != input.ExpectedStatus {
		return nil, nil, fmt.Errorf("el cobro no está disponible para acreditar (estado actual: %s)", payment.Status)
	}

	notes := strings.TrimSpace(input.Notes)
	if notes == "" {
		notes = fmt.Sprintf("Recarga verificada S/ %.2f op=%s", payment.AmountPen, payment.Operation)
	}
	txRecord, err := addCreditsTx(ctx, tx, AddCreditsInput{
		OrgID: targetOrgID, Credits: credits, Type: CreditTxRecharge,
		ReferenceType: CreditRefPaymentRecords, ReferenceID: input.PaymentRecordID,
		Notes: notes, IdempotencyKey: idempotencyKey,
		Metadata: map[string]any{
			"activation_code": activationCode, "phone": input.Phone,
			"payment_record_id": input.PaymentRecordID, "operation": payment.Operation,
			"amount_pen": payment.AmountPen, "provider": payment.Provider,
			"source": input.Source, "approved_by": input.ApprovedBy,
		},
	})
	if err != nil {
		return nil, nil, err
	}
	confirmation := map[string]any{
		"estado": "confirmado", "creditos": credits,
		"organizacion_id": targetOrgID, "codigo_activacion": activationCode,
		"confirmado_en":  time.Now().UTC().Format(time.RFC3339),
		"confirmado_por": strings.TrimSpace(input.ApprovedBy),
	}
	confirmationRaw, _ := json.Marshal(confirmation)
	if _, err := tx.Exec(ctx, `UPDATE data_records SET data=data || $2::jsonb, updated_at=NOW()
		WHERE id=$1::uuid`, input.PaymentRecordID, confirmationRaw); err != nil {
		return nil, nil, fmt.Errorf("confirmar cobro: %w", err)
	}
	wallet, err := creditWalletTx(ctx, tx, targetOrgID)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return wallet, txRecord, nil
}

func validatePaymentForRecharge(payment creditPaymentRecord) error {
	if !finitePositive(payment.AmountPen) || payment.Currency != "PEN" || payment.Operation == "" {
		return fmt.Errorf("el cobro requiere importe PEN y número de operación válidos")
	}
	return nil
}

func validateAutomaticPaymentReceipt(payment creditPaymentRecord, now time.Time) error {
	if strings.TrimSpace(payment.Provider) == "" {
		return fmt.Errorf("el comprobante automático requiere proveedor")
	}
	occurredAt, err := time.Parse(time.RFC3339, strings.TrimSpace(payment.OccurredAt))
	if err != nil {
		return fmt.Errorf("el comprobante automático requiere fecha y hora RFC3339 válidas")
	}
	if occurredAt.After(now.Add(automaticPaymentFutureSkew)) {
		return fmt.Errorf("la fecha del comprobante está en el futuro")
	}
	if occurredAt.Before(now.Add(-automaticPaymentMaxAge)) {
		return fmt.Errorf("el comprobante supera las 72 horas permitidas para acreditación automática")
	}
	return nil
}

func deferAutomaticPaymentReview(ctx context.Context, tx pgx.Tx, paymentRecordID string, validationErr error) error {
	review := map[string]any{
		"estado": "pendiente",
		"notas":  "Revisión manual requerida: " + validationErr.Error(),
	}
	raw, _ := json.Marshal(review)
	if _, err := tx.Exec(ctx, `UPDATE data_records SET data=data || $2::jsonb, updated_at=NOW()
		WHERE id=$1::uuid`, paymentRecordID, raw); err != nil {
		return fmt.Errorf("marcar cobro para revisión: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("confirmar revisión manual del cobro: %w", err)
	}
	return validationErr
}

func rechargeTargetOrganization(ctx context.Context, tx pgx.Tx, inputCode string, payment creditPaymentRecord) (string, string, error) {
	code := strings.ToUpper(strings.TrimSpace(inputCode))
	storedCode := strings.ToUpper(strings.TrimSpace(payment.ActivationCode))
	if code == "" {
		code = storedCode
	}
	if code == "" && strings.TrimSpace(payment.OrganizationID) == "" {
		return "", "", fmt.Errorf("el cobro no identifica la organización destino")
	}
	if storedCode != "" && code != storedCode {
		return "", "", fmt.Errorf("el código de activación no coincide con el cobro")
	}
	var orgID, canonicalCode string
	if code != "" {
		err := tx.QueryRow(ctx, `SELECT id::text, activation_code FROM organizations
			WHERE upper(activation_code)=$1`, code).Scan(&orgID, &canonicalCode)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", fmt.Errorf("código de activación inválido")
		}
		if err != nil {
			return "", "", err
		}
	} else {
		err := tx.QueryRow(ctx, `SELECT id::text, activation_code FROM organizations
			WHERE id=$1::uuid`, payment.OrganizationID).Scan(&orgID, &canonicalCode)
		if err != nil {
			return "", "", fmt.Errorf("organización cliente no encontrada")
		}
	}
	if payment.OrganizationID != "" && payment.OrganizationID != orgID {
		return "", "", fmt.Errorf("la organización del cobro no coincide con el código de activación")
	}
	return orgID, strings.ToUpper(strings.TrimSpace(canonicalCode)), nil
}

func paymentCreditIdempotencyKey(sellerOrgID string, payment creditPaymentRecord) string {
	// El monto no participa: si alguien altera un registro ya acreditado, la
	// misma operación bancaria sigue siendo la misma operación económica.
	proof := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(payment.Provider)),
		strings.ToLower(strings.TrimSpace(payment.Operation)),
	}, "|")
	hash := sha256.Sum256([]byte(proof))
	return fmt.Sprintf("recharge:%s:%x", sellerOrgID, hash[:])
}

func creditWalletTx(ctx context.Context, tx pgx.Tx, orgID string) (*CreditWallet, error) {
	var wallet CreditWallet
	err := tx.QueryRow(ctx, `SELECT organization_id::text, balance::float8,
		lifetime_credited::float8, lifetime_consumed::float8, low_balance_threshold::float8,
		created_at, updated_at
		FROM organization_credit_wallets WHERE organization_id=$1::uuid`, orgID).Scan(
		&wallet.OrganizationID, &wallet.Balance, &wallet.LifetimeCredited,
		&wallet.LifetimeConsumed, &wallet.LowBalanceThreshold,
		&wallet.CreatedAt, &wallet.UpdatedAt)
	return &wallet, err
}

type PlatformPaymentInstruction struct {
	Key         string `json:"key"`
	Medium      string `json:"medium"`
	Destination string `json:"destination"`
	Holder      string `json:"holder"`
	Currency    string `json:"currency"`
	Note        string `json:"note,omitempty"`
}

func ListPlatformPaymentInstructions(ctx context.Context, pool *pgxpool.Pool) ([]PlatformPaymentInstruction, error) {
	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT COALESCE(r.data->>'clave',''),
		COALESCE(r.data->>'medio',''), COALESCE(r.data->>'destino',''),
		COALESCE(r.data->>'titular',''), COALESCE(r.data->>'moneda','PEN'),
		COALESCE(r.data->>'nota','')
		FROM data_records r JOIN data_objects o ON o.id=r.object_id
		WHERE o.org_id=$1::uuid AND o.key='instrucciones_pago_bawto'
		AND COALESCE((r.data->>'activo')::boolean,false)=true
		ORDER BY COALESCE((r.data->>'prioridad')::int,9999), r.created_at`, sellerOrgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PlatformPaymentInstruction, 0)
	for rows.Next() {
		var item PlatformPaymentInstruction
		if err := rows.Scan(&item.Key, &item.Medium, &item.Destination, &item.Holder, &item.Currency, &item.Note); err != nil {
			return nil, err
		}
		if strings.TrimSpace(item.Medium) == "" || strings.TrimSpace(item.Destination) == "" || strings.TrimSpace(item.Holder) == "" {
			continue
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no hay métodos de pago activos")
	}
	return items, nil
}

func paymentRecipientAuthorized(ctx context.Context, tx pgx.Tx, sellerOrgID, recipient string) (bool, error) {
	recipientID := normalizedPaymentIdentity(recipient)
	if len(recipientID) < 6 {
		return false, nil
	}
	rows, err := tx.Query(ctx, `SELECT COALESCE(r.data->>'destino',''), COALESCE(r.data->>'titular','')
		FROM data_records r JOIN data_objects o ON o.id=r.object_id
		WHERE o.org_id=$1::uuid AND o.key='instrucciones_pago_bawto'
		AND COALESCE((r.data->>'activo')::boolean,false)=true`, sellerOrgID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var destination, holder string
		if err := rows.Scan(&destination, &holder); err != nil {
			return false, err
		}
		for _, official := range []string{destination, holder} {
			officialID := normalizedPaymentIdentity(official)
			if len(officialID) >= 6 && (strings.Contains(recipientID, officialID) || strings.Contains(officialID, recipientID)) {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}

func normalizedPaymentIdentity(value string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			return unicode.ToLower(char)
		}
		return -1
	}, strings.TrimSpace(value))
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
		{"creditos", "Créditos", "number", false},
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
	if !finitePositive(input.AmountPen) {
		return nil, fmt.Errorf("monto en soles inválido")
	}
	input.AmountPen = roundCredits(input.AmountPen)
	credits := SolesToCredits(input.AmountPen)
	input.Operation = strings.TrimSpace(input.Operation)
	if input.Operation == "" {
		return nil, fmt.Errorf("el número de operación es obligatorio")
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
			"creditos":            credits,
			"operacion":           input.Operation,
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
		Credits:          credits,
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
	return activatePlatformCreditRecharge(ctx, pool, platformCreditRechargeInput{
		SellerOrgID: sellerOrgID, PaymentRecordID: recordID,
		ExpectedStatus: "pendiente", Source: "platform_review",
		ApprovedBy: strings.TrimSpace(approverEmail),
	})
}

// RejectPlatformCreditRecharge rechaza un cobro pendiente en la organización comercial.
func RejectPlatformCreditRecharge(ctx context.Context, pool *pgxpool.Pool, recordID string, reason string, rejecterEmail string) error {
	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	patch, _ := json.Marshal(map[string]any{
		"estado": "rechazado", "motivo_rechazo": strings.TrimSpace(reason),
		"rechazado_por": strings.TrimSpace(rejecterEmail), "rechazado_en": now.Format(time.RFC3339),
	})
	tag, err := pool.Exec(ctx, `UPDATE data_records r SET data=r.data || $4::jsonb, updated_at=NOW()
		FROM data_objects o WHERE r.id=$1::uuid AND o.id=r.object_id
		AND o.org_id=$2::uuid AND o.key='cobros' AND r.data->>'estado'=$3`,
		recordID, sellerOrgID, "pendiente", patch)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("el cobro no está pendiente o no existe")
	}
	return nil
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
