package ai

import (
	"strings"
	"testing"
)

func TestResolvePricingUsaElCatalogo(t *testing.T) {
	pricing, err := ResolvePricing("deepseek", "deepseek-v4-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pricing.Rates.InputPerMillion != 0.14 || pricing.Rates.OutputPerMillion != 0.28 {
		t.Errorf("tarifas inesperadas: %+v", pricing.Rates)
	}
	if pricing.Provider != "deepseek" || pricing.Source == "" {
		t.Errorf("proveedor u origen sin rellenar: %+v", pricing)
	}
}

// El nombre viene de un .env escrito a mano; que la caja cambie el tarifario
// sería una trampa silenciosa.
func TestResolvePricingIgnoraMayusculas(t *testing.T) {
	pricing, err := ResolvePricing("", "MiniMax-M3", nil)
	if err != nil {
		t.Fatal(err)
	}
	if pricing.Rates.InputPerMillion != 0.30 || pricing.Provider != "minimax" {
		t.Errorf("no resolvió MiniMax-M3: %+v", pricing)
	}
}

// Este es el fallo que motiva todo: cambiar el modelo y olvidar el proveedor
// mete los dos tarifarios en el mismo cubo de los reportes.
func TestResolvePricingRechazaProveedorQueNoConcuerda(t *testing.T) {
	_, err := ResolvePricing("minimax", "deepseek-v4-flash", nil)
	if err == nil {
		t.Fatal("se esperaba rechazo de AI_PROVIDER=minimax con un modelo de deepseek")
	}
	if !strings.Contains(err.Error(), "deepseek") {
		t.Errorf("el error debería nombrar el proveedor correcto: %v", err)
	}
}

// Un modelo desconocido no puede heredar el precio del anterior: esas filas
// nacerían mal y el histórico es inmutable.
func TestResolvePricingRechazaModeloDesconocidoSinTarifas(t *testing.T) {
	_, err := ResolvePricing("deepseek", "deepseek-v9-imaginario", nil)
	if err == nil {
		t.Fatal("se esperaba rechazo de un modelo fuera del catálogo")
	}
	// El mensaje tiene que decir qué hacer, no solo que está mal.
	if !strings.Contains(err.Error(), "AI_INPUT_USD_PER_MILLION") ||
		!strings.Contains(err.Error(), "deepseek-v4-flash") {
		t.Errorf("el error debería listar la salida y el catálogo: %v", err)
	}
}

func TestResolvePricingAdmiteModeloDesconocidoConTarifas(t *testing.T) {
	override := &Rates{InputPerMillion: 1, OutputPerMillion: 2, CacheReadPerMillion: 3}
	pricing, err := ResolvePricing("otro", "modelo-nuevo", override)
	if err != nil {
		t.Fatal(err)
	}
	if pricing.Rates != *override || pricing.Provider != "otro" {
		t.Errorf("no respetó las tarifas explícitas: %+v", pricing)
	}
}

// El override existe para un cambio de precio del proveedor antes de que se
// actualice el catálogo, así que tiene que ganarle.
func TestResolvePricingElOverrideGanaAlCatalogo(t *testing.T) {
	override := &Rates{InputPerMillion: 0.99, OutputPerMillion: 0.88}
	pricing, err := ResolvePricing("deepseek", "deepseek-v4-flash", override)
	if err != nil {
		t.Fatal(err)
	}
	if pricing.Rates.InputPerMillion != 0.99 {
		t.Errorf("el catálogo pisó al override: %+v", pricing.Rates)
	}
}

func TestResolvePricingConTarifasExigeProveedor(t *testing.T) {
	if _, err := ResolvePricing("  ", "modelo-nuevo", &Rates{}); err == nil {
		t.Fatal("se esperaba rechazo de tarifas explícitas sin AI_PROVIDER")
	}
}

// El catálogo alimenta un mensaje de error y una decisión de facturación: una
// entrada sin proveedor o sin origen documentado no vale para ninguna de las dos.
func TestCatalogoCompleto(t *testing.T) {
	for _, model := range KnownModels() {
		pricing, ok := RatesFor(model)
		if !ok {
			t.Fatalf("KnownModels devolvió %q pero RatesFor no lo encuentra", model)
		}
		if pricing.Provider == "" || pricing.Source == "" {
			t.Errorf("%s: falta proveedor u origen del precio", model)
		}
		if pricing.Rates.InputPerMillion <= 0 || pricing.Rates.OutputPerMillion <= 0 {
			t.Errorf("%s: entrada y salida tienen que costar algo: %+v", model, pricing.Rates)
		}
	}
}
