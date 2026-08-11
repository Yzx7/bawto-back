package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Yzx7/sacs-chatbots/connectors"
	"github.com/Yzx7/sacs-chatbots/connectors/meudim"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Ejecutores del catálogo externo.
//
// A diferencia de data_query, estos salen a la red **dentro del turno**, con el
// advisory lock del chat tomado. Eso obliga a tres cosas que no existen en una
// lectura local y que están aquí y no en el cliente HTTP, porque son propiedades
// de la conversación y no del transporte:
//
//   - un presupuesto de llamadas por turno, para que un bucle agéntico mal
//     instruido no se coma la cuota del minuto de la tienda;
//   - una caché muy corta, para no repetir la misma pregunta dos veces en el
//     mismo turno;
//   - un fallo que se cuenta en vez de disimularse: sin copia local no hay red
//     de seguridad, y lo único peor que decir «no pude consultar» es inventar un
//     precio.

const (
	// maxCatalogCallsPerTurn acota las llamadas externas de un mensaje. Cuatro
	// alcanzan para buscar, mirar un detalle y corregir una búsqueda; más que eso
	// ya no es una conversación, es un bucle.
	maxCatalogCallsPerTurn = 4
	// catalogCacheTTL cubre el turno y los segundos siguientes. No es sincronizar
	// el catálogo: es no preguntar dos veces lo mismo mientras se responde.
	catalogCacheTTL = 30 * time.Second
	// storeCacheTTL es más largo porque la moneda de una tienda no cambia en una
	// tarde, y pagar una llamada por turno solo para saber que sigue en PEN sería
	// gastar cuota en un dato estático.
	storeCacheTTL = 10 * time.Minute
	// maxAgentCatalogResults es el tope por defecto de productos que ve el modelo.
	// Cinco es lo que se puede leer en un mensaje de WhatsApp sin marear.
	maxAgentCatalogResults = 5
)

// catalogBudget es el presupuesto de un turno. Un acierto de caché no consume:
// no costó una petición, así que cobrarla castigaría justo el camino barato.
type catalogBudget struct {
	remaining int
}

func newCatalogBudget() *catalogBudget { return &catalogBudget{remaining: maxCatalogCallsPerTurn} }

var errCatalogBudget = errors.New("presupuesto de consultas al catálogo agotado en este turno")

func (b *catalogBudget) consume() error {
	if b == nil {
		return nil
	}
	if b.remaining <= 0 {
		return errCatalogBudget
	}
	b.remaining--
	return nil
}

// catalogCache es de proceso, no compartida. La PC y el servidor tienen la suya
// y eso está bien: cada una responde sus propios turnos, y una entrada de treinta
// segundos no es un estado que valga la pena coordinar.
type responseCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	value     any
	expiresAt time.Time
}

var catalogCache = &responseCache{entries: map[string]cacheEntry{}}

func (c *responseCache) get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok || time.Now().After(entry.expiresAt) {
		return nil, false
	}
	return entry.value, true
}

func (c *responseCache) put(key string, value any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// La caché se poda al escribir: sin esto, una organización con muchas
	// búsquedas distintas la haría crecer sin techo durante toda la vida del
	// proceso.
	if len(c.entries) > 512 {
		now := time.Now()
		for existing, entry := range c.entries {
			if now.After(entry.expiresAt) {
				delete(c.entries, existing)
			}
		}
	}
	c.entries[key] = cacheEntry{value: value, expiresAt: time.Now().Add(ttl)}
}

// resetCatalogCache existe para las pruebas: sin ella, una prueba vería el
// resultado que dejó otra.
func resetCatalogCache() {
	catalogCache.mu.Lock()
	defer catalogCache.mu.Unlock()
	catalogCache.entries = map[string]cacheEntry{}
}

// catalogConnection resuelve la tienda de la organización del bot.
//
// La clave la fija el bloque; la organización sale del bot. Ni el modelo ni el
// mensaje del cliente participan en decidir a quién se llama.
func (con *Controller) catalogConnection(ctx context.Context, bot *models.BotChannel, key string) (*meudim.Client, *models.ExternalConnection, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil, fmt.Errorf("el bloque no declara conexión")
	}
	connection, err := models.ExternalConnectionByKey(ctx, con.Env.Postgres, bot.OrgID, key)
	if err != nil {
		return nil, nil, err
	}
	if connection == nil {
		return nil, nil, fmt.Errorf("la organización no tiene una conexión %q", key)
	}
	if connection.Driver != connectors.DriverMeudim {
		return nil, nil, fmt.Errorf("la conexión %q usa el driver %q, que no consulta catálogos", key, connection.Driver)
	}
	if !connection.Active() {
		return nil, nil, fmt.Errorf("la conexión %q está deshabilitada", key)
	}
	if con.Env.Cipher == nil {
		return nil, nil, fmt.Errorf("no hay clave de cifrado configurada (TOKEN_ENC_KEY)")
	}
	credential, err := con.Env.Cipher.Decrypt(connection.CredentialEnc)
	if err != nil {
		return nil, nil, fmt.Errorf("no se pudo descifrar la credencial de %q", key)
	}
	client, err := meudim.New(connection.BaseURL, credential, nil)
	if err != nil {
		return nil, nil, err
	}
	return client, connection, nil
}

// recordCatalogCall anota el resultado sin escribir en cada turno.
//
// Un fallo siempre se guarda; un acierto solo cuando venía de un fallo, para
// limpiarlo. Registrar cada acierto sería un UPDATE por mensaje a cambio de un
// dato que ya se conoce.
func (con *Controller) recordCatalogCall(ctx context.Context, connection *models.ExternalConnection, callErr error) {
	if connection == nil {
		return
	}
	if callErr == nil && connection.LastError == nil {
		return
	}
	if err := models.RecordExternalConnectionResult(ctx, con.Env.Postgres, connection.ID, callErr); err != nil {
		con.whatsAppLogger().Warn("catálogo: no se pudo anotar el resultado",
			"connection", connection.Key, "err", err.Error())
	}
}

// catalogSearchParams es lo que acaba consultándose, ya fusionado lo que fijó el
// autor con lo que pidió el modelo o el grafo.
type catalogSearchParams struct {
	Connection         string
	Query              string
	CategoryID         int
	IncludeDescendants bool
	MinPrice           float64
	MaxPrice           float64
	Sort               string
	Limit              int
	URLTemplate        string
}

// catalogRecord espeja models.DataQueryRecord a propósito. Un Router escrito
// para una lectura de tabla se lee igual sobre el catálogo: `found`, `count`,
// `first.data.<campo>`. Que las dos herramientas tengan la misma forma es lo que
// permite cambiar de fuente sin reescribir el grafo.
type catalogRecord struct {
	RecordID string         `json:"recordId"`
	Data     map[string]any `json:"data"`
}

type catalogResult struct {
	Found   bool            `json:"found"`
	Count   int             `json:"count"`
	First   *catalogRecord  `json:"first"`
	Records []catalogRecord `json:"records"`
}

// searchCatalog es el camino común del grafo y del agente.
func (con *Controller) searchCatalog(ctx context.Context, bot *models.BotChannel, budget *catalogBudget,
	params catalogSearchParams) (catalogResult, error) {
	client, connection, err := con.catalogConnection(ctx, bot, params.Connection)
	if err != nil {
		return catalogResult{}, err
	}
	limit := params.Limit
	if limit <= 0 {
		limit = maxAgentCatalogResults
	}
	query := meudim.ProductQuery{
		Search: params.Query, CategoryID: int64(params.CategoryID),
		IncludeDescendants: params.IncludeDescendants,
		MinPrice:           params.MinPrice, MaxPrice: params.MaxPrice,
		Sort: params.Sort, Limit: limit,
	}
	cacheKey := fmt.Sprintf("search|%s|%s|%+v", bot.OrgID, connection.Key, query)
	products, cached := cachedProducts(cacheKey)
	if !cached {
		if err := budget.consume(); err != nil {
			return catalogResult{}, err
		}
		var meta meudim.Response
		products, meta, err = client.SearchProducts(ctx, query)
		con.recordCatalogCall(ctx, connection, err)
		if err != nil {
			return catalogResult{}, err
		}
		con.logCatalogCall("search", connection.Key, meta)
		catalogCache.put(cacheKey, products, catalogCacheTTL)
	}

	currency := con.storeCurrency(ctx, client, connection)
	result := catalogResult{Count: len(products), Records: make([]catalogRecord, 0, len(products))}
	for _, product := range products {
		result.Records = append(result.Records, catalogRecord{
			RecordID: strconv.FormatInt(product.ID, 10),
			Data:     productData(product, currency, params.URLTemplate),
		})
	}
	result.Found = len(result.Records) > 0
	if result.Found {
		first := result.Records[0]
		result.First = &first
	}
	return result, nil
}

func cachedProducts(key string) ([]meudim.Product, bool) {
	value, ok := catalogCache.get(key)
	if !ok {
		return nil, false
	}
	products, ok := value.([]meudim.Product)
	return products, ok
}

// storeCurrency resuelve la moneda con la que hay que hablarle al cliente.
//
// Un precio sin moneda en una conversación de venta no es un detalle estético:
// «cuesta 19.90» no dice si son soles o dólares. Si la tienda no responde a esta
// consulta, se devuelve vacío en vez de tumbar la búsqueda: el producto y su
// precio ya son correctos.
//
// **No consume presupuesto.** El presupuesto acota lo que el modelo puede pedir,
// y esta llamada no la pide él: sale como mucho una vez por búsqueda y se cachea
// diez minutos. Cobrársela le dejaba tres consultas de las cuatro y, peor, hacía
// que los precios perdieran la moneda justo en el turno donde ya iba justo.
func (con *Controller) storeCurrency(ctx context.Context, client *meudim.Client,
	connection *models.ExternalConnection) string {
	cacheKey := "store|" + connection.ID
	if value, ok := catalogCache.get(cacheKey); ok {
		if currency, ok := value.(string); ok {
			return currency
		}
	}
	store, _, err := client.Store(ctx)
	if err != nil {
		con.whatsAppLogger().Warn("catálogo: no se pudo leer la tienda",
			"connection", connection.Key, "err", err.Error())
		return ""
	}
	currency := strings.TrimSpace(store.Settings.Currency)
	catalogCache.put(cacheKey, currency, storeCacheTTL)
	return currency
}

// productData proyecta el producto a las claves que verá el flujo. Se traduce a
// español porque son las que el autor escribe en un mensaje —{producto.first.data.precio}—
// y porque el resto de objetos de datos ya están en español.
func productData(product meudim.Product, currency, urlTemplate string) map[string]any {
	data := map[string]any{
		"id":          product.ID,
		"nombre":      product.Name,
		"slug":        product.Slug,
		"precio":      product.Price,
		"stock":       product.StockQuantity,
		"disponible":  product.Available(),
		"descripcion": firstNonEmptyText(product.ShortDescription, product.Description),
		"imagen":      product.MainImageURL,
	}
	if currency != "" {
		data["moneda"] = currency
	}
	if product.SKU != "" {
		data["sku"] = product.SKU
	}
	if product.CategoryName != "" {
		data["categoria"] = product.CategoryName
	}
	if url := productURL(urlTemplate, product); url != "" {
		data["url"] = url
	}
	return data
}

// productURL compone el enlace público. La API no conoce las rutas del
// storefront, así que la plantilla la aporta quien sí las conoce; sin plantilla
// no se inventa una URL que daría 404 al cliente.
func productURL(template string, product meudim.Product) string {
	template = strings.TrimSpace(template)
	if template == "" {
		return ""
	}
	url := strings.ReplaceAll(template, "{slug}", product.Slug)
	return strings.ReplaceAll(url, "{id}", strconv.FormatInt(product.ID, 10))
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (con *Controller) logCatalogCall(operation, connection string, meta meudim.Response) {
	// Se registran ruta, cuota y totales; nunca la credencial ni el cuerpo, que
	// puede llevar datos del cliente.
	con.whatsAppLogger().Info("catálogo consultado",
		"operacion", operation, "conexion", connection,
		"total", meta.Total, "cuota_restante", meta.RateLimitRemaining)
}

// ---- Bloque del grafo ----

// execCatalogSearch ejecuta la búsqueda desde un bloque. Los argumentos llegan
// ya interpolados; el validador garantizó que solo `query` podía traer variables.
func (con *Controller) execCatalogSearch(ctx context.Context, bot *models.BotChannel,
	budget *catalogBudget, args map[string]string) (string, error) {
	params := catalogSearchParams{
		Connection:  args["connection"],
		Query:       strings.TrimSpace(args["query"]),
		Sort:        strings.TrimSpace(args["sort"]),
		URLTemplate: strings.TrimSpace(args["urlTemplate"]),
	}
	if raw := strings.TrimSpace(args["categoryId"]); raw != "" {
		params.CategoryID, _ = strconv.Atoi(raw)
	}
	params.IncludeDescendants, _ = strconv.ParseBool(defaultString(args["includeDescendants"], "false"))
	params.MinPrice, _ = strconv.ParseFloat(defaultString(args["minPrice"], "0"), 64)
	params.MaxPrice, _ = strconv.ParseFloat(defaultString(args["maxPrice"], "0"), 64)
	if raw := strings.TrimSpace(args["limit"]); raw != "" {
		params.Limit, _ = strconv.Atoi(raw)
	}
	result, err := con.searchCatalog(ctx, bot, budget, params)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}

// execCatalogProduct trae el detalle desde un bloque.
func (con *Controller) execCatalogProduct(ctx context.Context, bot *models.BotChannel,
	budget *catalogBudget, args map[string]string) (string, error) {
	result, err := con.catalogProduct(ctx, bot, budget, args["connection"],
		strings.TrimSpace(args["productId"]), strings.TrimSpace(args["slug"]),
		strings.TrimSpace(args["urlTemplate"]))
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(result)
	return string(raw), err
}

// catalogProduct resuelve el detalle por id o por slug.
//
// Un producto que no existe **no es un error**: sale como `found=false`, igual
// que una búsqueda sin coincidencias. La rama `error` queda para la conexión
// ausente, la credencial inválida y la tienda caída, que es la distinción que el
// Router necesita poder hacer.
func (con *Controller) catalogProduct(ctx context.Context, bot *models.BotChannel, budget *catalogBudget,
	connectionKey, productID, slug, urlTemplate string) (catalogResult, error) {
	client, connection, err := con.catalogConnection(ctx, bot, connectionKey)
	if err != nil {
		return catalogResult{}, err
	}
	if productID == "" && slug == "" {
		return catalogResult{}, fmt.Errorf("se requiere productId o slug")
	}
	cacheKey := fmt.Sprintf("product|%s|%s|%s|%s", bot.OrgID, connection.Key, productID, slug)
	product, cached := cachedProduct(cacheKey)
	if !cached {
		if err := budget.consume(); err != nil {
			return catalogResult{}, err
		}
		var meta meudim.Response
		if productID != "" {
			id, convErr := strconv.ParseInt(productID, 10, 64)
			if convErr != nil || id <= 0 {
				return catalogResult{}, fmt.Errorf("productId inválido")
			}
			product, meta, err = client.ProductByID(ctx, id)
		} else {
			product, meta, err = client.ProductBySlug(ctx, slug)
		}
		var apiErr *meudim.Error
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return catalogResult{Found: false, Count: 0, Records: []catalogRecord{}}, nil
		}
		con.recordCatalogCall(ctx, connection, err)
		if err != nil {
			return catalogResult{}, err
		}
		con.logCatalogCall("product", connection.Key, meta)
		catalogCache.put(cacheKey, product, catalogCacheTTL)
	}

	currency := con.storeCurrency(ctx, client, connection)
	record := catalogRecord{
		RecordID: strconv.FormatInt(product.ID, 10),
		Data:     productData(*product, currency, urlTemplate),
	}
	// El detalle es lo único que trae especificaciones e imágenes; si no se
	// proyectaran aquí, la herramienta no aportaría nada sobre la búsqueda.
	if len(product.Specifications) > 0 {
		record.Data["especificaciones"] = string(product.Specifications)
	}
	if len(product.Images) > 0 {
		urls := make([]string, 0, len(product.Images))
		for _, image := range product.Images {
			urls = append(urls, image.URL)
		}
		record.Data["imagenes"] = strings.Join(urls, ", ")
	}
	return catalogResult{Found: true, Count: 1, First: &record, Records: []catalogRecord{record}}, nil
}

func cachedProduct(key string) (*meudim.Product, bool) {
	value, ok := catalogCache.get(key)
	if !ok {
		return nil, false
	}
	product, ok := value.(*meudim.Product)
	return product, ok
}

// ---- Herramientas del agente ----

// execAgentCatalogSearch le entrega al modelo texto legible, no el JSON de la
// API: lo lee mejor y así no se le enseña la forma interna de la tienda.
func (con *Controller) execAgentCatalogSearch(ctx context.Context, bot *models.BotChannel,
	budget *catalogBudget, config map[string]string, input json.RawMessage) (string, error) {
	var args struct {
		Query    string  `json:"query"`
		MinPrice float64 `json:"minPrice"`
		MaxPrice float64 `json:"maxPrice"`
		Sort     string  `json:"sort"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("argumentos inválidos")
	}
	limit := maxAgentCatalogResults
	if raw := strings.TrimSpace(config["maxLimit"]); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	params := catalogSearchParams{
		Connection: config["connection"], Query: args.Query,
		MinPrice: args.MinPrice, MaxPrice: args.MaxPrice, Sort: args.Sort,
		Limit: limit, URLTemplate: strings.TrimSpace(config["urlTemplate"]),
	}
	if raw := strings.TrimSpace(config["categoryId"]); raw != "" {
		params.CategoryID, _ = strconv.Atoi(raw)
	}
	params.IncludeDescendants, _ = strconv.ParseBool(defaultString(config["includeDescendants"], "false"))

	result, err := con.searchCatalog(ctx, bot, budget, params)
	if err != nil {
		return catalogFailureForModel(err), nil
	}
	if !result.Found {
		// Se le dice explícitamente que no hay nada. Un resultado en blanco invita
		// a rellenar el hueco con lo que el modelo "recuerda" de un catálogo.
		return "Sin resultados en el catálogo de la tienda. Ese producto no está a la venta; dilo así en vez de suponer.", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d producto(s) en el catálogo:\n", result.Count)
	for index, record := range result.Records {
		fmt.Fprintf(&b, "\n%d. %s", index+1, renderValues(record.Data))
	}
	return b.String(), nil
}

func (con *Controller) execAgentCatalogProduct(ctx context.Context, bot *models.BotChannel,
	budget *catalogBudget, config map[string]string, input json.RawMessage) (string, error) {
	var args struct {
		ProductID int64  `json:"productId"`
		Slug      string `json:"slug"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("argumentos inválidos")
	}
	productID := ""
	if args.ProductID > 0 {
		productID = strconv.FormatInt(args.ProductID, 10)
	}
	if productID == "" && strings.TrimSpace(args.Slug) == "" {
		return "Necesito el id o el slug del producto. Búscalo primero en el catálogo.", nil
	}
	result, err := con.catalogProduct(ctx, bot, budget, config["connection"], productID,
		strings.TrimSpace(args.Slug), strings.TrimSpace(config["urlTemplate"]))
	if err != nil {
		return catalogFailureForModel(err), nil
	}
	if !result.Found {
		return "Ese producto no existe en el catálogo. No lo describas: búscalo de nuevo o dile al cliente que no lo tenemos.", nil
	}
	return renderValues(result.First.Data), nil
}

// catalogFailureForModel convierte el fallo en una instrucción que el modelo
// puede seguir.
//
// Se le devuelve texto y no un error porque un error revienta el nodo y el
// cliente recibe el mensaje genérico de siempre; así, en cambio, el bot puede
// decir que no puede consultar y ofrecer un asesor. Lo que nunca debe pasar es
// que interprete el fallo como «no lo vendemos», y por eso el texto lo dice.
func catalogFailureForModel(err error) string {
	var apiErr *meudim.Error
	switch {
	case errors.Is(err, errCatalogBudget):
		return "Ya consultaste el catálogo demasiadas veces en este mensaje. Responde con lo que obtuviste; no vuelvas a buscar."
	case errors.As(err, &apiErr) && apiErr.RateLimited():
		return "El catálogo está saturado ahora mismo. NO afirmes que el producto no existe: dile al cliente que no puedes consultar en este momento y ofrécele un asesor."
	default:
		return "No pude consultar el catálogo de la tienda. NO afirmes que el producto no existe ni inventes precios: dile al cliente que hay un problema para consultar y ofrécele un asesor."
	}
}
