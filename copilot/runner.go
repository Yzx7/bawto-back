package copilot

import (
	"context"
	"sync"
)

// Runner expone el agente de autoría ya configurado y registra las
// cancelaciones de los turnos en vuelo. El endpoint de cancel las usa para
// cortar la llamada al proveedor en la instancia que la está ejecutando; el
// estado definitivo del turno lo fija PostgreSQL, no este registro.
type Runner struct {
	Agent   *Agent
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
}

func NewRunner(agent *Agent) *Runner {
	return &Runner{Agent: agent, cancels: make(map[string]context.CancelFunc)}
}

func (r *Runner) RegisterCancel(turnID string, cancel context.CancelFunc) {
	if r == nil || turnID == "" || cancel == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cancels[turnID] = cancel
}

func (r *Runner) UnregisterCancel(turnID string) {
	if r == nil || turnID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cancels, turnID)
}

// CancelTurn detiene el proveedor del turno si corre en esta instancia.
// Devuelve false si el turno no estaba en vuelo aquí (ya terminó, o corre en
// otra instancia).
func (r *Runner) CancelTurn(turnID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	cancel, ok := r.cancels[turnID]
	r.mu.Unlock()
	if ok && cancel != nil {
		cancel()
	}
	return ok
}
