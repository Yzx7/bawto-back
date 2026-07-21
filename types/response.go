package types

// GenRes es la forma estándar de respuesta HTTP de la API.
// { ok, msg, data }
type GenRes struct {
	Ok   bool   `json:"ok"`
	Msg  string `json:"msg,omitempty"`
	Data any    `json:"data,omitempty"`
}

// OK construye una respuesta exitosa.
func OK(msg string, data any) GenRes {
	return GenRes{Ok: true, Msg: msg, Data: data}
}

// Err construye una respuesta de error (mensaje controlado hacia el cliente).
func Err(msg string) GenRes {
	return GenRes{Ok: false, Msg: msg}
}
