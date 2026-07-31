package models

import (
	"testing"
	"time"
)

func TestAdvancedProviderStatusEsMonotono(t *testing.T) {
	tests := []struct {
		current, incoming, want string
		changed                 bool
	}{
		{"", "sent", "sent", true},
		{"sent", "delivered", "delivered", true},
		{"delivered", "read", "read", true},
		{"read", "played", "played", true},
		{"played", "delivered", "played", false},
		{"read", "delivered", "read", false},
		{"read", "failed", "read", false},
		{"sent", "failed", "failed", true},
		{"failed", "sent", "failed", false},
	}
	for _, test := range tests {
		got, changed := advancedProviderStatus(test.current, test.incoming)
		if got != test.want || changed != test.changed {
			t.Errorf("%s + %s: got=(%s,%v) want=(%s,%v)",
				test.current, test.incoming, got, changed, test.want, test.changed)
		}
	}
}

func TestProviderStatusEventKeyEsIdempotente(t *testing.T) {
	at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	event := ProviderStatusEventInput{
		Channel: "wsp", ChannelID: "PNID", ProviderMessageID: "wamid.1",
		Status: "delivered", OccurredAt: at,
	}
	if ProviderStatusEventKey(event) != ProviderStatusEventKey(event) {
		t.Fatal("el mismo status debe producir la misma event_key")
	}
	other := event
	other.Status = "read"
	if ProviderStatusEventKey(event) == ProviderStatusEventKey(other) {
		t.Fatal("statuses distintos no deben colisionar")
	}
}
