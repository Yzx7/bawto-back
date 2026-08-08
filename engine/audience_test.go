package engine

import (
	"encoding/json"
	"strings"
	"testing"
)

// condicionValida es el caso normal: «contactos con un registro en
// perfiles_contacto donde piloto = true».
func condicionValida() map[string]string {
	return map[string]string{
		"object":             "perfiles_contacto",
		"linkCurrentContact": "true",
		"where.1.field":      "piloto",
		"where.1.op":         "eq",
		"where.1.value":      "true",
	}
}

func TestAudienciaValidaSeAcepta(t *testing.T) {
	if err := ValidateAudience(condicionValida()); err != nil {
		t.Fatalf("condición válida rechazada: %v", err)
	}
	// Sin condiciones también es legítimo: «los que tienen perfil, sea cual sea».
	sinReglas := map[string]string{"object": "perfiles_contacto", "linkCurrentContact": "true"}
	if err := ValidateAudience(sinReglas); err != nil {
		t.Fatalf("una audiencia sin reglas es legítima y se rechazó: %v", err)
	}
}

// `linkCurrentContact` es lo único que ata la condición al contacto de este
// mensaje. Sin él la consulta pregunta «¿existe alguna fila que cumpla?», que
// responde lo mismo para todo el mundo: el flujo se abriría a la organización
// entera pareciendo restringido. Es el fallo más caro de este diseño y por eso
// se comprueba tanto la ausencia como el `false` explícito.
func TestAudienciaExigeLinkCurrentContact(t *testing.T) {
	sinLink := condicionValida()
	delete(sinLink, "linkCurrentContact")
	if err := ValidateAudience(sinLink); err == nil {
		t.Fatal("una condición sin linkCurrentContact no habla del contacto y debe rechazarse")
	}

	for _, valor := range []string{"false", "0", "FALSE"} {
		desactivado := condicionValida()
		desactivado["linkCurrentContact"] = valor
		if err := ValidateAudience(desactivado); err == nil {
			t.Errorf("linkCurrentContact=%q debe rechazarse: la condición no sería del contacto", valor)
		}
	}

	basura := condicionValida()
	basura["linkCurrentContact"] = "quizá"
	if err := ValidateAudience(basura); err == nil {
		t.Fatal("un linkCurrentContact que no es booleano debe rechazarse")
	}
}

// El predicado se evalúa en el despacho, antes de que el flujo tenga estado: no
// hay variables que interpolar. Aceptar `{algo}` dejaría el marcador literal
// dentro de la comparación, la condición no casaría jamás y el flujo quedaría
// mudo sin un solo error que lo explicara.
func TestAudienciaRechazaVariablesEnValue(t *testing.T) {
	conVariable := condicionValida()
	conVariable["where.1.value"] = "{contact_phone}"
	err := ValidateAudience(conVariable)
	if err == nil {
		t.Fatal("un value con variable debe rechazarse en una audiencia")
	}
	if !strings.Contains(err.Error(), "where.1.value") {
		t.Errorf("el error debe nombrar la clave culpable, y dijo: %v", err)
	}
}

// Objeto, campo y operador los fija el autor. Aquí importa más que dentro de un
// grafo: esta condición decide qué grafo corre, así que dejar que el mensaje del
// cliente eligiera tabla sería peor que en cualquier otro sitio.
func TestAudienciaHeredaLasReglasDeDataQuery(t *testing.T) {
	casos := map[string]map[string]string{
		"objeto variable":       {"object": "{tabla}", "linkCurrentContact": "true"},
		"objeto ausente":        {"linkCurrentContact": "true"},
		"campo variable":        {"object": "t", "linkCurrentContact": "true", "where.1.field": "{c}", "where.1.op": "eq", "where.1.value": "x"},
		"operador inventado":    {"object": "t", "linkCurrentContact": "true", "where.1.field": "c", "where.1.op": "aproxima", "where.1.value": "x"},
		"argumento inexistente": {"object": "t", "linkCurrentContact": "true", "sql": "DROP TABLE flows"},
	}
	for nombre, args := range casos {
		if err := ValidateAudience(args); err == nil {
			t.Errorf("%s: debía rechazarse", nombre)
		}
	}
}

// Un `limit: 50` guardado en una audiencia insinúa con fuerza que hace algo, y
// no hace nada: el predicado responde sí o no y se ejecuta con límite 1. Se
// rechaza en vez de ignorarse, porque un argumento aceptado y descartado en
// silencio es de donde salen las tardes buscando por qué «el filtro no se aplica».
func TestAudienciaRechazaArgumentosSinSentidoEnUnPredicado(t *testing.T) {
	for _, key := range []string{"fields", "orderBy", "orderDir", "limit"} {
		args := condicionValida()
		args[key] = "algo"
		err := ValidateAudience(args)
		if err == nil {
			t.Errorf("%s no significa nada en un predicado y debe rechazarse", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: el error debe nombrar el argumento, y dijo: %v", key, err)
		}
	}
}

// Vacío, `null` y `{}` son «sin audiencia»: el flujo atiende a todos, que es el
// comportamiento de siempre. Es el camino que recorre cada flujo no restringido
// del sistema, así que tiene que ser barato y no fallar.
func TestParseAudienceVacioEsSinRestriccion(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", "  "} {
		cond, err := ParseAudience(json.RawMessage(raw))
		if err != nil {
			t.Errorf("%q: no debía fallar: %v", raw, err)
		}
		if cond != nil {
			t.Errorf("%q: debía ser sin restricción y devolvió %v", raw, cond)
		}
	}
}

func TestParseAudienceRechazaValoresNoTexto(t *testing.T) {
	// La condición es un mapa plano de texto a texto, igual que los args de un
	// bloque. Un número o un objeto anidado no es un descuido de tipos: es que
	// alguien está mandando otra forma.
	if _, err := ParseAudience(json.RawMessage(`{"object":"t","limit":5}`)); err == nil {
		t.Fatal("un valor no textual debe rechazarse")
	}
	if _, err := ParseAudience(json.RawMessage(`[]`)); err == nil {
		t.Fatal("una lista no es una condición")
	}
}

func TestParseAudienceValidaLoQueGuarda(t *testing.T) {
	// ParseAudience es la puerta única: lo que entra por el endpoint y lo que se
	// lee de la columna pasan por aquí. Si aceptara sin validar, una condición
	// rota guardada antes de un endurecimiento se ejecutaría igual.
	raw, err := json.Marshal(condicionValida())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cond, err := ParseAudience(raw)
	if err != nil {
		t.Fatalf("condición válida rechazada: %v", err)
	}
	if cond["object"] != "perfiles_contacto" {
		t.Fatalf("condición mal leída: %v", cond)
	}
	if _, err := ParseAudience(json.RawMessage(`{"object":"t"}`)); err == nil {
		t.Fatal("sin linkCurrentContact no debe poder guardarse ni leerse como válida")
	}
}

// El resumen es lo que ven el panel y el log. Un flujo restringido que parezca
// general es cómo alguien concluye que «el bot no responde», así que la frase
// tiene que nombrar la tabla y la regla.
func TestAudienceSummaryExplicaLaCondicion(t *testing.T) {
	got := AudienceCondition(condicionValida()).Summary()
	for _, esperado := range []string{"perfiles_contacto", "piloto", "true"} {
		if !strings.Contains(got, esperado) {
			t.Errorf("el resumen debe mencionar %q, y fue: %q", esperado, got)
		}
	}
	if AudienceCondition(nil).Summary() != "" {
		t.Error("sin audiencia no hay resumen que mostrar")
	}
}
