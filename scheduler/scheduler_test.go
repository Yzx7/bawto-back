package scheduler

import (
	"testing"
	"time"
)

func TestOccurrencesExactasYCatchup(t *testing.T) {
	loc, err := time.LoadLocation("America/Lima")
	if err != nil {
		t.Fatal(err)
	}
	last := time.Date(2026, 7, 28, 8, 58, 0, 0, loc)
	now := time.Date(2026, 7, 28, 9, 31, 0, 0, loc)
	got, err := Occurrences("*/15 9 * * 1-5", loc, last, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("se esperaban 09:00, 09:15 y 09:30; got=%v", got)
	}
	for i, minute := range []int{0, 15, 30} {
		if got[i].In(loc).Hour() != 9 || got[i].In(loc).Minute() != minute {
			t.Fatalf("ocurrencia %d inesperada: %s", i, got[i].In(loc))
		}
	}
}

func TestOccurrencesRechazaCronInvalido(t *testing.T) {
	if _, err := Occurrences("cada rato", time.UTC, time.Now().Add(-time.Hour), time.Now()); err == nil {
		t.Fatal("cron inválido debería fallar")
	}
}

func TestRetryDelayEscalaYTieneJitterAcotado(t *testing.T) {
	if got := retryDelay(1); got != 0 {
		t.Fatalf("primer retry debe ser inmediato: %s", got)
	}
	cases := []struct {
		attempt int
		base    time.Duration
	}{
		{2, time.Minute},
		{3, 5 * time.Minute},
		{4, 30 * time.Minute},
		{5, 2 * time.Hour},
	}
	for _, tc := range cases {
		got := retryDelay(tc.attempt)
		if got < tc.base/2 || got > tc.base+tc.base/2 {
			t.Fatalf("attempt %d fuera del jitter 0.5–1.5: %s", tc.attempt, got)
		}
	}
}
