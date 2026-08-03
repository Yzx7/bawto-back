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
