package models

import (
	"sync"
	"testing"

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
		{0.00096, 0.384},    // MiniMax-M3 llamada típica
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
		OrgID:   bot.OrgID,
		Credits: 10.0,
		Type:    CreditTxAIRuntimeUsage,
	})
	if err != ErrInsufficientCredits {
		t.Fatalf("se esperaba ErrInsufficientCredits, obtenido: %v", err)
	}

	// 3. Abonar 100 créditos
	txAdd, err := AddCredits(ctx, pool, AddCreditsInput{
		OrgID:         bot.OrgID,
		Credits:       100.0,
		Type:          CreditTxRecharge,
		ReferenceType: CreditRefPaymentRecords,
		ReferenceID:   "test-cobro-1",
		Notes:         "Recarga de prueba inicial",
	})
	if err != nil {
		t.Fatalf("AddCredits: %v", err)
	}
	if txAdd.Amount != 100.0 || txAdd.BalanceAfter != 100.0 {
		t.Fatalf("transacción de abono incorrecta: %+v", txAdd)
	}

	// 4. Deducir 25.5 créditos
	txDeduct, err := DeductCredits(ctx, pool, DeductCreditsInput{
		OrgID:         bot.OrgID,
		Credits:       25.5,
		Type:          CreditTxAIRuntimeUsage,
		ReferenceType: CreditRefAIUsageEvents,
		ReferenceID:   "req-test-1",
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
	errCount := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, dErr := DeductCredits(ctx, pool, DeductCreditsInput{
				OrgID:   bot.OrgID,
				Credits: 1.0,
				Type:    CreditTxAIRuntimeUsage,
			})
			if dErr != nil {
				errCount++
			}
		}(i)
	}
	wg.Wait()
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
	wUpdated, err := UpdateWalletSettings(ctx, pool, bot.OrgID, 50.0, true, 20.0)
	if err != nil {
		t.Fatalf("UpdateWalletSettings: %v", err)
	}
	if wUpdated.LowBalanceThreshold != 50.0 || !wUpdated.AllowOverage || wUpdated.OverageLimit != 20.0 {
		t.Fatalf("configuración de wallet no actualizada: %+v", wUpdated)
	}
}
