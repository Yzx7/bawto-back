package meudim

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Store es la proyección pública de la tienda de la clave. Solo se declara lo
// que el bot necesita: nombre, dominio y la moneda con la que hay que hablarle
// al cliente. El resto del payload (tema, SEO, bloques del page-builder) es del
// storefront y no tiene sentido dentro de una conversación.
type Store struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	Settings struct {
		Currency       string `json:"currency"`
		CurrencySymbol string `json:"currencySymbol"`
		ContactEmail   string `json:"contactEmail"`
	} `json:"settings"`
}

// Product es un producto del catálogo.
//
// `Images` y `Specifications` **solo llegan en el detalle** por id o slug: la
// lista de /v1/products no los trae. Esa asimetría de la API es la razón de que
// existan dos herramientas de lectura y no una.
type Product struct {
	ID               int64           `json:"id"`
	CategoryID       int64           `json:"category_id"`
	CategoryName     string          `json:"category_name"`
	Name             string          `json:"name"`
	Slug             string          `json:"slug"`
	SKU              string          `json:"sku"`
	ShortDescription string          `json:"short_description"`
	Description      string          `json:"description"`
	MainImageURL     string          `json:"main_image_url"`
	Price            float64         `json:"price"`
	CompareAtPrice   float64         `json:"compare_at_price"`
	StockQuantity    int             `json:"stock_quantity"`
	TrackInventory   bool            `json:"track_inventory"`
	Status           string          `json:"status"`
	Specifications   json.RawMessage `json:"specifications"`
	Images           []ProductImage  `json:"images"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

// Available responde si se puede vender ahora mismo. Un producto sin control de
// inventario está siempre disponible: `stock_quantity` en 0 no significa nada
// cuando `track_inventory` es false, y tratarlo como agotado escondería medio
// catálogo de servicios.
func (p Product) Available() bool {
	return p.Status == "active" && (!p.TrackInventory || p.StockQuantity > 0)
}

type ProductImage struct {
	URL     string `json:"url"`
	AltText string `json:"alt_text"`
}

// ProductQuery son los filtros de /v1/products. Los rellena el ejecutor de la
// herramienta a partir de lo que el modelo pidió y de lo que el autor del flujo
// permitió; este paquete no decide nada de eso.
type ProductQuery struct {
	Search             string
	CategoryID         int64
	IncludeDescendants bool
	MinPrice           float64
	MaxPrice           float64
	// Sort admite price-asc, price-desc, name-asc, name-desc, newest y oldest.
	Sort   string
	Limit  int
	Offset int
}

// Store consulta los datos públicos de la tienda. Es la llamada más barata que
// prueba de verdad una credencial: si responde, la clave es válida y es de esta
// tienda.
func (c *Client) Store(ctx context.Context) (*Store, Response, error) {
	var store Store
	meta, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/store"}, &store)
	if err != nil {
		return nil, meta, err
	}
	return &store, meta, nil
}

// SearchProducts lista el catálogo.
//
// `status=active` lo impone esta función y no el llamador: un borrador de la
// tienda es trabajo a medias del dueño y no debe aparecer nunca en una
// conversación con un cliente, por mucho que la herramienta se configure mal.
func (c *Client) SearchProducts(ctx context.Context, query ProductQuery) ([]Product, Response, error) {
	values := url.Values{}
	values.Set("status", "active")
	if search := strings.TrimSpace(query.Search); search != "" {
		values.Set("search", search)
	}
	if query.CategoryID > 0 {
		values.Set("category_id", strconv.FormatInt(query.CategoryID, 10))
		if query.IncludeDescendants {
			values.Set("include_descendants", "true")
		}
	}
	if query.MinPrice > 0 {
		values.Set("min_price", strconv.FormatFloat(query.MinPrice, 'f', -1, 64))
	}
	if query.MaxPrice > 0 {
		values.Set("max_price", strconv.FormatFloat(query.MaxPrice, 'f', -1, 64))
	}
	if sort := strings.TrimSpace(query.Sort); sort != "" {
		values.Set("sort_by", sort)
	}
	if query.Limit > 0 {
		values.Set("limit", strconv.Itoa(query.Limit))
	}
	if query.Offset > 0 {
		values.Set("offset", strconv.Itoa(query.Offset))
	}
	// Las listas de Meudim son `data: []` cuando no hay nada, nunca null, así que
	// un slice vacío significa «no hay coincidencias» y no «no pude preguntar».
	products := []Product{}
	meta, err := c.do(ctx, request{method: http.MethodGet, path: "/v1/products", query: values}, &products)
	if err != nil {
		return nil, meta, err
	}
	return products, meta, nil
}

// ProductByID trae el detalle, con imágenes y especificaciones.
func (c *Client) ProductByID(ctx context.Context, id int64) (*Product, Response, error) {
	if id <= 0 {
		return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: id de producto inválido")
	}
	return c.product(ctx, "/v1/products/"+strconv.FormatInt(id, 10))
}

// ProductBySlug trae el detalle por su slug público.
func (c *Client) ProductBySlug(ctx context.Context, slug string) (*Product, Response, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: slug de producto vacío")
	}
	return c.product(ctx, "/v1/products/slug/"+url.PathEscape(slug))
}

func (c *Client) product(ctx context.Context, path string) (*Product, Response, error) {
	var product Product
	meta, err := c.do(ctx, request{method: http.MethodGet, path: path}, &product)
	if err != nil {
		return nil, meta, err
	}
	return &product, meta, nil
}
