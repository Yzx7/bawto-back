package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	PlatformPlansObject         = "planes_bawto"
	PlatformSubscriptionsObject = "suscripciones_bawto"
)

type PlatformPlan struct {
	Key                   string   `json:"key"`
	Name                  string   `json:"name"`
	MonthlyPrice          float64  `json:"monthlyPrice"`
	QuarterlyMonthlyPrice float64  `json:"quarterlyMonthlyPrice"`
	QuarterlyTotal        float64  `json:"quarterlyTotal"`
	Currency              string   `json:"currency"`
	Calls                 string   `json:"calls"`
	Benefits              []string `json:"benefits"`
}

type OrganizationSubscription struct {
	RecordID           string     `json:"recordId,omitempty"`
	OrganizationID     string     `json:"organizationId"`
	OrganizationName   string     `json:"organizationName,omitempty"`
	ActivationCode     string     `json:"activationCode"`
	PlanKey            string     `json:"planKey,omitempty"`
	PlanName           string     `json:"planName,omitempty"`
	BillingCycle       string     `json:"billingCycle,omitempty"`
	Amount             float64    `json:"amount,omitempty"`
	Currency           string     `json:"currency,omitempty"`
	Phone              string     `json:"phone,omitempty"`
	PaymentRecordID    string     `json:"paymentRecordId,omitempty"`
	OperationCode      string     `json:"operationCode,omitempty"`
	Status             string     `json:"status"`
	StartsAt           *time.Time `json:"startsAt,omitempty"`
	EndsAt             *time.Time `json:"endsAt,omitempty"`
	CancelledAt        *time.Time `json:"cancelledAt,omitempty"`
	CancellationReason string     `json:"cancellationReason,omitempty"`
}

type ActivateSubscriptionInput struct {
	SellerOrgID     string
	ActivationCode  string
	PlanKey         string
	BillingCycle    string
	Phone           string
	PaymentRecordID string
	IdempotencyKey  string
}

// PlatformSalesOrgID identifica al dueño del catálogo reservado. El índice
// parcial de la migración garantiza que sólo exista uno en toda la instalación.
func PlatformSalesOrgID(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var orgID string
	err := pool.QueryRow(ctx, `SELECT org_id::text FROM data_objects WHERE key=$1`, PlatformSubscriptionsObject).Scan(&orgID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = pool.QueryRow(ctx, `SELECT id::text FROM organizations WHERE lower(name) LIKE '%sistemuino%' ORDER BY created_at LIMIT 1`).Scan(&orgID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("la organización comercial Sistemuino no está configurada")
		}
		return orgID, nil
	}
	return orgID, err
}

func GetOrganizationSubscription(ctx context.Context, pool *pgxpool.Pool, orgID string) (*OrganizationSubscription, error) {
	org, err := GetOrganization(ctx, pool, orgID)
	if err != nil || org == nil {
		return nil, err
	}
	out := &OrganizationSubscription{
		OrganizationID: org.ID, OrganizationName: org.Name,
		ActivationCode: org.ActivationCode, Status: "sin_plan",
	}
	row := pool.QueryRow(ctx, subscriptionSelect+`
		WHERE o.key=$1 AND r.data->>'organizacion_id'=$2
		ORDER BY r.updated_at DESC LIMIT 1`, PlatformSubscriptionsObject, orgID)
	if err := scanSubscription(row, out); errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	return out, nil
}

func ListPlatformSubscriptions(ctx context.Context, pool *pgxpool.Pool) ([]OrganizationSubscription, error) {
	rows, err := pool.Query(ctx, subscriptionSelect+`
		WHERE o.key=$1 ORDER BY r.updated_at DESC`, PlatformSubscriptionsObject)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]OrganizationSubscription, 0)
	for rows.Next() {
		var item OrganizationSubscription
		if err := scanSubscription(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

const subscriptionSelect = `SELECT r.id::text,
	COALESCE(r.data->>'organizacion_id',''),COALESCE(org.name,''),COALESCE(org.activation_code,''),
	COALESCE(r.data->>'plan_clave',''),COALESCE(r.data->>'plan_nombre',''),
	COALESCE(r.data->>'ciclo',''),COALESCE((r.data->>'monto')::numeric,0)::float8,
	COALESCE(r.data->>'moneda',''),COALESCE(r.data->>'telefono',''),
	COALESCE(r.data->>'cobro_id',''),COALESCE(r.data->>'operacion',''),
	COALESCE(r.data->>'estado',''),
	CASE WHEN COALESCE(r.data->>'vigente_desde','')='' THEN NULL ELSE (r.data->>'vigente_desde')::timestamptz END,
	CASE WHEN COALESCE(r.data->>'vigente_hasta','')='' THEN NULL ELSE (r.data->>'vigente_hasta')::timestamptz END,
	CASE WHEN COALESCE(r.data->>'cancelado_en','')='' THEN NULL ELSE (r.data->>'cancelado_en')::timestamptz END,
	COALESCE(r.data->>'motivo_anulacion','')
	FROM data_records r JOIN data_objects o ON o.id=r.object_id
	LEFT JOIN organizations org ON org.id::text=r.data->>'organizacion_id' `

type rowScanner interface{ Scan(dest ...any) error }

func scanSubscription(row rowScanner, out *OrganizationSubscription) error {
	return row.Scan(&out.RecordID, &out.OrganizationID, &out.OrganizationName, &out.ActivationCode,
		&out.PlanKey, &out.PlanName, &out.BillingCycle, &out.Amount, &out.Currency,
		&out.Phone, &out.PaymentRecordID, &out.OperationCode, &out.Status,
		&out.StartsAt, &out.EndsAt, &out.CancelledAt, &out.CancellationReason)
}

func ActivatePlatformSubscription(ctx context.Context, pool *pgxpool.Pool, input ActivateSubscriptionInput) (*OrganizationSubscription, error) {
	input.ActivationCode = strings.ToUpper(strings.TrimSpace(input.ActivationCode))
	input.PlanKey = strings.ToLower(strings.TrimSpace(input.PlanKey))
	input.BillingCycle = strings.ToLower(strings.TrimSpace(input.BillingCycle))
	input.Phone = NormalizePhone(input.Phone)
	if input.BillingCycle != "monthly" && input.BillingCycle != "quarterly" {
		return nil, fmt.Errorf("ciclo inválido")
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
	// Dos comprobantes distintos para la misma organización podrían usar claves
	// idempotentes distintas. Este lock adicional serializa el upsert lógico por
	// organización y evita dos filas activas en una carrera.
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer lockConn.Release()
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1,0))`, "subscription:"+targetOrgID); err != nil {
		return nil, err
	}
	defer func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1,0))`, "subscription:"+targetOrgID)
	}()
	plan, err := platformPlan(ctx, pool, input.PlanKey)
	if err != nil {
		return nil, err
	}
	operation := ""
	if input.PaymentRecordID != "" {
		operation, err = paymentOperation(ctx, pool, sellerOrgID, input.PaymentRecordID)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	endsAt := now.AddDate(0, 1, 0)
	amount := plan.MonthlyPrice
	if input.BillingCycle == "quarterly" {
		endsAt = now.AddDate(0, 3, 0)
		amount = plan.QuarterlyTotal
	}
	benefits, _ := json.Marshal(plan.Benefits)
	result, err := MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID: sellerOrgID, ObjectKey: PlatformSubscriptionsObject, Operation: "upsert",
		MatchField: "organizacion_id", MatchValue: targetOrgID,
		IdempotencyKey: input.IdempotencyKey,
		Values: map[string]any{
			"organizacion_id": targetOrgID, "codigo_activacion": input.ActivationCode,
			"plan_clave": plan.Key, "plan_nombre": plan.Name, "ciclo": input.BillingCycle,
			"monto": amount, "moneda": plan.Currency, "telefono": input.Phone,
			"cobro_id": input.PaymentRecordID, "operacion": operation,
			"estado": "activa", "vigente_desde": now.Format(time.RFC3339),
			"vigente_hasta": endsAt.Format(time.RFC3339), "cancelado_en": "",
			"motivo_anulacion": "", "beneficios": string(benefits),
		},
	})
	if err != nil {
		return nil, err
	}
	_ = result
	return GetOrganizationSubscription(ctx, pool, targetOrgID)
}

func CancelPlatformSubscription(ctx context.Context, pool *pgxpool.Pool, sellerOrgID, recordID, reason string, blockPhone bool) (*OrganizationSubscription, error) {
	var targetOrgID, phone string
	err := pool.QueryRow(ctx, `SELECT COALESCE(r.data->>'organizacion_id',''),COALESCE(r.data->>'telefono','')
		FROM data_records r JOIN data_objects o ON o.id=r.object_id
		WHERE r.id=$1::uuid AND o.org_id=$2::uuid AND o.key=$3`, recordID, sellerOrgID, PlatformSubscriptionsObject).
		Scan(&targetOrgID, &phone)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_, err = MutateDataRecord(ctx, pool, DataMutationInput{
		OrgID: sellerOrgID, ObjectKey: PlatformSubscriptionsObject, Operation: "update", RecordID: recordID,
		IdempotencyKey: fmt.Sprintf("cancel:%s:%d", recordID, now.UnixNano()),
		Values: map[string]any{"estado": "cancelada", "vigente_hasta": now.Format(time.RFC3339),
			"cancelado_en": now.Format(time.RFC3339), "motivo_anulacion": strings.TrimSpace(reason)},
	})
	if err != nil {
		return nil, err
	}
	if blockPhone && phone != "" {
		if _, err := pool.Exec(ctx, `UPDATE contacts SET status='blocked',updated_at=NOW()
			WHERE org_id=$1::uuid AND phone_normalized=$2`, sellerOrgID, NormalizePhone(phone)); err != nil {
			return nil, err
		}
	}
	return GetOrganizationSubscription(ctx, pool, targetOrgID)
}

func platformPlan(ctx context.Context, pool *pgxpool.Pool, key string) (*PlatformPlan, error) {
	var raw json.RawMessage
	err := pool.QueryRow(ctx, `SELECT r.data FROM data_records r JOIN data_objects o ON o.id=r.object_id
		WHERE o.key=$1 AND lower(r.data->>'clave')=$2 AND COALESCE(r.data->>'activo','true')='true'
		ORDER BY r.updated_at DESC LIMIT 1`, PlatformPlansObject, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("plan inválido")
	}
	if err != nil {
		return nil, err
	}
	var data struct {
		Key              string  `json:"clave"`
		Name             string  `json:"nombre"`
		Monthly          float64 `json:"precio_mensual"`
		QuarterlyMonthly float64 `json:"precio_trimestral_mensual"`
		QuarterlyTotal   float64 `json:"monto_trimestral"`
		Currency         string  `json:"moneda"`
		Calls            string  `json:"llamadas"`
		Benefits         string  `json:"beneficios"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	return &PlatformPlan{Key: data.Key, Name: data.Name, MonthlyPrice: data.Monthly,
		QuarterlyMonthlyPrice: data.QuarterlyMonthly, QuarterlyTotal: data.QuarterlyTotal,
		Currency: data.Currency, Calls: data.Calls, Benefits: splitBenefits(data.Benefits)}, nil
}

func paymentOperation(ctx context.Context, pool *pgxpool.Pool, orgID, recordID string) (string, error) {
	var operation string
	err := pool.QueryRow(ctx, `SELECT COALESCE(r.data->>'operacion','') FROM data_records r
		JOIN data_objects o ON o.id=r.object_id
		WHERE r.id=$1::uuid AND o.org_id=$2::uuid AND o.key='cobros'`, recordID, orgID).Scan(&operation)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("el cobro no existe en la organización comercial")
	}
	return operation, err
}

func splitBenefits(value string) []string {
	parts := strings.Split(value, "|")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
