package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joho/godotenv"
)

func TestCostUSDToCreditsConversion(t *testing.T) {
	cases := []struct {
		usd     float64
		credits float64
	}{
		{0.0, 0.0},
		{1.0, 400.0},
		{0.0025, 1.0},
		{0.000364, 0.1456}, // DeepSeek v4-flash llamada típica
		{0.00096, 0.384},   // MiniMax-M3 llamada típica
	}

	for _, tc := range cases {
		got := CostUSDToCredits(tc.usd)
		if got != tc.credits {
			t.Errorf("CostUSDToCredits(%f) = %f; want %f", tc.usd, got, tc.credits)
		}
	}
}

func TestSolesToCreditsConversion(t *testing.T) {
	cases := []struct {
		soles   float64
		credits float64
	}{
		{0.0, 0.0},
		{5.0, 100.0},   // 5 soles = 100 créditos
		{20.0, 400.0},  // 20 soles = 400 créditos
		{50.0, 1000.0}, // 50 soles = 1,000 créditos
		{100.0, 2000.0},
	}

	for _, tc := range cases {
		got := SolesToCredits(tc.soles)
		if got != tc.credits {
			t.Errorf("SolesToCredits(%f) = %f; want %f", tc.soles, got, tc.credits)
		}
		gotSoles := CreditsToSoles(tc.credits)
		if gotSoles != tc.soles {
			t.Errorf("CreditsToSoles(%f) = %f; want %f", tc.credits, gotSoles, tc.soles)
		}
	}
}

func TestPaymentCreditIdempotencyIgnoresMutableAmount(t *testing.T) {
	first := creditPaymentRecord{Provider: "yape", Operation: "001234", AmountPen: 5, Recipient: "Gerson"}
	altered := first
	altered.AmountPen = 500
	altered.Recipient = "Otro texto"
	if paymentCreditIdempotencyKey("seller", first) != paymentCreditIdempotencyKey("seller", altered) {
		t.Fatal("la misma operación bancaria no puede acreditarse otra vez alterando monto o destinatario")
	}
}

func TestValidateAutomaticPaymentReceipt(t *testing.T) {
	now := time.Date(2026, time.August, 13, 12, 0, 0, 0, time.FixedZone("PET", -5*60*60))
	valid := creditPaymentRecord{
		Provider: "bcp", AmountPen: 240, Currency: "PEN", Operation: "01391991",
		OccurredAt: now.Add(-time.Hour).Format(time.RFC3339), ResultText: "¡Operación exitosa!",
	}
	if err := validatePaymentForRecharge(valid); err != nil {
		t.Fatalf("comprobante económico válido: %v", err)
	}
	if err := validateAutomaticPaymentReceipt(valid, now); err != nil {
		t.Fatalf("comprobante automático reciente: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*creditPaymentRecord)
		want   string
	}{
		{"sin proveedor", func(p *creditPaymentRecord) { p.Provider = "" }, "requiere proveedor"},
		{"sin fecha", func(p *creditPaymentRecord) { p.OccurredAt = "" }, "requiere fecha"},
		{"antiguo", func(p *creditPaymentRecord) { p.OccurredAt = now.Add(-73 * time.Hour).Format(time.RFC3339) }, "72 horas"},
		{"futuro", func(p *creditPaymentRecord) { p.OccurredAt = now.Add(11 * time.Minute).Format(time.RFC3339) }, "en el futuro"},
		{"sin éxito visible", func(p *creditPaymentRecord) { p.ResultText = "Procesando" }, "resultado exitoso"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payment := valid
			tc.mutate(&payment)
			if err := validateAutomaticPaymentReceipt(payment, now); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("se esperaba error con %q, se obtuvo %v", tc.want, err)
			}
		})
	}
}

func TestPaymentReceiptSuccessfulUsesVisibleStatus(t *testing.T) {
	for _, visible := range []string{"¡Yapeaste!", "Operación exitosa", "PAGO EXITOSO", "Transferencia completada"} {
		if !paymentReceiptSuccessful(visible) {
			t.Errorf("debía reconocer %q", visible)
		}
	}
	for _, visible := range []string{"", "Procesando", "Operación rechazada", "Pendiente"} {
		if paymentReceiptSuccessful(visible) {
			t.Errorf("no debía reconocer %q", visible)
		}
	}
}

func TestAutomaticStalePaymentIsDeferredWithoutCredit(t *testing.T) {
	_ = godotenv.Load("../.env")
	pool, ctx := flowTestPool(t)
	sellerOrgID, err := PlatformSalesOrgID(ctx, pool)
	if err != nil {
		t.Fatalf("organización comercial: %v", err)
	}
	var objectID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM data_objects WHERE org_id=$1::uuid AND key='cobros'`, sellerOrgID).Scan(&objectID); err != nil {
		t.Fatalf("objeto cobros: %v", err)
	}
	payment := map[string]any{
		"monto": 240, "moneda": "PEN", "operacion": "STALE-" + randID("pay_"),
		"proveedor": "bcp", "fecha": time.Now().Add(-73 * time.Hour).Format(time.RFC3339),
		"destinatario": "Sistemuino", "estado": "aceptado",
	}
	raw, _ := json.Marshal(payment)
	var recordID string
	if err := pool.QueryRow(ctx, `INSERT INTO data_records(object_id, data) VALUES ($1::uuid, $2::jsonb) RETURNING id::text`, objectID, raw).Scan(&recordID); err != nil {
		t.Fatalf("crear cobro antiguo: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM data_records WHERE id=$1::uuid`, recordID) })

	_, err = RechargePlatformCredits(ctx, pool, RechargePlatformCreditsInput{
		SellerOrgID: sellerOrgID, ActivationCode: "NO-SE-DEBE-RESOLVER", PaymentRecordID: recordID,
	})
	if err == nil || !strings.Contains(err.Error(), "72 horas") {
		t.Fatalf("el cobro antiguo debía quedar en revisión, se obtuvo %v", err)
	}
	var status, notes string
	if err := pool.QueryRow(ctx, `SELECT data->>'estado', data->>'notas' FROM data_records WHERE id=$1::uuid`, recordID).Scan(&status, &notes); err != nil {
		t.Fatalf("releer cobro antiguo: %v", err)
	}
	if status != "pendiente" || !strings.Contains(notes, "72 horas") {
		t.Fatalf("revisión no persistida: estado=%q notas=%q", status, notes)
	}
	var ledgerCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM organization_credit_transactions WHERE reference_id=$1`, recordID).Scan(&ledgerCount); err != nil {
		t.Fatalf("consultar ledger: %v", err)
	}
	if ledgerCount != 0 {
		t.Fatalf("un comprobante antiguo creó %d movimientos", ledgerCount)
	}
}

func TestCreditWalletLifecycle(t *testing.T) {
	_ = godotenv.Load("../.env")
	pool, ctx := flowTestPool(t)
	bot := botDePrueba(t, ctx, pool, "cred_")

	// 1. Obtener wallet inicial (debe nacer con balance 0)
	w, err := GetOrCreateCreditWallet(ctx, pool, bot.OrgID)
	if err != nil {
		t.Fatalf("GetOrCreateCreditWallet: %v", err)
	}
	if w.Balance != 0 {
		t.Fatalf("saldo inicial esperado 0, obtenido %f", w.Balance)
	}

	// 2. Intentar deducir sin saldo (debe fallar con ErrInsufficientCredits)
	_, err = DeductCredits(ctx, pool, DeductCreditsInput{
		OrgID:          bot.OrgID,
		Credits:        10.0,
		Type:           CreditTxAIRuntimeUsage,
		IdempotencyKey: "test:no-balance",
	})
	if err != ErrInsufficientCredits {
		t.Fatalf("se esperaba ErrInsufficientCredits, obtenido: %v", err)
	}

	// 3. Abonar 100 créditos
	txAdd, err := AddCredits(ctx, pool, AddCreditsInput{
		OrgID:          bot.OrgID,
		Credits:        100.0,
		Type:           CreditTxRecharge,
		ReferenceType:  CreditRefPaymentRecords,
		ReferenceID:    "test-cobro-1",
		Notes:          "Recarga de prueba inicial",
		IdempotencyKey: "test:add-100",
	})
	if err != nil {
		t.Fatalf("AddCredits: %v", err)
	}
	if txAdd.Amount != 100.0 || txAdd.BalanceAfter != 100.0 {
		t.Fatalf("transacción de abono incorrecta: %+v", txAdd)
	}

	// 4. Deducir 25.5 créditos
	txDeduct, err := DeductCredits(ctx, pool, DeductCreditsInput{
		OrgID:          bot.OrgID,
		Credits:        25.5,
		Type:           CreditTxAIRuntimeUsage,
		ReferenceType:  CreditRefAIUsageEvents,
		ReferenceID:    "req-test-1",
		IdempotencyKey: "test:deduct-25.5",
	})
	if err != nil {
		t.Fatalf("DeductCredits: %v", err)
	}
	if txDeduct.Amount != -25.5 || txDeduct.BalanceAfter != 74.5 {
		t.Fatalf("transacción de deducción incorrecta: %+v", txDeduct)
	}

	// 5. Verificar Overview
	ov, err := GetCreditOverview(ctx, pool, bot.OrgID)
	if err != nil {
		t.Fatalf("GetCreditOverview: %v", err)
	}
	if ov.Wallet.Balance != 74.5 {
		t.Fatalf("balance en overview esperado 74.5, obtenido %f", ov.Wallet.Balance)
	}
	if ov.Wallet.LifetimeCredited != 100.0 || ov.Wallet.LifetimeConsumed != 25.5 {
		t.Fatalf("totales acumulados incorrectos: %+v", ov.Wallet)
	}
	if len(ov.RecentTransactions) != 2 {
		t.Fatalf("esperadas 2 transacciones, obtenidas %d", len(ov.RecentTransactions))
	}

	// 6. Test de concurrencia: 10 deducciones en paralelo de 1.0 crédito cada una
	var wg sync.WaitGroup
	errCh := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, dErr := DeductCredits(ctx, pool, DeductCreditsInput{
				OrgID:          bot.OrgID,
				Credits:        1.0,
				Type:           CreditTxAIRuntimeUsage,
				IdempotencyKey: fmt.Sprintf("test:concurrent:%d", idx),
			})
			if dErr != nil {
				errCh <- dErr
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	errCount := len(errCh)
	if errCount > 0 {
		t.Fatalf("fallaron %d deducciones concurrentes", errCount)
	}

	// Saldo debe ser 74.5 - 10 = 64.5
	wFinal, err := GetOrCreateCreditWallet(ctx, pool, bot.OrgID)
	if err != nil {
		t.Fatalf("GetOrCreateCreditWallet final: %v", err)
	}
	if wFinal.Balance != 64.5 {
		t.Fatalf("saldo final tras concurrencia esperado 64.5, obtenido %f", wFinal.Balance)
	}

	// 7. UpdateWalletSettings
	wUpdated, err := UpdateWalletSettings(ctx, pool, bot.OrgID, 50.0)
	if err != nil {
		t.Fatalf("UpdateWalletSettings: %v", err)
	}
	if wUpdated.LowBalanceThreshold != 50.0 {
		t.Fatalf("configuración de wallet no actualizada: %+v", wUpdated)
	}

	// 8. Repetir una misma operación devuelve el movimiento previo sin alterar saldo.
	repeated, err := AddCredits(ctx, pool, AddCreditsInput{
		OrgID: bot.OrgID, Credits: 100, Type: CreditTxRecharge,
		ReferenceType: CreditRefPaymentRecords, ReferenceID: "test-cobro-1",
		IdempotencyKey: "test:add-100",
	})
	if err != nil || repeated.ID != txAdd.ID {
		t.Fatalf("idempotencia de abono: tx=%+v err=%v", repeated, err)
	}
	wAfterRepeat, err := GetOrCreateCreditWallet(ctx, pool, bot.OrgID)
	if err != nil || wAfterRepeat.Balance != 64.5 {
		t.Fatalf("el reintento movió el saldo: wallet=%+v err=%v", wAfterRepeat, err)
	}

	if _, err := AddCredits(ctx, pool, AddCreditsInput{
		OrgID: bot.OrgID, Credits: math.NaN(), IdempotencyKey: "test:nan",
	}); !errors.Is(err, ErrInvalidCreditAmount) {
		t.Fatalf("NaN debe rechazarse, se obtuvo %v", err)
	}

	// 9. Dos goroutines con la misma operación económica acreditan una sola vez.
	var sameKeyWG sync.WaitGroup
	sameKeyResults := make(chan *CreditTransaction, 2)
	sameKeyErrors := make(chan error, 2)
	for range 2 {
		sameKeyWG.Add(1)
		go func() {
			defer sameKeyWG.Done()
			item, err := AddCredits(ctx, pool, AddCreditsInput{
				OrgID: bot.OrgID, Credits: 7, Type: CreditTxRecharge,
				ReferenceType: CreditRefPaymentRecords, ReferenceID: "same-payment",
				IdempotencyKey: "test:concurrent-same-payment",
			})
			if err != nil {
				sameKeyErrors <- err
				return
			}
			sameKeyResults <- item
		}()
	}
	sameKeyWG.Wait()
	close(sameKeyResults)
	close(sameKeyErrors)
	if len(sameKeyErrors) != 0 || len(sameKeyResults) != 2 {
		t.Fatalf("reintentos concurrentes: resultados=%d errores=%d", len(sameKeyResults), len(sameKeyErrors))
	}
	var firstID string
	for item := range sameKeyResults {
		if firstID == "" {
			firstID = item.ID
		} else if item.ID != firstID {
			t.Fatalf("una operación idempotente creó dos movimientos: %s y %s", firstID, item.ID)
		}
	}
	wAfterSameKey, err := GetOrCreateCreditWallet(ctx, pool, bot.OrgID)
	if err != nil || wAfterSameKey.Balance != 71.5 {
		t.Fatalf("el abono concurrente no fue exactamente uno: wallet=%+v err=%v", wAfterSameKey, err)
	}

	// 10. Uso IA y cargo comparten idempotencia: reentregar el mismo request no
	// duplica ni ai_usage_events ni el movimiento del monedero.
	providerRequestID := "credit-test-" + bot.ID
	charge := AIUsageChargeInput{
		CreditType:     CreditTxAIRuntimeUsage,
		IdempotencyKey: "ai:test:" + providerRequestID,
		Notes:          "prueba de cargo unido a usage",
		Usage: AIUsageEventInput{
			OrganizationID: bot.OrgID, BotID: bot.ID, Purpose: "flow_runtime",
			Provider: "credit-test", Model: "test-model", ProviderRequestID: providerRequestID,
			InputTokens: 1000, InputUSDPerMillion: 1,
		},
	}
	if err := RecordAIUsageAndChargeCredits(ctx, pool, charge); err != nil {
		t.Fatalf("primer cargo de usage: %v", err)
	}
	if err := RecordAIUsageAndChargeCredits(ctx, pool, charge); err != nil {
		t.Fatalf("reintento de cargo de usage: %v", err)
	}
	var usageCount, chargeCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM ai_usage_events WHERE provider=$1 AND provider_request_id=$2`,
		charge.Usage.Provider, providerRequestID).Scan(&usageCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM organization_credit_transactions WHERE idempotency_key=$1`,
		charge.IdempotencyKey).Scan(&chargeCount); err != nil {
		t.Fatal(err)
	}
	if usageCount != 1 || chargeCount != 1 {
		t.Fatalf("usage/cargo duplicado: usage=%d cargos=%d", usageCount, chargeCount)
	}
	wAfterUsage, err := GetOrCreateCreditWallet(ctx, pool, bot.OrgID)
	if err != nil || wAfterUsage.Balance != 71.1 {
		t.Fatalf("el usage debía cobrar 0.4 créditos una sola vez: wallet=%+v err=%v", wAfterUsage, err)
	}
}
