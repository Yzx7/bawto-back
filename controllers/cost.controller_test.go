package controllers

import (
	"testing"
	"time"
)

func TestCostPeriodDefaultMesActual(t *testing.T) {
	now := time.Date(2026, 7, 29, 15, 30, 0, 0, time.UTC)
	from, to, err := costPeriod("", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if from != time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) || to != now {
		t.Fatalf("periodo inesperado: %s → %s", from, to)
	}
}

func TestCostPeriodFechaFinalEsInclusiva(t *testing.T) {
	from, to, err := costPeriod("2026-07-01", "2026-07-29", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if from.Day() != 1 || to != time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("periodo inesperado: %s → %s", from, to)
	}
}

func TestCostPeriodRechazaRangoExcesivo(t *testing.T) {
	if _, _, err := costPeriod("2025-01-01", "2026-07-29", time.Now()); err == nil {
		t.Fatal("se esperaba rechazo del rango mayor a 366 días")
	}
}
