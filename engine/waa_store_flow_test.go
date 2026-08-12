package engine

import (
	"encoding/json"
	"os"
	"testing"
)

// Este fixture es el borrador vivo de flow_waa_tienda. La copia empezó a
// recibir ediciones propias en el panel, así que reutilizar el fixture de
// Sistemuino habría vuelto a probar posiciones y configuración ajenas.
func loadWAAStoreFlow(t *testing.T) *Flow {
	t.Helper()
	raw, err := os.ReadFile("../db/flows/waa-tienda.json")
	if err != nil {
		t.Fatal(err)
	}
	var flow Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		t.Fatal(err)
	}
	return &flow
}

func TestWAATiendaDeclaraElComprobanteCompletoEnMeudim(t *testing.T) {
	flow := loadWAAStoreFlow(t)
	if err := Validate(flow); err != nil {
		t.Fatalf("flow_waa_tienda inválido: %v", err)
	}

	nodes := map[string]*Node{}
	for i := range flow.Nodes {
		nodes[flow.Nodes[i].ID] = &flow.Nodes[i]
	}
	receipt := nodes["n_store_receipt"]
	submit := nodes["n_store_payment_submit"]
	amountOK := nodes["n_store_amount_ok"]
	if receipt == nil || submit == nil || amountOK == nil {
		t.Fatalf("faltan nodos del comprobante: receipt=%v submit=%v amountOK=%v", receipt, submit, amountOK)
	}

	fields := map[string]bool{}
	for _, field := range receipt.OutputFields {
		fields[field.Key] = true
	}
	for _, key := range []string{"provider", "amount", "currency", "occurredAt", "operationCode", "payerName", "recipient", "confidence"} {
		if !fields[key] {
			t.Errorf("n_store_receipt no declara %s", key)
		}
	}
	for key, want := range map[string]string{
		"reference":      "{receipt_tienda.operationCode}",
		"declaredAmount": "{receipt_tienda.amount}",
		"declaredAt":     "{receipt_tienda.occurredAt}",
		"channel":        "{receipt_tienda.provider}",
		"payerName":      "{receipt_tienda.payerName}",
		"recipient":      "{receipt_tienda.recipient}",
	} {
		if submit.Args[key] != want {
			t.Errorf("n_store_payment_submit.%s = %q, esperado %q", key, submit.Args[key], want)
		}
	}
	if amountOK.Expression != "empty(declaracion.amountMatches) || declaracion.amountMatches == true" {
		t.Fatalf("la comparación no usa el resultado de Meudim: %q", amountOK.Expression)
	}

	wantEdges := map[string]string{
		"e_prod_14": "n_store_receipt:valid>n_store_payment_submit",
		"e_prod_15": "n_store_receipt:needs_review>n_store_payment_submit",
		"e_prod_18": "n_store_payment_submit:ok>n_store_amount_ok",
		"e_prod_22": "n_store_amount_ok:true>n_store_payment_done",
		"e_prod_23": "n_store_amount_ok:false>n_store_amount_mismatch",
	}
	for _, edge := range flow.Edges {
		if want, ok := wantEdges[edge.ID]; ok {
			got := edge.Source + ":" + edge.SourceHandle + ">" + edge.Target
			if got != want {
				t.Errorf("%s = %s, esperado %s", edge.ID, got, want)
			}
			delete(wantEdges, edge.ID)
		}
	}
	for id := range wantEdges {
		t.Errorf("falta la arista %s", id)
	}
}
