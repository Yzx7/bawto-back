package meudim

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Pedidos y pagos.
//
// El checkout de Meudim es *server-authoritative*: el precio se lee del catálogo
// dentro de la transacción y el `unit_price` que se envíe se ignora. Por eso
// aquí no existe: mandar un precio calculado por Bawto solo serviría para creer
// que se controla algo que no se controla.

// OrderItem es una línea del pedido. El precio no viaja: lo pone la tienda.
type OrderItem struct {
	ProductID int64 `json:"product_id"`
	VariantID int64 `json:"variant_id,omitempty"`
	Quantity  int   `json:"quantity"`
}

// Address es la dirección de envío. Meudim la exige completa o no la exige;
// no admite media dirección.
type Address struct {
	Name       string `json:"name"`
	Phone      string `json:"phone,omitempty"`
	Address    string `json:"address"`
	City       string `json:"city"`
	State      string `json:"state"`
	PostalCode string `json:"postalCode"`
	Country    string `json:"country"`
	Reference  string `json:"reference,omitempty"`
}

// Complete responde si la dirección puede enviarse. Una dirección a medias
// provoca un 400 del checkout; es mejor no mandarla que perder el pedido, porque
// Meudim la considera opcional.
func (a Address) Complete() bool {
	return strings.TrimSpace(a.Name) != "" && strings.TrimSpace(a.Address) != "" &&
		strings.TrimSpace(a.City) != "" && strings.TrimSpace(a.State) != "" &&
		strings.TrimSpace(a.PostalCode) != "" && strings.TrimSpace(a.Country) != ""
}

// CreateOrderInput son los únicos datos que Bawto aporta. `customer_email` e
// `items` son los dos campos que la API exige; el resto es opcional.
type CreateOrderInput struct {
	CustomerEmail   string
	CustomerName    string
	CustomerPhone   string
	Notes           string
	CouponCode      string
	Currency        string
	ShippingAddress *Address
	Items           []OrderItem
	// IdempotencyKey es obligatoria aquí aunque la API la acepte opcional: sin
	// ella un webhook reintentado por Meta crea un segundo pedido, y el cliente
	// se entera cuando le llega el doble de mercadería.
	IdempotencyKey string
}

type createOrderBody struct {
	CustomerEmail   string      `json:"customer_email"`
	CustomerName    string      `json:"customer_name,omitempty"`
	CustomerPhone   string      `json:"customer_phone,omitempty"`
	Notes           string      `json:"notes,omitempty"`
	CouponCode      string      `json:"coupon_code,omitempty"`
	Currency        string      `json:"currency,omitempty"`
	ShippingAddress *Address    `json:"shipping_address,omitempty"`
	Items           []OrderItem `json:"items"`
}

// Order es el pedido creado, con los totales calculados por el servidor.
type Order struct {
	ID          int64       `json:"id"`
	OrderNumber string      `json:"order_number"`
	Status      string      `json:"status"`
	Subtotal    float64     `json:"subtotal"`
	Tax         float64     `json:"tax"`
	Shipping    float64     `json:"shipping_cost"`
	Discount    float64     `json:"discount"`
	Total       float64     `json:"total"`
	Currency    string      `json:"currency"`
	CouponCode  string      `json:"coupon_code"`
	Items       []OrderLine `json:"items"`
}

type OrderLine struct {
	ProductID   int64   `json:"product_id"`
	ProductName string  `json:"product_name"`
	SKU         string  `json:"sku"`
	Quantity    int     `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	TotalPrice  float64 `json:"total_price"`
}

// CreateOrder ejecuta el checkout.
//
// Un `400` no es un fallo técnico: es la tienda diciendo que no —stock
// insuficiente, cupón vencido, producto inactivo— con un mensaje redactado para
// una persona. Ese mensaje se conserva en el *Error y llega hasta el flujo.
func (c *Client) CreateOrder(ctx context.Context, input CreateOrderInput) (*Order, Response, error) {
	if strings.TrimSpace(input.CustomerEmail) == "" {
		return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: el pedido requiere el correo del comprador")
	}
	if len(input.Items) == 0 {
		return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: el pedido no tiene líneas")
	}
	for _, item := range input.Items {
		if item.ProductID <= 0 || item.Quantity <= 0 {
			return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: línea con producto o cantidad inválidos")
		}
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: el pedido requiere clave de idempotencia")
	}
	body := createOrderBody{
		CustomerEmail: strings.TrimSpace(input.CustomerEmail),
		CustomerName:  strings.TrimSpace(input.CustomerName),
		CustomerPhone: strings.TrimSpace(input.CustomerPhone),
		Notes:         strings.TrimSpace(input.Notes),
		CouponCode:    strings.TrimSpace(input.CouponCode),
		Currency:      strings.TrimSpace(input.Currency),
		Items:         input.Items,
	}
	// Una dirección incompleta se omite en vez de enviarse: Meudim la trata como
	// opcional, pero si llega a medias rechaza el pedido entero.
	if input.ShippingAddress != nil && input.ShippingAddress.Complete() {
		body.ShippingAddress = input.ShippingAddress
	}
	var order Order
	meta, err := c.do(ctx, request{
		method: http.MethodPost, path: "/v1/orders", body: body,
		idempotencyKey: strings.TrimSpace(input.IdempotencyKey),
	}, &order)
	if err != nil {
		return nil, meta, err
	}
	return &order, meta, nil
}

// Payment es el intento de cobro de un pedido.
type Payment struct {
	ID        int64   `json:"id"`
	OrderID   int64   `json:"order_id"`
	Provider  string  `json:"provider"`
	Status    string  `json:"status"`
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	Reference string  `json:"reference"`
}

// PaymentAccount es una cuenta donde el comprador puede pagar.
type PaymentAccount struct {
	Bank   string `json:"bank"`
	Holder string `json:"holder"`
	Number string `json:"number"`
	CCI    string `json:"cci"`
}

// PaymentInstructions es lo que la tienda quiere que vea el comprador.
type PaymentInstructions struct {
	Provider string `json:"provider"`
	// Configured en false significa que la tienda no definió sus datos de pago.
	// No es un detalle: enviar instrucciones vacías deja al cliente sin saber a
	// dónde pagar, así que el flujo debe derivar antes que mostrar eso.
	Configured   bool             `json:"configured"`
	Warning      string           `json:"warning"`
	Instructions string           `json:"instructions"`
	Accounts     []PaymentAccount `json:"accounts"`
	QRUrls       []string         `json:"qr_urls"`
}

type PaymentIntent struct {
	Payment      Payment             `json:"payment"`
	Instructions PaymentInstructions `json:"instructions"`
}

// CreatePaymentIntent prepara el cobro manual de un pedido.
//
// Es idempotente del lado de Meudim: si el pedido ya tiene un intent activo
// devuelve ese mismo con 200 en vez de duplicarlo, así que reejecutar el bloque
// no genera dos cobros del mismo pedido.
func (c *Client) CreatePaymentIntent(ctx context.Context, orderID int64, provider string) (*PaymentIntent, Response, error) {
	if orderID <= 0 {
		return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: id de pedido inválido")
	}
	if provider = strings.TrimSpace(provider); provider == "" {
		provider = "manual"
	}
	var intent PaymentIntent
	meta, err := c.do(ctx, request{
		method: http.MethodPost,
		path:   "/v1/orders/" + strconv.FormatInt(orderID, 10) + "/payments",
		body:   map[string]string{"provider": provider},
		// La idempotencia la garantiza Meudim para este endpoint, pero la clave
		// también habilita el reintento del cliente ante un 5xx.
		idempotencyKey: fmt.Sprintf("intent:%d", orderID),
	}, &intent)
	if err != nil {
		return nil, meta, err
	}
	return &intent, meta, nil
}

// SubmitPayment declara la operación que hizo el comprador.
//
// Reenviarlo corrige la declaración en vez de fallar, así que un segundo
// comprobante del mismo cliente actualiza el número de operación en lugar de
// crear un cobro nuevo.
func (c *Client) SubmitPayment(ctx context.Context, paymentID int64, reference, receiptURL string) (*Payment, Response, error) {
	if paymentID <= 0 {
		return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: id de pago inválido")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil, Response{RateLimitRemaining: -1}, fmt.Errorf("meudim: la declaración requiere el número de operación")
	}
	body := map[string]string{"reference": reference}
	if receiptURL = strings.TrimSpace(receiptURL); receiptURL != "" {
		body["receipt_url"] = receiptURL
	}
	var payment Payment
	meta, err := c.do(ctx, request{
		method:         http.MethodPut,
		path:           "/v1/payments/" + strconv.FormatInt(paymentID, 10) + "/submit",
		body:           body,
		idempotencyKey: fmt.Sprintf("submit:%d:%s", paymentID, reference),
	}, &payment)
	if err != nil {
		return nil, meta, err
	}
	return &payment, meta, nil
}
