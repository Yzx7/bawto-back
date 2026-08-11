package engine

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Este evaluador es intencionalmente pequeño y determinista. Es el contrato
// compartido por Condition y Router y nunca ejecuta código, SQL o JavaScript.
// Gramática soportada:
//
//   expr        := or
//   or          := and ("||" and)*
//   and         := unary ("&&" unary)*
//   unary       := "!" unary | "(" expr ")" | predicate
//   predicate   := value [comparison value | "in" "(" value ("," value)* ")"]
//                | exists(value) | empty(value) | contains(value, value)
//                | startsWith(value, value) | endsWith(value, value)
//
// Un identificador se resuelve contra vars; si no existe se trata como literal
// para conservar expresiones legadas como `estado == valido`.

type expressionTokenKind uint8

const (
	expressionEOF expressionTokenKind = iota
	expressionWord
	expressionString
	expressionOperator
	expressionLParen
	expressionRParen
	expressionComma
)

type expressionToken struct {
	kind  expressionTokenKind
	value string
	pos   int
}

type expressionValue struct {
	raw    string
	value  string
	exists bool
}

type expressionParser struct {
	tokens []expressionToken
	index  int
	vars   map[string]string
}

func evalExpression(expression string, vars map[string]string) (bool, error) {
	tokens, err := lexExpression(expression)
	if err != nil {
		return false, err
	}
	parser := expressionParser{tokens: tokens, vars: vars}
	result, err := parser.parseOr()
	if err != nil {
		return false, err
	}
	if token := parser.current(); token.kind != expressionEOF {
		return false, fmt.Errorf("token inesperado %q en posición %d", token.value, token.pos)
	}
	return result, nil
}

func validateExpression(expression string) error {
	if strings.TrimSpace(expression) == "" {
		return fmt.Errorf("la expresión está vacía")
	}
	_, err := evalExpression(expression, map[string]string{})
	return err
}

func lexExpression(expression string) ([]expressionToken, error) {
	var tokens []expressionToken
	for index := 0; index < len(expression); {
		r := rune(expression[index])
		if unicode.IsSpace(r) {
			index++
			continue
		}
		start := index
		switch expression[index] {
		case '(':
			tokens = append(tokens, expressionToken{kind: expressionLParen, value: "(", pos: index})
			index++
		case ')':
			tokens = append(tokens, expressionToken{kind: expressionRParen, value: ")", pos: index})
			index++
		case ',':
			tokens = append(tokens, expressionToken{kind: expressionComma, value: ",", pos: index})
			index++
		case '\'', '"':
			quote := expression[index]
			index++
			var value strings.Builder
			closed := false
			for index < len(expression) {
				char := expression[index]
				index++
				if char == quote {
					closed = true
					break
				}
				if char == '\\' && index < len(expression) {
					char = expression[index]
					index++
				}
				value.WriteByte(char)
			}
			if !closed {
				return nil, fmt.Errorf("texto sin cerrar en posición %d", start)
			}
			tokens = append(tokens, expressionToken{kind: expressionString, value: value.String(), pos: start})
		case '&', '|', '=', '!', '>', '<':
			index++
			if index < len(expression) {
				pair := expression[start : index+1]
				if pair == "&&" || pair == "||" || pair == "==" || pair == "!=" || pair == ">=" || pair == "<=" {
					index++
					tokens = append(tokens, expressionToken{kind: expressionOperator, value: pair, pos: start})
					continue
				}
			}
			single := expression[start:index]
			if single != "!" && single != ">" && single != "<" {
				return nil, fmt.Errorf("operador inválido %q en posición %d", single, start)
			}
			tokens = append(tokens, expressionToken{kind: expressionOperator, value: single, pos: start})
		default:
			for index < len(expression) && !isExpressionDelimiter(expression[index]) {
				index++
			}
			if index == start {
				return nil, fmt.Errorf("carácter inválido en posición %d", start)
			}
			tokens = append(tokens, expressionToken{kind: expressionWord, value: expression[start:index], pos: start})
		}
	}
	tokens = append(tokens, expressionToken{kind: expressionEOF, pos: len(expression)})
	return tokens, nil
}

func isExpressionDelimiter(char byte) bool {
	return unicode.IsSpace(rune(char)) || strings.ContainsRune("(),!<>=&|\"'", rune(char))
}

func (parser *expressionParser) current() expressionToken {
	if parser.index >= len(parser.tokens) {
		return expressionToken{kind: expressionEOF}
	}
	return parser.tokens[parser.index]
}

func (parser *expressionParser) advance() expressionToken {
	token := parser.current()
	if parser.index < len(parser.tokens) {
		parser.index++
	}
	return token
}

func (parser *expressionParser) parseOr() (bool, error) {
	left, err := parser.parseAnd()
	if err != nil {
		return false, err
	}
	for parser.current().kind == expressionOperator && parser.current().value == "||" {
		parser.advance()
		right, parseErr := parser.parseAnd()
		if parseErr != nil {
			return false, parseErr
		}
		left = left || right
	}
	return left, nil
}

func (parser *expressionParser) parseAnd() (bool, error) {
	left, err := parser.parseUnary()
	if err != nil {
		return false, err
	}
	for parser.current().kind == expressionOperator && parser.current().value == "&&" {
		parser.advance()
		right, parseErr := parser.parseUnary()
		if parseErr != nil {
			return false, parseErr
		}
		left = left && right
	}
	return left, nil
}

func (parser *expressionParser) parseUnary() (bool, error) {
	if parser.current().kind == expressionOperator && parser.current().value == "!" {
		parser.advance()
		value, err := parser.parseUnary()
		return !value, err
	}
	if parser.current().kind == expressionLParen {
		parser.advance()
		value, err := parser.parseOr()
		if err != nil {
			return false, err
		}
		if parser.current().kind != expressionRParen {
			return false, fmt.Errorf("falta cerrar paréntesis")
		}
		parser.advance()
		return value, nil
	}
	return parser.parsePredicate()
}

func (parser *expressionParser) parsePredicate() (bool, error) {
	if token := parser.current(); token.kind == expressionWord && parser.peekKind(1) == expressionLParen {
		return parser.parseFunction()
	}
	left, err := parser.parseValue()
	if err != nil {
		return false, err
	}
	left = expressionLeftValue(left)
	token := parser.current()
	if token.kind == expressionWord && token.value == "in" {
		parser.advance()
		if parser.current().kind != expressionLParen {
			return false, fmt.Errorf("in requiere una lista entre paréntesis")
		}
		parser.advance()
		matched := false
		count := 0
		for parser.current().kind != expressionRParen {
			candidate, valueErr := parser.parseValue()
			if valueErr != nil {
				return false, valueErr
			}
			count++
			matched = matched || left.value == candidate.value
			if parser.current().kind != expressionComma {
				break
			}
			parser.advance()
		}
		if count == 0 || parser.current().kind != expressionRParen {
			return false, fmt.Errorf("lista de in inválida")
		}
		parser.advance()
		return matched, nil
	}
	if token.kind != expressionOperator || token.value == "&&" || token.value == "||" || token.value == "!" {
		return truthy(left.value), nil
	}
	operator := parser.advance().value
	right, err := parser.parseValue()
	if err != nil {
		return false, err
	}
	return compareExpressionValues(left.value, right.value, operator)
}

func (parser *expressionParser) parseFunction() (bool, error) {
	name := parser.advance().value
	parser.advance() // (
	first, err := parser.parseValue()
	if err != nil {
		return false, err
	}
	if name == "exists" || name == "empty" {
		if parser.current().kind != expressionRParen {
			return false, fmt.Errorf("%s recibe un argumento", name)
		}
		parser.advance()
		if name == "exists" {
			return first.exists && strings.TrimSpace(first.value) != "", nil
		}
		return !first.exists || strings.TrimSpace(first.value) == "", nil
	}
	first = expressionLeftValue(first)
	if parser.current().kind != expressionComma {
		return false, fmt.Errorf("%s recibe dos argumentos", name)
	}
	parser.advance()
	second, err := parser.parseValue()
	if err != nil {
		return false, err
	}
	if parser.current().kind != expressionRParen {
		return false, fmt.Errorf("falta cerrar %s", name)
	}
	parser.advance()
	switch name {
	case "contains":
		return strings.Contains(first.value, second.value), nil
	case "startsWith":
		return strings.HasPrefix(first.value, second.value), nil
	case "endsWith":
		return strings.HasSuffix(first.value, second.value), nil
	default:
		return false, fmt.Errorf("función %q no permitida", name)
	}
}

func (parser *expressionParser) parseValue() (expressionValue, error) {
	token := parser.current()
	if token.kind != expressionWord && token.kind != expressionString {
		return expressionValue{}, fmt.Errorf("se esperaba un valor en posición %d", token.pos)
	}
	parser.advance()
	if token.kind == expressionString {
		return expressionValue{raw: token.value, value: token.value, exists: true}, nil
	}
	value, exists := parser.vars[token.value]
	if !exists {
		value = token.value
	}
	return expressionValue{raw: token.value, value: value, exists: exists}, nil
}

func (parser *expressionParser) peekKind(offset int) expressionTokenKind {
	index := parser.index + offset
	if index < 0 || index >= len(parser.tokens) {
		return expressionEOF
	}
	return parser.tokens[index].kind
}

func compareExpressionValues(left, right, operator string) (bool, error) {
	switch operator {
	case "==":
		return left == right, nil
	case "!=":
		return left != right, nil
	case ">", ">=", "<", "<=":
		// Una variable ausente es «no sé», y no sé no es mayor que nada: se
		// resuelve como falso en vez de reventar.
		//
		// No es una tolerancia cosmética. `validateExpression` evalúa contra un
		// mapa vacío, así que sin esto **cualquier** comparación numérica entre
		// dos variables era imposible de publicar —los dos lados llegaban vacíos
		// y el validador la rechazaba—, y el operador solo servía entre literales,
		// que no compara nada. Un valor presente pero no numérico sigue siendo un
		// error: eso sí es un fallo de quien escribió el flujo o de los datos.
		if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
			return false, nil
		}
		leftNumber, leftErr := strconv.ParseFloat(left, 64)
		rightNumber, rightErr := strconv.ParseFloat(right, 64)
		if leftErr != nil || rightErr != nil {
			return false, fmt.Errorf("%s requiere dos números", operator)
		}
		switch operator {
		case ">":
			return leftNumber > rightNumber, nil
		case ">=":
			return leftNumber >= rightNumber, nil
		case "<":
			return leftNumber < rightNumber, nil
		default:
			return leftNumber <= rightNumber, nil
		}
	default:
		return false, fmt.Errorf("operador %q no permitido", operator)
	}
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "false", "0", "null", "nil":
		return false
	default:
		return true
	}
}

// A la izquierda, un identificador ausente representa una variable vacía. A la
// derecha se conserva como literal para admitir `estado == valido`. Esto replica
// la sintaxis histórica sin hacer que una variable inexistente sea verdadera.
func expressionLeftValue(value expressionValue) expressionValue {
	if value.exists {
		return value
	}
	normalized := strings.ToLower(strings.TrimSpace(value.raw))
	if normalized == "true" || normalized == "false" || normalized == "null" || normalized == "nil" {
		return value
	}
	if _, err := strconv.ParseFloat(normalized, 64); err == nil {
		return value
	}
	value.value = ""
	return value
}
