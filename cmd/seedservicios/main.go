// Crea el objeto de datos `servicios` de una organización y lo rellena con el
// catálogo de Sistemuino.
//
//	go run ./cmd/seedservicios -bot <uuid> [-apply]
//
// Sin -apply no escribe nada: dice qué haría. Es idempotente — repetirlo
// reutiliza el objeto y los campos, y salta los servicios que ya existan por
// nombre.
//
// El contenido sale **literalmente** del catálogo que hoy vive incrustado en la
// instrucción del agente (`db/flows/sistemuino-agente.json`). No se inventa
// nada: no hay precios ni plazos porque esa instrucción prohíbe expresamente
// darlos, y meterlos aquí los pondría en boca del bot.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/models"
)

const objectKey = "servicios"

type campo struct {
	key, label, tipo string
	req              bool
}

var campos = []campo{
	{"nombre", "Nombre", "text", true},
	{"descripcion", "Descripción", "text", true},
	{"incluye", "Qué incluye", "text", false},
	{"palabras_clave", "Palabras clave", "text", false},
}

// Cada entrada es un servicio del catálogo, tal como está redactado hoy en la
// instrucción del agente.
var servicios = []map[string]any{
	{
		"nombre":         "Páginas y aplicaciones web a medida",
		"descripcion":    "Desarrollo web a medida para presencia y captación.",
		"incluye":        "Landing pages, sitios corporativos y rediseños.",
		"palabras_clave": "web, página, sitio, landing, rediseño, aplicación web",
	},
	{
		"nombre":         "Proyectos IoT",
		"descripcion":    "Electrónica y software para medir y automatizar procesos físicos.",
		"incluye":        "Prototipos con sensores y microcontroladores, monitoreo remoto, automatización, paneles de control e integraciones.",
		"palabras_clave": "iot, sensores, microcontrolador, monitoreo, automatización, telemetría, acuicultura, agricultura",
	},
	{
		"nombre":         "BAWTO",
		"descripcion":    "Plataforma propia de bots y automatización por WhatsApp.",
		"incluye":        "Flujos conversacionales, derivación a asesores, datos de clientes, seguimientos y recordatorios.",
		"palabras_clave": "bot, whatsapp, chatbot, automatización, recordatorios, cobranza, atención",
	},
	{
		"nombre":         "Meudim Ecommerce",
		"descripcion":    "Tienda online, consumiendo nuestra API BaaS multitienda o encargándonos el desarrollo completo.",
		"incluye":        "API BaaS multitienda o desarrollo de la tienda.",
		"palabras_clave": "ecommerce, tienda, vender online, carrito, catálogo, api, baas",
	},
	{
		"nombre":         "Meudim Tarjetas",
		"descripcion":    "Un enlace público corto que reúne todos los medios de cobro del negocio.",
		"incluye":        "QR, Yape, Plin, CCI, cuentas bancarias y wallets.",
		"palabras_clave": "tarjeta, enlace, qr, yape, plin, cci, cobro, pagos, wallet",
	},
	{
		"nombre":         "Servicios técnicos",
		"descripcion":    "Infraestructura y acompañamiento técnico.",
		"incluye":        "Servidores, Linux, despliegues, dominios y SSL, nube, redes, revisión de arquitectura y acompañamiento.",
		"palabras_clave": "servidor, linux, despliegue, dominio, ssl, nube, redes, infraestructura, arquitectura, soporte",
	},
}

func main() {
	_ = godotenv.Load()
	botID := flag.String("bot", "", "uuid del bot cuya organización recibe el catálogo")
	apply := flag.Bool("apply", false, "escribe de verdad; sin este flag solo informa")
	flag.Parse()
	if *botID == "" {
		fail("se requiere -bot")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fail("pool: %v", err)
	}
	defer pool.Close()

	orgID := ""
	if err := pool.QueryRow(ctx, `SELECT org_id::text FROM bots WHERE id=$1::uuid`, *botID).Scan(&orgID); err != nil {
		fail("bot %s: %v", *botID, err)
	}
	fmt.Printf("organización %s · modo %s\n\n", orgID, modo(*apply))

	// 1 · objeto
	objects, err := models.ListDataObjectsByOrg(ctx, pool, orgID)
	if err != nil {
		fail("objetos: %v", err)
	}
	var object *models.DataObject
	for i := range objects {
		if objects[i].Key == objectKey {
			object = &objects[i]
		}
	}
	switch {
	case object != nil:
		fmt.Printf("objeto %s ya existe (%s)\n", objectKey, object.ID)
	case *apply:
		object, err = models.CreateDataObjectByOrg(ctx, pool, orgID, objectKey, "Servicio", "Servicios")
		if err != nil {
			fail("crear objeto: %v", err)
		}
		fmt.Printf("objeto %s creado (%s)\n", objectKey, object.ID)
	default:
		fmt.Printf("objeto %s: se crearía\n", objectKey)
		return
	}

	// 2 · campos
	for _, c := range campos {
		if !*apply {
			fmt.Printf("campo %s: se aseguraría\n", c.key)
			continue
		}
		if _, err := models.UpsertDataFieldByOrg(ctx, pool, orgID, object.ID, c.key, c.label, c.tipo, c.req); err != nil {
			fail("campo %s: %v", c.key, err)
		}
		fmt.Printf("campo %s asegurado\n", c.key)
	}

	// 3 · registros, saltando los que ya estén por nombre
	existentes := map[string]bool{}
	registros, err := models.ListDataRecordsByOrg(ctx, pool, orgID, object.ID)
	if err != nil {
		fail("registros: %v", err)
	}
	for _, r := range registros {
		var data map[string]any
		if json.Unmarshal(r.Data, &data) == nil {
			existentes[fmt.Sprint(data["nombre"])] = true
		}
	}

	creados := 0
	for _, servicio := range servicios {
		nombre := fmt.Sprint(servicio["nombre"])
		if existentes[nombre] {
			fmt.Printf("servicio %q ya existe\n", nombre)
			continue
		}
		if !*apply {
			fmt.Printf("servicio %q: se crearía\n", nombre)
			continue
		}
		payload, _ := json.Marshal(servicio)
		if _, err := models.CreateDataRecordByOrg(ctx, pool, orgID, object.ID, payload); err != nil {
			fail("crear %q: %v", nombre, err)
		}
		fmt.Printf("servicio %q creado\n", nombre)
		creados++
	}

	fmt.Printf("\n%d servicio(s) nuevo(s). El agente los consulta con la herramienta search_data sobre %q.\n",
		creados, objectKey)
}

func modo(apply bool) string {
	if apply {
		return "APLICANDO cambios"
	}
	return "simulación (sin escribir)"
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
