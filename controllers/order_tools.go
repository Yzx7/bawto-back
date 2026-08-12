package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Yzx7/sacs-chatbots/connectors/meudim"
	"github.com/Yzx7/sacs-chatbots/models"
)

// Ejecutores de la venta: crear el pedido, abrir el cobro y declarar el pago.
//
// Los tres son **solo de grafo**. El agente recopila qué quiere el cliente y el
// grafo ejecuta, igual que en el circuito de cobros: un modelo que puede crear
// pedidos puede crear pedidos por error, y aquí el error tiene mercadería y
// dinero detrás.
//
// Tampoco consumen el presupuesto de llamadas del turno. Ese presupuesto acota
// lo que pide el modelo; que el agente hubiera buscado de más no puede impedir
// que se registre una compra que el cliente ya confirmó.

// maxOrderLines espeja el tope del validador. Existe aquí para acotar el
// recorrido, no para validar: un bloque publicado ya pasó por engine.
const maxOrderLines = 10

// orderResult es lo que queda bajo `saveAs`. Los importes son los que calculó la
// tienda, nunca los que pudiera haber supuesto el flujo.
type orderResult struct {
	OrderID     int64   `json:"orderId"`
	OrderNumber string  `json:"orderNumber"`
	Status      string  `json:"status"`
	Subtotal    float64 `json:"subtotal"`
	Discount    float64 `json:"discount"`
	Total       float64 `json:"total"`
	Currency    string  `json:"currency"`
	ItemCount   int     `json:"itemCount"`
	// Summary lo compone el backend a partir de las líneas reales que devolvió la
	// tienda. Es lo que permite confirmarle la compra al cliente sin que la IA
	// redacte cantidades ni importes.
	Summary string `json:"summary"`
}

// execOrderCreate cierra la venta en la tienda conectada.
func (con *Controller) execOrderCreate(ctx context.Context, bot *models.BotChannel,
	channelMessageID string, args map[string]string) (string, error) {
	client, connection, err := con.catalogConnection(ctx, bot, args["connection"])
	if err != nil {
		return "", err
	}
	items, err := orderLinesFromArgs(args)
	if err != nil {
		return "", err
	}
	idempotencyKey := strings.TrimSpace(args["idempotencyKey"])
	if idempotencyKey == "" && strings.TrimSpace(channelMessageID) != "" {
		// Misma convención que data_mutate: la identidad del intento es el mensaje
		// que lo provocó. Meta reintenta webhooks, y sin esto el reintento cobra
		// dos veces.
		idempotencyKey = "message:" + strings.TrimSpace(channelMessageID)
	}
	input := meudim.CreateOrderInput{
		CustomerEmail:  strings.TrimSpace(args["customerEmail"]),
		CustomerName:   strings.TrimSpace(args["customerName"]),
		CustomerPhone:  strings.TrimSpace(args["customerPhone"]),
		Notes:          strings.TrimSpace(args["notes"]),
		CouponCode:     strings.TrimSpace(args["couponCode"]),
		Currency:       strings.TrimSpace(args["currency"]),
		Items:          items,
		IdempotencyKey: idempotencyKey,
	}
	input.ShippingAddress = shippingFromArgs(args)

	order, meta, err := client.CreateOrder(ctx, input)
	con.recordCatalogCall(ctx, connection, err)
	if err != nil {
		return "", err
	}
	con.logCatalogCall("order_create", connection.Key, meta)

	result := orderResult{
		OrderID: order.ID, OrderNumber: order.OrderNumber, Status: order.Status,
		Subtotal: order.Subtotal, Discount: order.Discount, Total: order.Total,
		Currency: order.Currency, ItemCount: len(order.Items),
		Summary: orderSummary(order),
	}
	raw, marshalErr := json.Marshal(result)
	return string(raw), marshalErr
}

// orderLinesFromArgs recompone las líneas numeradas del bloque.
//
// Se recorren por índice y no iterando el mapa: el orden de un `map` en Go es
// aleatorio, y un pedido cuyas líneas cambian de orden entre ejecuciones hace
// que un error mencione una línea distinta cada vez.
func orderLinesFromArgs(args map[string]string) ([]meudim.OrderItem, error) {
	items := make([]meudim.OrderItem, 0, maxOrderLines)
	for index := 1; index <= maxOrderLines; index++ {
		prefix := "item." + strconv.Itoa(index) + "."
		rawID := strings.TrimSpace(args[prefix+"productId"])
		if rawID == "" {
			continue
		}
		productID, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil || productID <= 0 {
			return nil, fmt.Errorf("la línea %d no tiene un producto válido", index)
		}
		quantity, err := strconv.Atoi(strings.TrimSpace(args[prefix+"quantity"]))
		if err != nil || quantity <= 0 {
			return nil, fmt.Errorf("la línea %d no tiene una cantidad válida", index)
		}
		item := meudim.OrderItem{ProductID: productID, Quantity: quantity}
		if rawVariant := strings.TrimSpace(args[prefix+"variantId"]); rawVariant != "" {
			if variantID, convErr := strconv.ParseInt(rawVariant, 10, 64); convErr == nil && variantID > 0 {
				item.VariantID = variantID
			}
		}
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("el pedido no tiene líneas")
	}
	return items, nil
}

// shippingFromArgs arma la dirección con lo que traiga el bloque.
//
// **No decide si se envía**: eso lo hace el cliente, que es quien conoce el
// contrato de Meudim —dirección completa o ninguna—. Comprobarlo también aquí
// era una duplicación silenciosa: al quitar esta copia, ninguna prueba se ponía
// roja, que es la definición de código que no sostiene nada.
func shippingFromArgs(args map[string]string) *meudim.Address {
	address := meudim.Address{
		Name:       strings.TrimSpace(args["shipping.name"]),
		Phone:      strings.TrimSpace(args["shipping.phone"]),
		Address:    strings.TrimSpace(args["shipping.address"]),
		City:       strings.TrimSpace(args["shipping.city"]),
		State:      strings.TrimSpace(args["shipping.state"]),
		PostalCode: strings.TrimSpace(args["shipping.postalCode"]),
		Country:    strings.TrimSpace(args["shipping.country"]),
		Reference:  strings.TrimSpace(args["shipping.reference"]),
	}
	return &address
}

func orderSummary(order *meudim.Order) string {
	parts := make([]string, 0, len(order.Items))
	for _, line := range order.Items {
		parts = append(parts, fmt.Sprintf("%d × %s (%s)",
			line.Quantity, line.ProductName, money(line.TotalPrice, order.Currency)))
	}
	return strings.Join(parts, " · ")
}

// paymentIntentResult expone lo justo para que el grafo decida y la Salida
// entregue. `message` va compuesto por el backend: la IA no redacta números de
// cuenta, igual que en payment_methods_render y por la misma razón.
type paymentIntentResult struct {
	PaymentID int64   `json:"paymentId"`
	OrderID   int64   `json:"orderId"`
	Status    string  `json:"status"`
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	Provider  string  `json:"provider"`
	Message   string  `json:"message"`
}

func (con *Controller) execPaymentIntentCreate(ctx context.Context, bot *models.BotChannel,
	args map[string]string) (string, error) {
	client, connection, err := con.catalogConnection(ctx, bot, args["connection"])
	if err != nil {
		return "", err
	}
	orderID, err := strconv.ParseInt(strings.TrimSpace(args["orderId"]), 10, 64)
	if err != nil || orderID <= 0 {
		return "", fmt.Errorf("orderId inválido")
	}
	intent, meta, err := client.CreatePaymentIntent(ctx, orderID, args["provider"])
	con.recordCatalogCall(ctx, connection, err)
	if err != nil {
		return "", err
	}
	con.logCatalogCall("payment_intent", connection.Key, meta)

	// Una tienda sin datos de pago configurados sale por `error`, no con un
	// mensaje a medias. Enviar instrucciones vacías deja al cliente sin saber a
	// dónde pagar, y eso es peor que derivar a una persona.
	if !intent.Instructions.Configured || len(intent.Instructions.Accounts) == 0 {
		return "", fmt.Errorf("la tienda no tiene configurados sus datos de pago: %s",
			defaultString(intent.Instructions.Warning, "settings.payments.manual está vacío"))
	}
	result := paymentIntentResult{
		PaymentID: intent.Payment.ID, OrderID: orderID, Status: intent.Payment.Status,
		Amount: intent.Payment.Amount, Currency: intent.Payment.Currency,
		Provider: intent.Payment.Provider,
		Message:  paymentMessage(intent),
	}
	raw, marshalErr := json.Marshal(result)
	return string(raw), marshalErr
}

// paymentMessage compone el texto exacto que verá el comprador.
func paymentMessage(intent *meudim.PaymentIntent) string {
	var message strings.Builder
	fmt.Fprintf(&message, "Total a pagar: %s\n\n",
		money(intent.Payment.Amount, intent.Payment.Currency))
	if text := strings.TrimSpace(intent.Instructions.Instructions); text != "" {
		message.WriteString(text)
		message.WriteString("\n\n")
	}
	if len(intent.Instructions.Accounts) == 1 {
		message.WriteString("Puedes pagar con este medio:\n\n")
	} else {
		message.WriteString("Puedes elegir uno de estos medios:\n\n")
	}
	for index, account := range intent.Instructions.Accounts {
		fmt.Fprintf(&message, "%d. %s\nDestino: %s\nTitular: %s",
			index+1, account.Bank, account.Number, account.Holder)
		if cci := strings.TrimSpace(account.CCI); cci != "" {
			fmt.Fprintf(&message, "\nCCI: %s", cci)
		}
		message.WriteString("\n\n")
	}
	message.WriteString("Cuando termines, envía una captura completa y nítida donde se vean la fecha, el importe y el número de operación.")
	return message.String()
}

type paymentSubmitResult struct {
	PaymentID      int64    `json:"paymentId"`
	OrderID        int64    `json:"orderId"`
	Status         string   `json:"status"`
	Reference      string   `json:"reference"`
	Amount         float64  `json:"amount"`
	Currency       string   `json:"currency"`
	ReceiptURL     string   `json:"receiptUrl,omitempty"`
	DeclaredAmount *float64 `json:"declaredAmount"`
	DeclaredAt     string   `json:"declaredAt,omitempty"`
	Channel        string   `json:"channel,omitempty"`
	PayerName      string   `json:"payerName,omitempty"`
	Recipient      string   `json:"recipient,omitempty"`
	AmountMatches  *bool    `json:"amountMatches"`
}

// execPaymentSubmit registra el número de operación que declaró el comprador.
// Confirmarlo sigue fuera del bot: disponer de una clave secreta no registra por
// sí solo una herramienta capaz de dar por cobrado algo.
func (con *Controller) execPaymentSubmit(ctx context.Context, bot *models.BotChannel,
	args map[string]string) (string, error) {
	client, connection, err := con.catalogConnection(ctx, bot, args["connection"])
	if err != nil {
		return "", err
	}
	paymentID, err := strconv.ParseInt(strings.TrimSpace(args["paymentId"]), 10, 64)
	if err != nil || paymentID <= 0 {
		return "", fmt.Errorf("paymentId inválido")
	}
	reference := strings.TrimSpace(args["reference"])
	if reference == "" {
		return "", fmt.Errorf("falta el número de operación declarado")
	}
	var declaredAmount *float64
	if value := strings.TrimSpace(args["declaredAmount"]); value != "" {
		parsed, parseErr := strconv.ParseFloat(value, 64)
		if parseErr != nil || parsed < 0 {
			return "", fmt.Errorf("declaredAmount debe ser un importe mayor o igual a cero")
		}
		declaredAmount = &parsed
	}
	payment, meta, err := client.SubmitPayment(ctx, paymentID, meudim.SubmitPaymentInput{
		Reference: reference, ReceiptURL: args["receiptUrl"], DeclaredAmount: declaredAmount,
		DeclaredAt: args["declaredAt"], Channel: args["channel"],
		PayerName: args["payerName"], Recipient: args["recipient"],
	})
	con.recordCatalogCall(ctx, connection, err)
	if err != nil {
		return "", err
	}
	con.logCatalogCall("payment_submit", connection.Key, meta)
	raw, marshalErr := json.Marshal(paymentSubmitResult{
		PaymentID: payment.ID, OrderID: payment.OrderID,
		Status: payment.Status, Reference: payment.Reference,
		Amount: payment.Amount, Currency: payment.Currency, ReceiptURL: payment.ReceiptURL,
		DeclaredAmount: payment.DeclaredAmount, DeclaredAt: payment.DeclaredAt,
		Channel: payment.Channel, PayerName: payment.PayerName, Recipient: payment.Recipient,
		AmountMatches: payment.AmountMatches,
	})
	return string(raw), marshalErr
}

// money formatea un importe con su moneda. PEN se muestra como lo escribe todo
// el mundo en Perú; el resto queda como código ISO, que es preferible a
// inventarse un símbolo equivocado.
func money(amount float64, currency string) string {
	switch strings.ToUpper(strings.TrimSpace(currency)) {
	case "PEN":
		return fmt.Sprintf("S/ %.2f", amount)
	case "USD":
		return fmt.Sprintf("US$ %.2f", amount)
	case "":
		return fmt.Sprintf("%.2f", amount)
	default:
		return fmt.Sprintf("%.2f %s", amount, strings.ToUpper(currency))
	}
}
