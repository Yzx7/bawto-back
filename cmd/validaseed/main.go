// validaseed comprueba los dos grafos de la semilla sin tocar PostgreSQL.
//
// Es un programa efímero, el camino normal de este repositorio para verificar
// algo que no tiene `psql` a mano. Responde tres preguntas que una prueba de
// «la fila existe» no responde:
//
//  1. ¿Se puede publicar? engine.Validate sobre el núcleo y sobre el injerto.
//     Un grafo sembrado que no se puede publicar no sirve para nada.
//  2. ¿Se dibuja? Toda arista que sale de un nodo con ramas lleva su
//     sourceHandle, ninguna trae un id escrito a mano, y engine.NormalizeEdgeIDs
//     —que es quien los rellena— produce ids únicos para todas.
//  3. ¿Corre? El recorrido principal con stubs, al estilo de
//     engine/waa_store_flow_test.go, comprobando además que ningún mensaje sale
//     con un {marcador} sin sustituir.
//
// Uso: go run ./cmd/validaseed
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/Yzx7/sacs-chatbots/db/defaults"
	"github.com/Yzx7/sacs-chatbots/engine"
)

var fallos int

func main() {
	nucleoRaw, err := defaults.FlujoInicial()
	if err != nil {
		fatal("FlujoInicial: %v", err)
	}
	injertadoRaw, err := defaults.Injertar(nucleoRaw)
	if err != nil {
		fatal("Injertar: %v", err)
	}

	nucleo := decodificar("núcleo", nucleoRaw)
	injertado := decodificar("injertado", injertadoRaw)

	comprobar("el núcleo valida", engine.Validate(nucleo))
	comprobar("el injerto valida", engine.Validate(injertado))

	comprobarInjertoAditivo(nucleo, injertado)
	comprobarEstructura("núcleo", nucleo, nucleoRaw)
	comprobarEstructura("injertado", injertado, injertadoRaw)
	comprobarSinRelecturaCortada(nucleo)
	comprobarInjertarNoEsIdempotente(injertadoRaw)

	recorridoConsulta(nucleo)
	recorridoSinFilaDeNegocio(nucleo)
	recorridoImagenDelNucleo(nucleo)
	recorridoCompra(injertado)

	if fallos > 0 {
		fmt.Printf("\n%d comprobación(es) fallida(s)\n", fallos)
		os.Exit(1)
	}
	fmt.Printf("\nTodo verde. Núcleo: %d nodos / %d aristas. Injertado: %d nodos / %d aristas.\n",
		len(nucleo.Nodes), len(nucleo.Edges), len(injertado.Nodes), len(injertado.Edges))
}

// ---------------------------------------------------------------- estructura

// comprobarEstructura vigila el fallo que describe §3 del CLAUDE.md: un grafo
// sin ids de arista valida y se ejecuta perfecto, pero aparece en el editor sin
// una sola conexión. Los ids no se escriben a mano —los deriva el servidor— así
// que aquí se comprueba lo contrario: que no haya ninguno escrito, y que el
// normalizador sepa rellenarlos todos sin colisión.
func comprobarEstructura(nombre string, flow *engine.Flow, raw json.RawMessage) {
	nodos := map[string]*engine.Node{}
	for i := range flow.Nodes {
		nodos[flow.Nodes[i].ID] = &flow.Nodes[i]
	}

	for _, edge := range flow.Edges {
		if edge.ID != "" {
			falla("%s: la arista %s → %s trae un id escrito a mano; lo rellena NormalizeEdgeIDs",
				nombre, edge.Source, edge.Target)
		}
		origen := nodos[edge.Source]
		if origen == nil {
			continue // el trigger; Validate ya comprobó que existe el resto
		}
		conRamas := ramificado(origen)
		if conRamas && edge.SourceHandle == "" {
			falla("%s: la arista %s → %s sale de un nodo con ramas y no dice cuál",
				nombre, edge.Source, edge.Target)
		}
		if !conRamas && edge.SourceHandle != "" {
			falla("%s: la arista %s → %s inventa la rama %q en un nodo de salida única",
				nombre, edge.Source, edge.Target, edge.SourceHandle)
		}
	}

	// Ninguna rama declarada puede quedar suelta. Validate ya lo exige, pero el
	// mensaje de aquí nombra la rama concreta en vez de fallar en el primer nodo.
	salidas := map[string]map[string]int{}
	for _, edge := range flow.Edges {
		if salidas[edge.Source] == nil {
			salidas[edge.Source] = map[string]int{}
		}
		salidas[edge.Source][edge.SourceHandle]++
	}
	for _, node := range flow.Nodes {
		var esperadas []string
		switch node.Kind {
		case "agent":
			esperadas = node.Outputs
		case "router":
			for _, caso := range node.Cases {
				esperadas = append(esperadas, caso.ID)
			}
			esperadas = append(esperadas, "default")
		case "tool":
			esperadas = []string{"ok", "error"}
		case "condition":
			esperadas = []string{"true", "false"}
		}
		for _, rama := range esperadas {
			if salidas[node.ID][rama] != 1 {
				falla("%s: la rama %q de %s tiene %d conexiones y debe tener 1",
					nombre, rama, node.ID, salidas[node.ID][rama])
			}
		}
	}

	normalizado, err := engine.NormalizeEdgeIDs(raw)
	if err != nil {
		falla("%s: NormalizeEdgeIDs: %v", nombre, err)
		return
	}
	var conIDs engine.Flow
	if err := json.Unmarshal(normalizado, &conIDs); err != nil {
		falla("%s: el flujo normalizado no se puede releer: %v", nombre, err)
		return
	}
	vistos := map[string]bool{}
	for _, edge := range conIDs.Edges {
		if edge.ID == "" {
			falla("%s: NormalizeEdgeIDs dejó sin id la arista %s → %s", nombre, edge.Source, edge.Target)
		}
		if vistos[edge.ID] {
			falla("%s: NormalizeEdgeIDs repitió el id %s", nombre, edge.ID)
		}
		vistos[edge.ID] = true
	}
	if len(conIDs.Edges) != len(flow.Edges) {
		falla("%s: la normalización cambió el número de aristas", nombre)
	}
	fmt.Printf("ok  %s: %d aristas con id derivado y único\n", nombre, len(vistos))
}

func ramificado(n *engine.Node) bool {
	switch n.Kind {
	case "agent", "router", "condition", "tool":
		return true
	case "wait":
		return len(n.Accepts) > 1
	}
	return false
}

// comprobarInjertoAditivo verifica lo que el plan promete del fragmento: que
// añade y no sustituye. Si algún día reemplazara un nodo del núcleo, dejaría de
// poder aplicarse sobre un borrador que el dueño ya editó (§4, Fase 6).
func comprobarInjertoAditivo(nucleo, injertado *engine.Flow) {
	antes := map[string]engine.Node{}
	for _, n := range nucleo.Nodes {
		antes[n.ID] = n
	}
	nuevos := 0
	for _, n := range injertado.Nodes {
		previo, existia := antes[n.ID]
		if !existia {
			nuevos++
			continue
		}
		if n.ID == "n_agente" {
			// El orquestador es el único que el injerto toca, y solo sumando.
			if len(n.Outputs) <= len(previo.Outputs) {
				falla("el injerto no añadió ninguna rama al orquestador")
			}
			if !strings.HasPrefix(n.Instruction, previo.Instruction) {
				falla("el injerto reescribió la instrucción del orquestador en vez de ampliarla")
			}
			for i, rama := range previo.Outputs {
				if n.Outputs[i] != rama {
					falla("el injerto reordenó las ramas del orquestador: %q pasó a %q", rama, n.Outputs[i])
				}
			}
			continue
		}
		if fmt.Sprint(previo) != fmt.Sprint(n) {
			falla("el injerto modificó el nodo %s del núcleo", n.ID)
		}
	}
	if nuevos == 0 {
		falla("el injerto no añadió ningún nodo")
	}
	if len(injertado.Edges) <= len(nucleo.Edges) {
		falla("el injerto no añadió aristas")
	}
	fmt.Printf("ok  el injerto es aditivo: +%d nodos, +%d aristas\n",
		nuevos, len(injertado.Edges)-len(nucleo.Edges))

	usadas := map[string]bool{}
	for _, n := range injertado.Nodes {
		if n.Kind == "tool" {
			usadas[n.ToolRef] = true
		}
		for _, t := range n.Tools {
			usadas[t.Ref] = true
		}
	}
	for _, ref := range []string{"catalog_search", "catalog_product", "order_create",
		"payment_intent_create", "payment_submit"} {
		if !usadas[ref] {
			falla("el injerto no usa %s", ref)
		}
	}
	// §4 del plan: estas nunca entran, ni con tienda.
	for _, ref := range []string{"subscription_activate", "credit_recharge_activate",
		"payment_methods_render"} {
		if usadas[ref] {
			falla("el injerto usa %s, que solo funciona en el bot de la organización comercial", ref)
		}
	}
	for _, n := range injertado.Nodes {
		if n.Kind != "tool" {
			continue
		}
		for _, prohibido := range []string{"planes_bawto", "segmentos"} {
			if n.Args["object"] == prohibido {
				falla("%s lee la tabla %s, que un cliente no tiene", n.ID, prohibido)
			}
		}
		if conexion := n.Args["connection"]; conexion != "" && conexion != "meudim" {
			falla("%s nombra la conexión %q en vez de meudim", n.ID, conexion)
		}
	}
	for _, n := range injertado.Nodes {
		for _, t := range n.Tools {
			if c := t.Config["connection"]; c != "" && c != "meudim" {
				falla("%s ofrece %s sobre la conexión %q en vez de meudim", n.ID, t.Ref, c)
			}
		}
	}
	fmt.Println("ok  el catálogo de herramientas del injerto es el acordado y nombra meudim por clave")
}

// comprobarSinRelecturaCortada fija la decisión: el núcleo relee su contexto en
// cada turno. Copiar el Router de corte de Sistemuino congelaría «Mi negocio»
// hasta que la conversación expira, justo mientras el dueño la está editando.
func comprobarSinRelecturaCortada(nucleo *engine.Flow) {
	for _, n := range nucleo.Nodes {
		if n.Kind != "router" {
			continue
		}
		for _, caso := range n.Cases {
			if strings.Contains(caso.Expression, "negocio.found") && n.ID != "n_business_router" {
				falla("%s corta la relectura del contexto; el núcleo la quiere fresca", n.ID)
			}
		}
	}
	// El único camino desde la entrada conversacional hasta el orquestador pasa
	// por las dos lecturas. Si alguien añade un atajo, esto se pone rojo.
	if !todoCaminoPasaPor(nucleo, "n_entry_router", "conversation", "n_agente", "n_read_business") {
		falla("hay un camino de la entrada al orquestador que no relee «Mi negocio»")
	}
	fmt.Println("ok  el contexto se relee en cada turno (decisión deliberada, ver injerto.go)")
}

func todoCaminoPasaPor(flow *engine.Flow, desde, rama, hasta, obligatorio string) bool {
	inicio := ""
	for _, e := range flow.Edges {
		if e.Source == desde && e.SourceHandle == rama {
			inicio = e.Target
		}
	}
	if inicio == "" {
		return false
	}
	visto := map[string]bool{}
	cola := []string{inicio}
	for len(cola) > 0 {
		id := cola[0]
		cola = cola[1:]
		if id == obligatorio || visto[id] {
			continue
		}
		if id == hasta {
			return false // llegó sin pasar por el nodo obligatorio
		}
		visto[id] = true
		for _, e := range flow.Edges {
			if e.Source == id {
				cola = append(cola, e.Target)
			}
		}
	}
	return true
}

// comprobarInjertarNoEsIdempotente documenta el contrato de la Fase 6: injertar
// dos veces sobre el mismo borrador tiene que fallar, no duplicar la rama.
func comprobarInjertarNoEsIdempotente(injertadoRaw json.RawMessage) {
	if _, err := defaults.Injertar(injertadoRaw); err == nil {
		falla("Injertar sobre un flujo ya injertado debería fallar y no lo hizo")
		return
	}
	fmt.Println("ok  injertar dos veces falla en vez de duplicar la rama comercial")
}

// ----------------------------------------------------------------- recorrido

var marcadorRe = regexp.MustCompile(`\{[A-Za-z_][A-Za-z0-9_.]*\}`)

// guion registra lo que el motor pidió durante un avance, para poder afirmar
// sobre el camino real y no solo sobre el resultado.
type guion struct {
	// ramas decide qué contesta cada agente, por id de nodo.
	ramas map[string]engine.AgentResult
	// tablas decide qué devuelve data_query, por clave de objeto.
	tablas map[string]string
	// respuestas decide qué devuelve el resto de herramientas, por toolRef.
	respuestas map[string]string

	instrucciones map[string]string
	argumentos    map[string]map[string]string
	falta         []string
}

func (g *guion) deps(contentType, id string) engine.Deps {
	g.instrucciones = map[string]string{}
	g.argumentos = map[string]map[string]string{}
	return engine.Deps{
		Context: map[string]string{
			"contact_name":  "Ana Torres",
			"contact_phone": "+51999888777",
		},
		Input: engine.InboundInput{ID: id, ContentType: contentType, EventType: "message"},
		AgentStructured: func(req engine.AgentRequest) (engine.AgentResult, error) {
			g.instrucciones[req.NodeID] = req.Instruction
			resultado, ok := g.ramas[req.NodeID]
			if !ok {
				g.falta = append(g.falta, "agente "+req.NodeID)
				return engine.AgentResult{}, fmt.Errorf("el guion no dice qué contesta %s", req.NodeID)
			}
			return resultado, nil
		},
		Tool: func(ref string, args, _ map[string]string) (string, error) {
			g.argumentos[ref] = args
			if ref == "data_query" {
				salida, ok := g.tablas[args["object"]]
				if !ok {
					g.falta = append(g.falta, "tabla "+args["object"])
					return "", fmt.Errorf("el guion no dice qué hay en %s", args["object"])
				}
				return salida, nil
			}
			salida, ok := g.respuestas[ref]
			if !ok {
				g.falta = append(g.falta, "herramienta "+ref)
				return "", fmt.Errorf("el guion no dice qué devuelve %s", ref)
			}
			return salida, nil
		},
	}
}

const negocioLleno = `{"found":true,"count":1,"first":{"recordId":"neg-1","data":{` +
	`"nombre":"Panadería La Unión","rubro":"Panadería y pastelería",` +
	`"que_ofrece":"Pan artesanal diario y tortas por encargo",` +
	`"tono":"Cercano, breve y sin tecnicismos",` +
	`"restricciones":"No prometer entregas el mismo día ni descuentos",` +
	`"horario":"Lunes a sábado de 7:00 a 20:00","automatiza":"vender"}}}`

// El caso que de verdad importa del cuestionario abandonado: la tabla existe,
// no tiene filas, y eso es `ok` con found=false, nunca la rama de error.
const negocioVacio = `{"found":false,"count":0,"first":null,"records":[]}`

const perfilLleno = `{"found":true,"count":1,"first":{"recordId":"per-1","data":{` +
	`"contexto_personal":"Cliente frecuente, compra los sábados","activo":true,"prioridad":10}}}`

const perfilVacio = `{"found":false,"count":0,"first":null,"records":[]}`

func recorridoConsulta(flow *engine.Flow) {
	g := &guion{
		tablas: map[string]string{"negocio": negocioLleno, "perfiles_contacto": perfilLleno},
		ramas: map[string]engine.AgentResult{
			"n_agente":       {Branch: "consulta", Reply: "no debería enviarse"},
			"n_especialista": {Branch: "conversar", Reply: "Hacemos pan artesanal todos los días y tortas por encargo. ¿Para qué día lo necesitas?"},
		},
	}
	res, err := engine.Advance(flow, nil, "hola, ¿qué venden?", g.deps("text", "wamid.1"))
	if err != nil {
		falla("consulta: %v", err)
		return
	}
	exige("consulta: un solo mensaje enviado", len(res.Sends) == 1, "sends=%v", res.Sends)
	exige("consulta: habla el especialista, no el orquestador",
		len(res.Sends) == 1 && strings.HasPrefix(res.Sends[0], "Hacemos pan"), "sends=%v", res.Sends)
	exige("consulta: queda esperando en n_espera",
		res.State != nil && res.State.NodeID == "n_espera", "state=%+v", res.State)
	exige("consulta: no deriva a una persona", !res.Handoff, "handoff=%v", res.Handoff)
	sinMarcadores("consulta", res.Sends)

	instruccion := g.instrucciones["n_especialista"]
	exige("consulta: el especialista recibe «Mi negocio» resuelto",
		strings.Contains(instruccion, "Panadería La Unión") &&
			strings.Contains(instruccion, "No prometer entregas el mismo día"),
		"instrucción=%q", recorte(instruccion))
	exige("consulta: el especialista recibe el contexto del contacto",
		strings.Contains(instruccion, "compra los sábados"), "instrucción=%q", recorte(instruccion))
	exige("consulta: ningún marcador llega sin sustituir al modelo",
		!strings.Contains(instruccion, "{negocio.") && !strings.Contains(instruccion, "{perfil.") &&
			!strings.Contains(instruccion, "{contexto_organizacion}"),
		"instrucción=%q", recorte(instruccion))
	exige("consulta: la lectura de «Mi negocio» filtra por activo",
		g.argumentos["data_query"] != nil, "no se llamó a data_query")
	informe(g)
}

func recorridoSinFilaDeNegocio(flow *engine.Flow) {
	g := &guion{
		tablas: map[string]string{"negocio": negocioVacio, "perfiles_contacto": perfilVacio},
		ramas: map[string]engine.AgentResult{
			"n_agente": {Branch: "aclarar", Reply: "¡Hola! ¿En qué te puedo ayudar?"},
		},
	}
	res, err := engine.Advance(flow, nil, "hola", g.deps("text", "wamid.2"))
	if err != nil {
		falla("cuestionario abandonado: %v", err)
		return
	}
	exige("cuestionario abandonado: el bot conversa igual",
		len(res.Sends) == 1 && res.State != nil && res.State.NodeID == "n_espera",
		"sends=%v state=%+v", res.Sends, res.State)
	sinMarcadores("cuestionario abandonado", res.Sends)
	instruccion := g.instrucciones["n_agente"]
	exige("cuestionario abandonado: el orquestador no recibe un {marcador} literal",
		!marcadorRe.MatchString(instruccion), "marcadores=%v", marcadorRe.FindAllString(instruccion, -1))
	exige("cuestionario abandonado: no se lee el perfil sin negocio",
		g.argumentos["data_query"]["object"] == "negocio",
		"último data_query=%v", g.argumentos["data_query"])
	informe(g)
}

func recorridoImagenDelNucleo(flow *engine.Flow) {
	g := &guion{}
	res, err := engine.Advance(flow, nil, "", g.deps("image", "wamid.3"))
	if err != nil {
		falla("imagen: %v", err)
		return
	}
	exige("imagen: avisa y deriva a una persona",
		res.Handoff && res.Done && len(res.Sends) == 1, "res=%+v", res)
	exige("imagen: no llama a ningún agente ni herramienta",
		len(g.instrucciones) == 0 && len(g.argumentos) == 0,
		"agentes=%v tools=%v", g.instrucciones, g.argumentos)
	sinMarcadores("imagen", res.Sends)
	informe(g)
}

func recorridoCompra(flow *engine.Flow) {
	g := &guion{
		tablas: map[string]string{"negocio": negocioLleno, "perfiles_contacto": perfilVacio},
		ramas: map[string]engine.AgentResult{
			"n_agente": {Branch: "productos"},
			"n_products_specialist": {
				Branch: "comprar",
				Reply:  "Perfecto, registro tu pedido.",
				// json.Number y no float64: es lo que produce parseAgentData con
				// UseNumber, y un stub con float64 no prueba el camino real (§15).
				Data: map[string]any{
					"productId": "55",
					"cantidad":  json.Number("2"),
					"correo":    "ana@example.com",
				},
			},
		},
		respuestas: map[string]string{
			"order_create":          `{"orderId":"1042","orderNumber":"MD-1042","summary":"2 x Pan de campo 1kg"}`,
			"payment_intent_create": `{"paymentId":"pi_77","amount":45.5,"currency":"PEN","message":"Yapea 45.50 al 999888777 y mándame la captura."}`,
		},
	}
	res, err := engine.Advance(flow, nil, "quiero 2 panes de campo, mi correo es ana@example.com", g.deps("text", "wamid.4"))
	if err != nil {
		falla("compra: %v", err)
		return
	}
	exige("compra: queda esperando el comprobante",
		res.State != nil && res.State.NodeID == "n_receipt_wait", "state=%+v", res.State)
	exige("compra: confirma el pedido y manda las instrucciones de pago",
		len(res.Sends) == 2 && strings.Contains(res.Sends[1], "MD-1042") &&
			strings.Contains(res.Sends[1], "Yapea 45.50"), "sends=%v", res.Sends)
	sinMarcadores("compra", res.Sends)
	pedido := g.argumentos["order_create"]
	exige("compra: la cantidad number del agente llega al pedido",
		pedido["item.1.quantity"] == "2", "item.1.quantity=%q", pedido["item.1.quantity"])
	exige("compra: el correo y el producto son los que confirmó el cliente",
		pedido["customerEmail"] == "ana@example.com" && pedido["item.1.productId"] == "55",
		"args=%v", pedido)
	exige("compra: la idempotencia cuelga del mensaje entrante",
		pedido["idempotencyKey"] == "message:wamid.4", "idempotencyKey=%q", pedido["idempotencyKey"])
	exige("compra: el pedido nombra la conexión por clave",
		pedido["connection"] == "meudim", "connection=%q", pedido["connection"])
	informe(g)

	// Segundo turno: la captura del comprobante sobre el estado que quedó.
	g2 := &guion{
		ramas: map[string]engine.AgentResult{
			"n_receipt": {
				Branch: "valid",
				Data: map[string]any{
					"provider":      "yape",
					"amount":        json.Number("45.5"),
					"currency":      "PEN",
					"occurredAt":    "2026-08-18T10:15:00-05:00",
					"operationCode": "0000123",
					"payerName":     "Ana Torres",
					"recipient":     "Panadería La Unión",
					"confidence":    json.Number("0.94"),
				},
			},
		},
		respuestas: map[string]string{"payment_submit": `{"amountMatches":true,"status":"pending_review"}`},
	}
	res2, err := engine.Advance(flow, res.State, "", g2.deps("image", "wamid.5"))
	if err != nil {
		falla("comprobante: %v", err)
		return
	}
	exige("comprobante: la conversación termina confirmada",
		res2.Done && !res2.Handoff && len(res2.Sends) == 1, "res=%+v", res2)
	exige("comprobante: el mensaje final cita importe, operación y pedido",
		len(res2.Sends) == 1 && strings.Contains(res2.Sends[0], "45.5") &&
			strings.Contains(res2.Sends[0], "0000123") && strings.Contains(res2.Sends[0], "MD-1042"),
		"sends=%v", res2.Sends)
	sinMarcadores("comprobante", res2.Sends)
	declarado := g2.argumentos["payment_submit"]
	exige("comprobante: el importe number sobrevive hasta payment_submit",
		declarado["declaredAmount"] == "45.5", "declaredAmount=%q", declarado["declaredAmount"])
	exige("comprobante: la operación conserva los ceros iniciales",
		declarado["reference"] == "0000123", "reference=%q", declarado["reference"])
	exige("comprobante: se declara contra el cobro abierto por la tienda",
		declarado["paymentId"] == "pi_77", "paymentId=%q", declarado["paymentId"])
	informe(g2)
}

// ------------------------------------------------------------------ utilería

func decodificar(nombre string, raw json.RawMessage) *engine.Flow {
	var flow engine.Flow
	if err := json.Unmarshal(raw, &flow); err != nil {
		fatal("%s: no se puede decodificar: %v", nombre, err)
	}
	return &flow
}

func sinMarcadores(caso string, sends []string) {
	for _, mensaje := range sends {
		if encontrados := marcadorRe.FindAllString(mensaje, -1); len(encontrados) > 0 {
			falla("%s: un mensaje sale con marcadores sin sustituir: %v", caso, encontrados)
		}
	}
}

func informe(g *guion) {
	if len(g.falta) == 0 {
		return
	}
	sort.Strings(g.falta)
	falla("el guion se quedó corto: %s", strings.Join(g.falta, ", "))
}

func comprobar(que string, err error) {
	if err != nil {
		falla("%s: %v", que, err)
		return
	}
	fmt.Printf("ok  %s\n", que)
}

func exige(que string, condicion bool, formato string, args ...any) {
	if !condicion {
		falla("%s — %s", que, fmt.Sprintf(formato, args...))
		return
	}
	fmt.Printf("ok  %s\n", que)
}

func falla(formato string, args ...any) {
	fallos++
	fmt.Printf("FALLA  %s\n", fmt.Sprintf(formato, args...))
}

func fatal(formato string, args ...any) {
	fmt.Printf("FATAL  %s\n", fmt.Sprintf(formato, args...))
	os.Exit(1)
}

func recorte(texto string) string {
	if len([]rune(texto)) <= 200 {
		return texto
	}
	return string([]rune(texto)[:200]) + "…"
}
