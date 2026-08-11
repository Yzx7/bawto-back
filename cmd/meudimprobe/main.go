// Command meudimprobe da de alta y verifica una conexión a Meudim.
//
// Existe porque el panel de conexiones todavía no está (fase D del plan) y
// porque, incluso cuando esté, hace falta poder comprobar una credencial sin
// pasar por una conversación real: un flujo que falla no distingue una clave
// revocada de una tienda caída, y aquí sí se ve.
//
// Solo lee. No crea pedidos ni pagos: con una clave `pk_live_` eso movería stock
// y dinero de una tienda de verdad.
//
//	go run ./cmd/meudimprobe -org-id <uuid> -save -search "ESP32"
//
// La credencial se toma del entorno (MEUDIM_API_KEY o, si no está,
// NEXT_PUBLIC_MEUD_PK) y **nunca se imprime**: solo su forma enmascarada.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/connectors"
	"github.com/Yzx7/sacs-chatbots/connectors/meudim"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
)

func main() {
	_ = godotenv.Load()
	orgID := flag.String("org-id", "", "organización dueña de la conexión")
	key := flag.String("key", "meudim", "clave técnica que referenciarán los bloques del flujo")
	label := flag.String("label", "", "nombre visible en el panel (por defecto, la clave)")
	baseURL := flag.String("base-url", connectors.DefaultBaseURL(connectors.DriverMeudim), "URL base de la API")
	save := flag.Bool("save", false, "crea o actualiza la conexión con la credencial del entorno")
	search := flag.String("search", "", "búsqueda de prueba en el catálogo")
	slug := flag.String("product-slug", "", "detalle de un producto por slug")
	productID := flag.Int64("product-id", 0, "detalle de un producto por id")
	flag.Parse()

	if *orgID == "" {
		fail("-org-id es obligatorio")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		fail("DATABASE_URL no está definida")
	}
	cipher, err := helpers.NewCipher(os.Getenv("TOKEN_ENC_KEY"))
	if err != nil {
		fail("TOKEN_ENC_KEY: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		fail("PostgreSQL: %v", err)
	}
	defer pool.Close()

	if *save {
		saveConnection(ctx, pool, cipher, *orgID, *key, *label, *baseURL)
	}

	connection, err := models.ExternalConnectionByKey(ctx, pool, *orgID, *key)
	if err != nil {
		fail("consultar la conexión: %v", err)
	}
	if connection == nil {
		fail("la organización no tiene una conexión %q; usa -save para crearla", *key)
	}
	credential, err := cipher.Decrypt(connection.CredentialEnc)
	if err != nil {
		fail("descifrar la credencial: %v", err)
	}

	fmt.Printf("Conexión   %s (%s)\n", connection.Key, connection.Driver)
	fmt.Printf("Destino    %s\n", connection.BaseURL)
	fmt.Printf("Credencial %s\n", models.MaskCredential(credential))
	fmt.Printf("Estado     %s\n", connection.Status)
	if !connection.Active() {
		fmt.Println("AVISO: la conexión está deshabilitada; los bloques del flujo saldrán por error.")
	}

	client, err := meudim.New(connection.BaseURL, credential, nil)
	if err != nil {
		fail("cliente: %v", err)
	}

	store, meta, err := client.Store(ctx)
	// El resultado se anota siempre, acierte o falle: es lo que después explica
	// en el panel por qué un flujo dejó de encontrar productos.
	if recErr := models.RecordExternalConnectionResult(ctx, pool, connection.ID, err); recErr != nil {
		fmt.Fprintf(os.Stderr, "aviso: no se pudo anotar el resultado: %v\n", recErr)
	}
	if err != nil {
		fail("consultar la tienda: %s", explain(err))
	}
	fmt.Printf("\nTienda     %s (id %d)\n", store.Name, store.ID)
	if store.Domain != "" {
		fmt.Printf("Dominio    %s\n", store.Domain)
	}
	fmt.Printf("Moneda     %s %s\n", store.Settings.Currency, store.Settings.CurrencySymbol)
	if store.CanCharge() {
		fmt.Printf("Cobro      %d medio(s) configurado(s)\n", len(store.Settings.Payments.Manual.Accounts))
	} else {
		fmt.Println("Cobro      SIN CONFIGURAR: la venta llegará al pedido y se detendrá al cobrar")
	}
	fmt.Printf("Cuota      %s peticiones restantes este minuto\n", remaining(meta))

	if *search != "" {
		products, meta, err := client.SearchProducts(ctx, meudim.ProductQuery{Search: *search, Limit: 5})
		if err != nil {
			fail("buscar %q: %s", *search, explain(err))
		}
		fmt.Printf("\nBúsqueda %q — %d de %d resultados (cuota %s)\n",
			*search, len(products), meta.Total, remaining(meta))
		if len(products) == 0 {
			// Cero coincidencias es una respuesta, no un fallo. Se imprime así para
			// que la diferencia quede clara también aquí.
			fmt.Println("  (sin coincidencias; la tienda respondió correctamente)")
		}
		for _, product := range products {
			fmt.Printf("  · [%d] %-40s %8.2f  %s\n",
				product.ID, truncate(product.Name, 40), product.Price, availability(product))
		}
	}

	if *slug != "" || *productID > 0 {
		var product *meudim.Product
		var meta meudim.Response
		var err error
		if *slug != "" {
			product, meta, err = client.ProductBySlug(ctx, *slug)
		} else {
			product, meta, err = client.ProductByID(ctx, *productID)
		}
		if err != nil {
			fail("detalle del producto: %s", explain(err))
		}
		fmt.Printf("\nProducto   %s (id %d, %s)\n", product.Name, product.ID, availability(*product))
		fmt.Printf("Slug       %s\n", product.Slug)
		fmt.Printf("Precio     %.2f\n", product.Price)
		fmt.Printf("Imágenes   %d\n", len(product.Images))
		if len(product.Specifications) > 0 {
			fmt.Printf("Especific. %s\n", truncate(string(product.Specifications), 200))
		}
		fmt.Printf("Cuota      %s\n", remaining(meta))
	}
}

func saveConnection(ctx context.Context, pool *pgxpool.Pool, cipher *helpers.Cipher, orgID, key, label, baseURL string) {
	credential := strings.TrimSpace(os.Getenv("MEUDIM_API_KEY"))
	source := "MEUDIM_API_KEY"
	if credential == "" {
		credential, source = strings.TrimSpace(os.Getenv("NEXT_PUBLIC_MEUD_PK")), "NEXT_PUBLIC_MEUD_PK"
	}
	if credential == "" {
		fail("define MEUDIM_API_KEY (o NEXT_PUBLIC_MEUD_PK) con la clave publicable de la tienda")
	}
	// El bot solo necesita leer catálogo y cerrar la compra, y eso lo cubre una
	// clave publicable. Una `sk_` da acceso total a la tienda —confirmar pagos,
	// borrar productos— y no tiene por qué estar al alcance de un flujo.
	if !strings.HasPrefix(credential, "pk_") {
		fail("la credencial de %s no es una clave publicable (pk_); el bot no debe usar una sk_", source)
	}
	if err := connectors.ValidateTarget(connectors.DriverMeudim, baseURL); err != nil {
		fail("destino no permitido: %v", err)
	}
	encrypted, err := cipher.Encrypt(credential)
	if err != nil {
		fail("cifrar la credencial: %v", err)
	}
	if _, err := models.SaveExternalConnection(ctx, pool, models.ExternalConnectionInput{
		OrgID: orgID, Key: key, Driver: connectors.DriverMeudim, Label: label,
		BaseURL: baseURL, CredentialEnc: encrypted, Status: "active",
	}); err != nil {
		fail("guardar la conexión: %v", err)
	}
	fmt.Printf("Conexión %q guardada con la credencial de %s.\n\n", key, source)
}

// explain traduce el fallo a la causa que hay que atender, que no siempre es
// evidente en el texto del error.
func explain(err error) string {
	var apiErr *meudim.Error
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	switch {
	case apiErr.Unauthorized():
		return "la clave es inválida, fue revocada o es de otra tienda (" + apiErr.Message + ")"
	case apiErr.RateLimited():
		return fmt.Sprintf("cuota agotada; reintentar en %s", apiErr.RetryAfter)
	case apiErr.Unreachable:
		return "no hubo respuesta de la API: " + apiErr.Message
	default:
		return apiErr.Error()
	}
}

func remaining(meta meudim.Response) string {
	if meta.RateLimitRemaining < 0 {
		return "desconocida"
	}
	return fmt.Sprint(meta.RateLimitRemaining)
}

func availability(product meudim.Product) string {
	if !product.Available() {
		return "agotado"
	}
	if !product.TrackInventory {
		return "disponible"
	}
	return fmt.Sprintf("stock %d", product.StockQuantity)
}

func truncate(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-1]) + "…"
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
