// Package auth valida los JWT emitidos por Better Auth (frontend) contra su
// endpoint JWKS. Mejora sobre el patrón meud: el JWKS se cachea en memoria
// (no se hace fetch en cada request) y no se consulta la BD por request —
// se confía en los claims del token. Los roles multi-tenant viven en memberships.
package auth

import (
	"context"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/lestrrat-go/jwx/v3/jwk"
	jwtlib "github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/Yzx7/sacs-chatbots/env"
	"github.com/Yzx7/sacs-chatbots/types"
)

// Claims son los datos que extraemos del JWT y dejamos en c.Locals.
type Claims struct {
	UserID string
	Email  string
	Name   string
}

var (
	appEnv *env.Env

	// Cache del JWKS.
	ksMu      sync.RWMutex
	keySet    jwk.Set
	ksFetched time.Time
	ksTTL     = 10 * time.Minute
)

// Configure inyecta las dependencias del middleware (patrón de burrito-esp).
func Configure(e *env.Env) {
	appEnv = e
}

// refreshKeySet descarga el JWKS y lo cachea. force ignora el TTL.
func refreshKeySet(ctx context.Context, force bool) (jwk.Set, error) {
	ksMu.RLock()
	if !force && keySet != nil && time.Since(ksFetched) < ksTTL {
		ks := keySet
		ksMu.RUnlock()
		return ks, nil
	}
	ksMu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	set, err := jwk.Fetch(ctx, appEnv.Config.JWKSURL)
	if err != nil {
		return nil, err
	}

	ksMu.Lock()
	keySet = set
	ksFetched = time.Now()
	ksMu.Unlock()
	return set, nil
}

// parse valida el token contra el JWKS cacheado; si falla (posible rotación de
// llaves) reintenta una vez con un refresh forzado.
func parse(tokenString string) (jwtlib.Token, error) {
	ctx := context.Background()

	set, err := refreshKeySet(ctx, false)
	if err != nil {
		return nil, err
	}
	tok, err := jwtlib.ParseString(tokenString, jwtlib.WithKeySet(set))
	if err == nil {
		return tok, nil
	}

	// Reintento con refresh forzado (llaves rotadas).
	set, ferr := refreshKeySet(ctx, true)
	if ferr != nil {
		return nil, err
	}
	return jwtlib.ParseString(tokenString, jwtlib.WithKeySet(set))
}

// VerifyToken es el middleware de Fiber que protege rutas.
// Lee el JWT del header x-access-token (inyectado por el proxy del frontend).
func VerifyToken(c *fiber.Ctx) error {
	if appEnv == nil {
		return c.Status(fiber.StatusInternalServerError).JSON(types.Err("auth no configurado"))
	}

	tokenString := c.Get("x-access-token")
	if tokenString == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(types.Err("No token provided"))
	}

	tok, err := parse(tokenString)
	if err != nil {
		return c.Status(fiber.StatusUnauthorized).JSON(types.Err("Token inválido"))
	}

	sub, ok := tok.Subject()
	if !ok || sub == "" {
		return c.Status(fiber.StatusUnauthorized).JSON(types.Err("Token sin subject"))
	}

	var email, name string
	_ = tok.Get("email", &email)
	_ = tok.Get("name", &name)

	c.Locals("claims", Claims{UserID: sub, Email: email, Name: name})
	c.Locals("userID", sub)
	return c.Next()
}

// Current devuelve los claims del usuario autenticado desde c.Locals.
func Current(c *fiber.Ctx) (Claims, bool) {
	cl, ok := c.Locals("claims").(Claims)
	return cl, ok
}
