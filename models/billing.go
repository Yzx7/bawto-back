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

type BillingProfile struct {
	OrganizationID string  `json:"organizationId"`
	LegalName      string  `json:"legalName,omitempty"`
	TaxID          string  `json:"taxId,omitempty"`
	BillingEmail   string  `json:"billingEmail,omitempty"`
	PlanName       string  `json:"planName,omitempty"`
	Currency       string  `json:"currency"`
	TaxRate        float64 `json:"taxRate"`
	Status         string  `json:"status"`
	BillingDay     *int16  `json:"billingDay,omitempty"`
	Configured     bool    `json:"configured"`
}

type BillingServiceRate struct {
	Code          string          `json:"code"`
	Name          string          `json:"name"`
	Description   string          `json:"description,omitempty"`
	Metric        string          `json:"metric"`
	FixedQuantity float64         `json:"fixedQuantity"`
	UnitSize      float64         `json:"unitSize"`
	UnitLabel     string          `json:"unitLabel"`
	UnitPrice     float64         `json:"unitPrice"`
	Currency      string          `json:"currency"`
	Active        bool            `json:"active"`
	EffectiveFrom string          `json:"effectiveFrom"`
	EffectiveTo   string          `json:"effectiveTo,omitempty"`
	SortOrder     int             `json:"sortOrder"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

type BillingLine struct {
	ServiceCode string          `json:"serviceCode"`
	Description string          `json:"description"`
	Metric      string          `json:"metric"`
	RawUnits    float64         `json:"rawUnits"`
	Quantity    float64         `json:"quantity"`
	UnitLabel   string          `json:"unitLabel"`
	UnitPrice   float64         `json:"unitPrice"`
	Subtotal    float64         `json:"subtotal"`
	SortOrder   int             `json:"sortOrder"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
}

type BillingEstimate struct {
	PeriodStart time.Time     `json:"periodStart"`
	PeriodEnd   time.Time     `json:"periodEnd"`
	Currency    string        `json:"currency"`
	Subtotal    float64       `json:"subtotal"`
	TaxRate     float64       `json:"taxRate"`
	TaxAmount   float64       `json:"taxAmount"`
	Total       float64       `json:"total"`
	Lines       []BillingLine `json:"lines"`
	Configured  bool          `json:"configured"`
}

type BillingStatement struct {
	ID                     string        `json:"id"`
	OrganizationID         string        `json:"organizationId"`
	PeriodStart            time.Time     `json:"periodStart"`
	PeriodEnd              time.Time     `json:"periodEnd"`
	Status                 string        `json:"status"`
	Currency               string        `json:"currency"`
	Subtotal               float64       `json:"subtotal"`
	TaxRate                float64       `json:"taxRate"`
	TaxAmount              float64       `json:"taxAmount"`
	Total                  float64       `json:"total"`
	IssuedAt               *time.Time    `json:"issuedAt,omitempty"`
	DueAt                  *time.Time    `json:"dueAt,omitempty"`
	PaidAt                 *time.Time    `json:"paidAt,omitempty"`
	ExternalDocumentType   string        `json:"externalDocumentType,omitempty"`
	ExternalDocumentNumber string        `json:"externalDocumentNumber,omitempty"`
	Notes                  string        `json:"notes,omitempty"`
	CreatedAt              time.Time     `json:"createdAt"`
	Lines                  []BillingLine `json:"lines,omitempty"`
}

type OrganizationBillingOverview struct {
	Profile          BillingProfile     `json:"profile"`
	Estimate         BillingEstimate    `json:"estimate"`
	Usage            *CostReport        `json:"usage"`
	RecentStatements []BillingStatement `json:"recentStatements"`
	MetaBilledBy     string             `json:"metaBilledBy"`
	Disclaimer       string             `json:"disclaimer"`
}

func GetOrganizationBillingOverview(ctx context.Context, pool *pgxpool.Pool, orgID string, from, to time.Time) (*OrganizationBillingOverview, error) {
	profile, err := getBillingProfile(ctx, pool, orgID)
	if err != nil {
		return nil, err
	}
	usage, err := GetCostReport(ctx, pool, orgID, "", from, to)
	if err != nil {
		return nil, err
	}
	estimate, err := calculateBillingEstimate(ctx, pool, profile, usage, orgID, from, to)
	if err != nil {
		return nil, err
	}
	statements, err := ListBillingStatements(ctx, pool, orgID, 12)
	if err != nil {
		return nil, err
	}
	return &OrganizationBillingOverview{
		Profile:          *profile,
		Estimate:         *estimate,
		Usage:            usage,
		RecentStatements: statements,
		MetaBilledBy:     "meta_direct",
		Disclaimer: "El estado de cuenta incluye únicamente servicios de Bawto. " +
			"WhatsApp es cobrado directamente por Meta al método de pago del WABA y se muestra solo como referencia.",
	}, nil
}

func getBillingProfile(ctx context.Context, pool *pgxpool.Pool, orgID string) (*BillingProfile, error) {
	var profile BillingProfile
	err := pool.QueryRow(ctx, `
		SELECT organization_id::text,COALESCE(legal_name,''),COALESCE(tax_id,''),
		       COALESCE(billing_email,''),COALESCE(plan_name,''),currency,
		       tax_rate::float8,status,billing_day
		  FROM organization_billing_profiles
		 WHERE organization_id=$1::uuid`, orgID).
		Scan(&profile.OrganizationID, &profile.LegalName, &profile.TaxID,
			&profile.BillingEmail, &profile.PlanName, &profile.Currency,
			&profile.TaxRate, &profile.Status, &profile.BillingDay)
	if errors.Is(err, pgx.ErrNoRows) {
		org, orgErr := GetOrganization(ctx, pool, orgID)
		if orgErr != nil {
			return nil, orgErr
		}
		if org == nil {
			return nil, fmt.Errorf("organización no encontrada")
		}
		profile = BillingProfile{
			OrganizationID: orgID,
			LegalName:      org.Name,
			Currency:       "PEN",
			Status:         "unconfigured",
		}
		if org.RUC != nil {
			profile.TaxID = *org.RUC
		}
		if err := applySubscriptionProfile(ctx, pool, &profile); err != nil {
			return nil, err
		}
		return &profile, nil
	}
	if err != nil {
		return nil, err
	}
	profile.Configured = true
	if err := applySubscriptionProfile(ctx, pool, &profile); err != nil {
		return nil, err
	}
	return &profile, nil
}

// El perfil fiscal conserva razón social, correo e impuestos. El plan y su
// estado, en cambio, se derivan siempre del ledger Data para no mantener dos
// verdades que puedan divergir después de una venta o anulación por WhatsApp.
func applySubscriptionProfile(ctx context.Context, pool *pgxpool.Pool, profile *BillingProfile) error {
	subscription, err := GetOrganizationSubscription(ctx, pool, profile.OrganizationID)
	if err != nil {
		return err
	}
	profile.PlanName = subscription.PlanName
	profile.Status = "unconfigured"
	if subscription.Status == "activa" && subscription.EndsAt != nil && subscription.EndsAt.After(time.Now()) {
		profile.Status = "active"
	} else if subscription.Status == "cancelada" || subscription.Status == "vencida" ||
		(subscription.EndsAt != nil && !subscription.EndsAt.After(time.Now())) {
		profile.Status = "suspended"
	}
	return nil
}

func calculateBillingEstimate(ctx context.Context, pool *pgxpool.Pool, profile *BillingProfile, usage *CostReport, orgID string, from, to time.Time) (*BillingEstimate, error) {
	var activeBots int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM bots WHERE org_id=$1::uuid`, orgID).Scan(&activeBots); err != nil {
		return nil, err
	}
	rates, err := listCurrentBillingRates(ctx, pool, orgID, to.Add(-time.Nanosecond))
	if err != nil {
		return nil, err
	}
	estimate := &BillingEstimate{
		PeriodStart: from.UTC(),
		PeriodEnd:   to.UTC(),
		Currency:    profile.Currency,
		TaxRate:     profile.TaxRate,
		Lines:       make([]BillingLine, 0, len(rates)),
		Configured:  profile.Configured && len(rates) > 0,
	}
	for _, rate := range rates {
		if rate.Currency != profile.Currency {
			return nil, fmt.Errorf("tarifa %s usa %s y el perfil usa %s", rate.Code, rate.Currency, profile.Currency)
		}
		raw := billingMetricValue(rate, activeBots, usage.AI)
		qty := raw / rate.UnitSize
		line := BillingLine{
			ServiceCode: rate.Code,
			Description: rate.Name,
			Metric:      rate.Metric,
			RawUnits:    roundQuantity(raw),
			Quantity:    roundQuantity(qty),
			UnitLabel:   rate.UnitLabel,
			UnitPrice:   rate.UnitPrice,
			Subtotal:    roundStatementMoney(qty * rate.UnitPrice),
			SortOrder:   rate.SortOrder,
			Metadata:    rate.Metadata,
		}
		estimate.Lines = append(estimate.Lines, line)
		estimate.Subtotal += line.Subtotal
	}
	estimate.Subtotal = roundStatementMoney(estimate.Subtotal)
	estimate.TaxAmount = roundStatementMoney(estimate.Subtotal * estimate.TaxRate / 100)
	estimate.Total = roundStatementMoney(estimate.Subtotal + estimate.TaxAmount)
	return estimate, nil
}

func listCurrentBillingRates(ctx context.Context, pool *pgxpool.Pool, orgID string, at time.Time) ([]BillingServiceRate, error) {
	rows, err := pool.Query(ctx, `
		SELECT code,name,COALESCE(description,''),metric,fixed_quantity::float8,
		       unit_size::float8,unit_label,unit_price::float8,currency,active,
		       effective_from::text,COALESCE(effective_to::text,''),sort_order,metadata
		  FROM organization_service_rates
		 WHERE organization_id=$1::uuid AND active
		   AND effective_from <= $2::date
		   AND (effective_to IS NULL OR effective_to > $2::date)
		 ORDER BY sort_order,name`, orgID, at)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BillingServiceRate, 0)
	for rows.Next() {
		var rate BillingServiceRate
		if err := rows.Scan(&rate.Code, &rate.Name, &rate.Description, &rate.Metric,
			&rate.FixedQuantity, &rate.UnitSize, &rate.UnitLabel, &rate.UnitPrice,
			&rate.Currency, &rate.Active, &rate.EffectiveFrom, &rate.EffectiveTo,
			&rate.SortOrder, &rate.Metadata); err != nil {
			return nil, err
		}
		out = append(out, rate)
	}
	return out, rows.Err()
}

func billingMetricValue(rate BillingServiceRate, bots int64, ai AIUsageSummary) float64 {
	switch rate.Metric {
	case "active_bot":
		return float64(bots)
	case "ai_request":
		return float64(ai.Requests)
	case "ai_input_token":
		return float64(ai.InputTokens)
	case "ai_output_token":
		return float64(ai.OutputTokens)
	case "ai_total_token":
		return float64(ai.TotalTokens)
	default:
		return rate.FixedQuantity
	}
}

func ListBillingStatements(ctx context.Context, pool *pgxpool.Pool, orgID string, limit int) ([]BillingStatement, error) {
	if limit < 1 || limit > 100 {
		limit = 12
	}
	rows, err := pool.Query(ctx, `
		SELECT id::text,organization_id::text,period_start::timestamptz,
		       period_end::timestamptz,status,currency,subtotal::float8,
		       tax_rate::float8,tax_amount::float8,total::float8,issued_at,due_at,
		       paid_at,COALESCE(external_document_type,''),
		       COALESCE(external_document_number,''),COALESCE(notes,''),created_at
		  FROM billing_statements
		 WHERE organization_id=$1::uuid
		 ORDER BY period_start DESC,created_at DESC LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BillingStatement, 0)
	for rows.Next() {
		var item BillingStatement
		if err := rows.Scan(&item.ID, &item.OrganizationID, &item.PeriodStart,
			&item.PeriodEnd, &item.Status, &item.Currency, &item.Subtotal,
			&item.TaxRate, &item.TaxAmount, &item.Total, &item.IssuedAt,
			&item.DueAt, &item.PaidAt, &item.ExternalDocumentType,
			&item.ExternalDocumentNumber, &item.Notes, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func GetBillingStatement(ctx context.Context, pool *pgxpool.Pool, orgID, statementID string) (*BillingStatement, error) {
	var statement BillingStatement
	err := pool.QueryRow(ctx, `
		SELECT id::text,organization_id::text,period_start::timestamptz,
		       period_end::timestamptz,status,currency,subtotal::float8,
		       tax_rate::float8,tax_amount::float8,total::float8,issued_at,due_at,
		       paid_at,COALESCE(external_document_type,''),
		       COALESCE(external_document_number,''),COALESCE(notes,''),created_at
		  FROM billing_statements
		 WHERE organization_id=$1::uuid AND id=$2::uuid`, orgID, statementID).
		Scan(&statement.ID, &statement.OrganizationID, &statement.PeriodStart,
			&statement.PeriodEnd, &statement.Status, &statement.Currency,
			&statement.Subtotal, &statement.TaxRate, &statement.TaxAmount,
			&statement.Total, &statement.IssuedAt, &statement.DueAt,
			&statement.PaidAt, &statement.ExternalDocumentType,
			&statement.ExternalDocumentNumber, &statement.Notes,
			&statement.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT service_code,description,metric,raw_units::float8,quantity::float8,
		       unit_label,unit_price::float8,subtotal::float8,sort_order,metadata
		  FROM billing_statement_items WHERE statement_id=$1::uuid
		 ORDER BY sort_order,created_at`, statementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	statement.Lines = make([]BillingLine, 0)
	for rows.Next() {
		var line BillingLine
		if err := rows.Scan(&line.ServiceCode, &line.Description, &line.Metric,
			&line.RawUnits, &line.Quantity, &line.UnitLabel, &line.UnitPrice,
			&line.Subtotal, &line.SortOrder, &line.Metadata); err != nil {
			return nil, err
		}
		statement.Lines = append(statement.Lines, line)
	}
	return &statement, rows.Err()
}

type BillingConfig struct {
	Profile BillingProfile       `json:"profile"`
	Rates   []BillingServiceRate `json:"rates"`
}

// SyncBillingConfig es para herramientas internas, no para una ruta del
// cliente. Reemplaza el conjunto activo sin borrar el historial.
func SyncBillingConfig(ctx context.Context, pool *pgxpool.Pool, orgID string, config BillingConfig) error {
	config.Profile.Currency = strings.ToUpper(strings.TrimSpace(config.Profile.Currency))
	if config.Profile.Currency == "" {
		config.Profile.Currency = "PEN"
	}
	if config.Profile.Status == "" {
		config.Profile.Status = "active"
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `
		INSERT INTO organization_billing_profiles
		    (organization_id,legal_name,tax_id,billing_email,plan_name,currency,
		     tax_rate,status,billing_day,updated_at)
		VALUES ($1::uuid,NULLIF($2,''),NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),
		        $6,$7,$8,$9,NOW())
		ON CONFLICT (organization_id) DO UPDATE SET
		    legal_name=EXCLUDED.legal_name,tax_id=EXCLUDED.tax_id,
		    billing_email=EXCLUDED.billing_email,plan_name=EXCLUDED.plan_name,
		    currency=EXCLUDED.currency,tax_rate=EXCLUDED.tax_rate,
		    status=EXCLUDED.status,billing_day=EXCLUDED.billing_day,updated_at=NOW()`,
		orgID, config.Profile.LegalName, config.Profile.TaxID,
		config.Profile.BillingEmail, config.Profile.PlanName,
		config.Profile.Currency, config.Profile.TaxRate,
		config.Profile.Status, config.Profile.BillingDay)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE organization_service_rates SET active=FALSE,updated_at=NOW() WHERE organization_id=$1::uuid`, orgID); err != nil {
		return err
	}
	for i, rate := range config.Rates {
		if strings.TrimSpace(rate.Code) == "" || strings.TrimSpace(rate.Name) == "" {
			return fmt.Errorf("rates[%d] requiere code y name", i)
		}
		if rate.UnitSize <= 0 {
			rate.UnitSize = 1
		}
		if rate.UnitLabel == "" {
			rate.UnitLabel = "unidad"
		}
		if rate.Currency == "" {
			rate.Currency = config.Profile.Currency
		}
		if rate.EffectiveFrom == "" {
			rate.EffectiveFrom = time.Now().UTC().Format("2006-01-02")
		}
		if rate.Metric == "" {
			rate.Metric = "fixed"
		}
		if len(rate.Metadata) == 0 {
			rate.Metadata = json.RawMessage(`{}`)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO organization_service_rates
			    (organization_id,code,name,description,metric,fixed_quantity,
			     unit_size,unit_label,unit_price,currency,active,effective_from,
			     effective_to,sort_order,metadata,updated_at)
			VALUES ($1::uuid,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,TRUE,
			        $11::date,NULLIF($12,'')::date,$13,$14::jsonb,NOW())
			ON CONFLICT (organization_id,code,effective_from) DO UPDATE SET
			    name=EXCLUDED.name,description=EXCLUDED.description,
			    metric=EXCLUDED.metric,fixed_quantity=EXCLUDED.fixed_quantity,
			    unit_size=EXCLUDED.unit_size,unit_label=EXCLUDED.unit_label,
			    unit_price=EXCLUDED.unit_price,currency=EXCLUDED.currency,
			    active=TRUE,effective_to=EXCLUDED.effective_to,
			    sort_order=EXCLUDED.sort_order,metadata=EXCLUDED.metadata,
			    updated_at=NOW()`,
			orgID, rate.Code, rate.Name, rate.Description, rate.Metric,
			rate.FixedQuantity, rate.UnitSize, rate.UnitLabel, rate.UnitPrice,
			strings.ToUpper(rate.Currency), rate.EffectiveFrom, rate.EffectiveTo,
			rate.SortOrder, rate.Metadata)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func IssueBillingStatement(ctx context.Context, pool *pgxpool.Pool, orgID string, from, to, dueAt time.Time, notes string) (*BillingStatement, error) {
	profile, err := getBillingProfile(ctx, pool, orgID)
	if err != nil {
		return nil, err
	}
	usage, err := GetCostReport(ctx, pool, orgID, "", from, to)
	if err != nil {
		return nil, err
	}
	estimate, err := calculateBillingEstimate(ctx, pool, profile, usage, orgID, from, to)
	if err != nil {
		return nil, err
	}
	if !estimate.Configured {
		return nil, fmt.Errorf("la organización no tiene tarifas de Bawto configuradas")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id string
	now := time.Now().UTC()
	err = tx.QueryRow(ctx, `
		INSERT INTO billing_statements
		    (organization_id,period_start,period_end,status,currency,subtotal,
		     tax_rate,tax_amount,total,issued_at,due_at,notes)
		VALUES ($1::uuid,$2::date,$3::date,'issued',$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''))
		RETURNING id::text`, orgID, from, to, estimate.Currency,
		estimate.Subtotal, estimate.TaxRate, estimate.TaxAmount, estimate.Total,
		now, nullableTime(dueAt), notes).Scan(&id)
	if err != nil {
		return nil, err
	}
	for _, line := range estimate.Lines {
		metadata := line.Metadata
		if len(metadata) == 0 {
			metadata = json.RawMessage(`{}`)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO billing_statement_items
			    (statement_id,service_code,description,metric,raw_units,quantity,
			     unit_label,unit_price,subtotal,sort_order,metadata)
			VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
			id, line.ServiceCode, line.Description, line.Metric, line.RawUnits,
			line.Quantity, line.UnitLabel, line.UnitPrice, line.Subtotal,
			line.SortOrder, metadata); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return GetBillingStatement(ctx, pool, orgID, id)
}

func MarkBillingStatementPaid(ctx context.Context, pool *pgxpool.Pool, statementID, externalType, externalNumber string) error {
	tag, err := pool.Exec(ctx, `
		UPDATE billing_statements SET status='paid',paid_at=NOW(),
		       external_document_type=NULLIF($2,''),
		       external_document_number=NULLIF($3,''),updated_at=NOW()
		 WHERE id=$1::uuid AND status IN ('draft','issued')`,
		statementID, externalType, externalNumber)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("estado de cuenta no encontrado o no cobrable")
	}
	return nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func roundStatementMoney(value float64) float64 {
	return math.Round(value*100) / 100
}

func roundQuantity(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}
