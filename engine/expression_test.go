package engine

import "testing"

func TestExpressionBooleanOperatorsAndFunctions(t *testing.T) {
	vars := map[string]string{
		"input.contentType": "image",
		"input.caption":     "Pago factura F-10",
		"amount":            "120.50",
	}
	tests := []struct {
		expression string
		want       bool
	}{
		{`input.contentType == image && contains(input.caption, "factura")`, true},
		{`input.contentType == audio || amount >= 100`, true},
		{`!(input.contentType in (audio, document))`, true},
		{`exists(input.caption) && empty(input.media.id)`, true},
		{`startsWith(input.caption, "Pago") && endsWith(input.caption, "F-10")`, true},
		{`input.contentType == text || amount < 10`, false},
	}
	for _, test := range tests {
		got, err := evalExpression(test.expression, vars)
		if err != nil {
			t.Fatalf("%s: %v", test.expression, err)
		}
		if got != test.want {
			t.Fatalf("%s: got %v want %v", test.expression, got, test.want)
		}
	}
}

func TestExpressionRejectsCodeLikeSyntax(t *testing.T) {
	for _, expression := range []string{`eval(input)`, `input == image; drop table x`, `input &&& true`} {
		if _, err := evalExpression(expression, nil); err == nil {
			t.Fatalf("se esperaba rechazar %q", expression)
		}
	}
}

func TestRouterUsesFirstMatchAndDefault(t *testing.T) {
	cases := []RouterCase{
		{ID: "media", Expression: `input.contentType in (image, document)`},
		{ID: "image", Expression: `input.contentType == image`},
	}
	if got := evalRouter(cases, map[string]string{"input.contentType": "image"}); got != "media" {
		t.Fatalf("debe ganar el primer caso: %s", got)
	}
	if got := evalRouter(cases, map[string]string{"input.contentType": "text"}); got != "default" {
		t.Fatalf("debe usar default: %s", got)
	}
}

// Comparar dos variables numéricas era imposible de publicar.
//
// `validateExpression` evalúa contra un mapa vacío, así que los dos lados
// llegaban en blanco y el validador rechazaba la expresión: `>=` solo servía
// entre literales, que no compara nada. Una variable ausente ahora es «no sé» y
// se resuelve como falso; un valor presente y no numérico sigue siendo error,
// porque eso sí es un fallo real.
func TestComparacionNumericaEntreVariables(t *testing.T) {
	const expr = "empty(receipt.amount) || receipt.amount >= pedido.total"

	if err := validateExpression(expr); err != nil {
		t.Fatalf("no se puede publicar una comparación entre variables: %v", err)
	}

	casos := []struct {
		nombre string
		vars   map[string]string
		quiere bool
	}{
		{"cubre el total", map[string]string{"receipt.amount": "17.8", "pedido.total": "17.8"}, true},
		{"paga de más", map[string]string{"receipt.amount": "20", "pedido.total": "17.8"}, true},
		{"paga de menos", map[string]string{"receipt.amount": "5", "pedido.total": "17.8"}, false},
		// Importe ilegible: lo cubre el empty() de la izquierda, no el error.
		{"importe ausente", map[string]string{"pedido.total": "17.8"}, true},
	}
	for _, caso := range casos {
		got, err := evalExpression(expr, caso.vars)
		if err != nil {
			t.Fatalf("%s: %v", caso.nombre, err)
		}
		if got != caso.quiere {
			t.Errorf("%s: got %v, esperado %v", caso.nombre, got, caso.quiere)
		}
	}

	// Un lado ausente sin empty() delante tampoco revienta: es falso.
	got, err := evalExpression("a >= b", map[string]string{"b": "1"})
	if err != nil || got {
		t.Fatalf("variable ausente: got=%v err=%v", got, err)
	}
	// Pero un valor presente que no es número sigue siendo un error.
	if _, err := evalExpression("a >= b", map[string]string{"a": "hola", "b": "1"}); err == nil {
		t.Fatal("un valor no numérico debe seguir siendo un error")
	}
}
