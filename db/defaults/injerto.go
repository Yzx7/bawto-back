package defaults

// Aquí vive el grafo del primer flujo de un bot. Son dos ficheros y no dos
// variantes completas: el núcleo tiene un solo dueño y no puede divergir, así
// que lo comercial se añade encima en vez de duplicarlo (§4 del plan).
//
// # La relectura del contexto es deliberada
//
// Sistemuino evita releer su contexto con un Router («CTX obtained») que corta
// la consulta cuando las variables ya están en el estado. Ahorra dos lecturas
// por turno y, a cambio, **congela** el contexto hasta que la conversación
// expira a las 24 h.
//
// El núcleo **no copia ese patrón** a propósito: el dueño de un bot recién
// creado está editando «Mi negocio» mientras prueba el bot con su propio
// teléfono, y con el Router de corte su cambio no se notaría hasta el día
// siguiente —parecería que el panel no guarda—. Dos lecturas locales por turno
// son ruido al lado de la llamada al modelo que viene después. Quien quiera el
// ahorro puede añadir ese Router más tarde, cuando su contexto ya no cambie a
// diario; al revés, descubrir el congelamiento el primer día cuesta la
// confianza en el producto.

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
)

//go:embed flujo-inicial.json flujo-comercial.json
var flujosFS embed.FS

// FlujoInicial devuelve el núcleo conversacional: el grafo que recibe todo bot
// recién creado. Funciona sin ninguna conexión externa, solo con las tablas que
// siembra Objetos, y emite únicamente texto porque engine.Result.Sends es
// []string.
func FlujoInicial() (json.RawMessage, error) {
	return leerFlujo("flujo-inicial.json")
}

// FlujoComercial devuelve el fragmento crudo, tal como está en el fichero. No
// es un flujo completo y no valida por sí solo: sus aristas apuntan a nodos del
// núcleo (n_espera, n_derivar, n_fin) y extiende un agente que vive allí.
func FlujoComercial() (json.RawMessage, error) {
	return leerFlujo("flujo-comercial.json")
}

func leerFlujo(nombre string) (json.RawMessage, error) {
	raw, err := flujosFS.ReadFile(nombre)
	if err != nil {
		return nil, fmt.Errorf("no se pudo leer %s: %w", nombre, err)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("%s no es JSON válido", nombre)
	}
	// Copia: el llamador no debe poder mutar los bytes embebidos del proceso.
	return append(json.RawMessage(nil), raw...), nil
}

// Injertar devuelve el núcleo con el fragmento comercial dentro: catálogo,
// pedido y cobro. Se usa cuando la organización quiere vender **y** tiene una
// conexión `meudim` activa.
//
// Es puramente aditivo: añade nodos, añade aristas y añade ramas al
// orquestador. No reescribe ni borra nada del núcleo, y por eso el núcleo puede
// seguir evolucionando sin que el fragmento tenga que enterarse.
//
// Trabaja sobre el documento JSON genérico y no sobre engine.Flow, por la misma
// razón que engine.Canonical y engine.NormalizeEdgeIDs: así sobreviven `pos` y
// cualquier clave que este backend todavía no conozca.
//
// Los `edges[].id` no se escriben aquí. El id es derivable de
// (source, sourceHandle) y lo rellena el servidor con engine.NormalizeEdgeIDs
// al guardar el borrador; inventarlo aquí solo abriría la puerta a un duplicado.
func Injertar(nucleo json.RawMessage) (json.RawMessage, error) {
	raiz, err := decodificarDocumento(nucleo, "el núcleo")
	if err != nil {
		return nil, err
	}
	crudo, err := FlujoComercial()
	if err != nil {
		return nil, err
	}
	fragmento, err := decodificarDocumento(crudo, "el fragmento comercial")
	if err != nil {
		return nil, err
	}

	nodos, err := lista(raiz, "nodes", "el núcleo")
	if err != nil {
		return nil, err
	}
	nodosFragmento, err := lista(fragmento, "nodes", "el fragmento comercial")
	if err != nil {
		return nil, err
	}
	aristas, err := lista(raiz, "edges", "el núcleo")
	if err != nil {
		return nil, err
	}
	aristasFragmento, err := lista(fragmento, "edges", "el fragmento comercial")
	if err != nil {
		return nil, err
	}

	porID := map[string]map[string]any{}
	for _, item := range nodos {
		nodo, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("el núcleo tiene un nodo que no es un objeto")
		}
		id, _ := nodo["id"].(string)
		porID[id] = nodo
	}
	for _, item := range nodosFragmento {
		nodo, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("el fragmento comercial tiene un nodo que no es un objeto")
		}
		id, _ := nodo["id"].(string)
		// Un id repetido significaría que el fragmento sustituye un nodo del
		// núcleo. El injerto solo añade: si algún día hace falta reemplazar, que
		// se decida a propósito y no por una colisión de nombres.
		if _, existe := porID[id]; existe {
			return nil, fmt.Errorf("el fragmento comercial repite el nodo %q del núcleo", id)
		}
		porID[id] = nodo
	}

	if err := extenderAgente(fragmento, porID); err != nil {
		return nil, err
	}

	raiz["nodes"] = append(nodos, nodosFragmento...)
	raiz["edges"] = append(aristas, aristasFragmento...)

	salida, err := json.Marshal(raiz)
	if err != nil {
		return nil, fmt.Errorf("no se pudo serializar el flujo injertado: %w", err)
	}
	return salida, nil
}

// extenderAgente aplica el bloque `extiendeAgente` del fragmento: añade ramas al
// orquestador del núcleo y le explica cuándo usarlas.
//
// Las ramas son la decisión de control del agente y las exige engine.Validate,
// así que añadir la arista sin añadir la salida produciría un grafo que el
// editor dibuja y el backend rechaza.
func extenderAgente(fragmento map[string]any, porID map[string]map[string]any) error {
	bruto, existe := fragmento["extiendeAgente"]
	if !existe {
		return nil
	}
	extension, ok := bruto.(map[string]any)
	if !ok {
		return fmt.Errorf("extiendeAgente debe ser un objeto")
	}
	id, _ := extension["id"].(string)
	agente := porID[id]
	if agente == nil {
		return fmt.Errorf("el fragmento comercial extiende el agente %q, que el núcleo no tiene", id)
	}
	if kind, _ := agente["kind"].(string); kind != "agent" {
		return fmt.Errorf("el nodo %q no es un agente", id)
	}

	if nuevas, ok := extension["outputs"].([]any); ok && len(nuevas) > 0 {
		salidas, _ := agente["outputs"].([]any)
		vistas := map[string]struct{}{}
		for _, salida := range salidas {
			if nombre, ok := salida.(string); ok {
				vistas[nombre] = struct{}{}
			}
		}
		for _, salida := range nuevas {
			nombre, ok := salida.(string)
			if !ok {
				return fmt.Errorf("extiendeAgente.outputs debe contener textos")
			}
			if _, repetida := vistas[nombre]; repetida {
				return fmt.Errorf("el agente %q ya declara la rama %q", id, nombre)
			}
			vistas[nombre] = struct{}{}
			salidas = append(salidas, nombre)
		}
		agente["outputs"] = salidas
	}

	if extra, _ := extension["instruction"].(string); extra != "" {
		instruccion, _ := agente["instruction"].(string)
		agente["instruction"] = instruccion + extra
	}
	return nil
}

// decodificarDocumento usa UseNumber para que un número del documento vuelva a
// serializarse tal cual. Sin él, una posición como 2460 saldría como 2460.0 y el
// checksum canónico del borrador cambiaría sin que nadie hubiera tocado el grafo.
func decodificarDocumento(raw json.RawMessage, quien string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s no es JSON válido: %w", quien, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%s trae contenido después del documento JSON", quien)
	}
	raiz, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s no es un objeto JSON", quien)
	}
	return raiz, nil
}

func lista(doc map[string]any, clave, quien string) ([]any, error) {
	bruto, existe := doc[clave]
	if !existe || bruto == nil {
		return nil, nil
	}
	items, ok := bruto.([]any)
	if !ok {
		return nil, fmt.Errorf("%s tiene un %q que no es una lista", quien, clave)
	}
	return items, nil
}
