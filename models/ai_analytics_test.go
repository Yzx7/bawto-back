package models

import (
	"testing"
	"time"
)

func TestUsageBucketPorRango(t *testing.T) {
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	casos := []struct {
		span time.Duration
		want string
	}{
		{24 * time.Hour, bucketHour},
		{48 * time.Hour, bucketHour},
		{72 * time.Hour, bucketDay},
		{92 * 24 * time.Hour, bucketDay},
		{120 * 24 * time.Hour, bucketWeek},
	}
	for _, caso := range casos {
		if got := usageBucket(base, base.Add(caso.span)); got != caso.want {
			t.Errorf("rango de %s: se esperaba %s y llegó %s", caso.span, caso.want, got)
		}
	}
}

// El relleno tiene que coincidir con date_trunc de Postgres: si truncateToBucket
// devolviera otro instante, el punto medido no casaría con ningún hueco y la
// barra saldría duplicada (una con datos fuera de sitio y otra a cero).
//
// El truncado es en hora de Perú, así que el caso que importa es el de las horas
// UTC de madrugada: las 02:00 UTC del sábado son todavía el **viernes** en Lima,
// y truncar en UTC las metería en el día equivocado.
func TestTruncateToBucketAnclaComoDateTruncEnHoraDePeru(t *testing.T) {
	// 2026-08-01 02:30 UTC = viernes 31 de julio, 21:30 en Lima.
	at := time.Date(2026, 8, 1, 2, 30, 0, 0, time.UTC)
	casos := map[string]time.Time{
		// 21:00 de Lima = 02:00 UTC del día siguiente.
		bucketHour: time.Date(2026, 8, 1, 2, 0, 0, 0, time.UTC),
		// Medianoche del viernes 31 en Lima = 05:00 UTC del 31.
		bucketDay: time.Date(2026, 7, 31, 5, 0, 0, 0, time.UTC),
		// Esa semana arranca el lunes 27 en Lima = 05:00 UTC del 27.
		bucketWeek: time.Date(2026, 7, 27, 5, 0, 0, 0, time.UTC),
	}
	for bucket, want := range casos {
		if got := truncateToBucket(at, bucket); !got.Equal(want) {
			t.Errorf("%s: se esperaba %s y llegó %s", bucket, want, got)
		}
	}
	// Un lunes de Lima ya truncado no puede retroceder una semana entera.
	lunes := time.Date(2026, 7, 27, 5, 0, 0, 0, time.UTC)
	if got := truncateToBucket(lunes, bucketWeek); !got.Equal(lunes) {
		t.Errorf("un lunes debería quedarse donde está, llegó %s", got)
	}
}

// limaWall y fromLimaWall tienen que ser inversas exactas: la serie convierte de
// ida al truncar y de vuelta al devolver el instante, y un desfase mal aplicado
// movería todas las barras cinco horas sin romper nada visible.
func TestLimaWallEsInvertible(t *testing.T) {
	at := time.Date(2026, 8, 1, 2, 30, 0, 0, time.UTC)
	if got := limaWall(at); got.Hour() != 21 || got.Day() != 31 {
		t.Fatalf("02:30 UTC debería ser el 31 a las 21:00 en Lima, llegó %s", got)
	}
	if got := fromLimaWall(limaWall(at)); !got.Equal(at) {
		t.Errorf("ida y vuelta no devolvió el instante original: %s vs %s", got, at)
	}
}

func TestFillSeriesGapsCompletaDiasSinConsumo(t *testing.T) {
	// Medianoche de Lima para que el rango caiga en límites de bucket exactos.
	from := time.Date(2026, 8, 1, 5, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 5, 5, 0, 0, 0, time.UTC)
	// El punto llega en una Location distinta a UTC a propósito: es lo que puede
	// devolver el driver, y una comparación con == lo dejaría fuera del mapa.
	medido := []AIUsageBucket{{
		// 06:00 en +01:00 = 05:00 UTC = medianoche del 3 en Lima.
		StartsAt:    time.Date(2026, 8, 3, 6, 0, 0, 0, time.FixedZone("x", 3600)),
		Requests:    7,
		TotalTokens: 1400,
	}}
	serie := fillSeriesGaps(medido, from, to, bucketDay)
	if len(serie) != 4 {
		t.Fatalf("se esperaban 4 días y llegaron %d", len(serie))
	}
	for i, punto := range serie {
		esperado := from.AddDate(0, 0, i)
		if !punto.StartsAt.Equal(esperado) {
			t.Errorf("punto %d: se esperaba %s y llegó %s", i, esperado, punto.StartsAt)
		}
	}
	if serie[2].Requests != 7 || serie[2].TotalTokens != 1400 {
		t.Errorf("el día con consumo se perdió: %+v", serie[2])
	}
	if serie[0].Requests != 0 || serie[3].Requests != 0 {
		t.Error("los días sin consumo deberían venir en cero, no ausentes")
	}
}

// Un rango que no empieza en un límite de bucket se dibuja desde el bucket que
// lo contiene: es lo que agrupa la consulta, y omitirlo perdería el primer punto.
func TestFillSeriesGapsArrancaEnElBucketQueContieneElInicio(t *testing.T) {
	from := time.Date(2026, 8, 1, 13, 20, 0, 0, time.UTC)
	to := time.Date(2026, 8, 1, 16, 0, 0, 0, time.UTC)
	serie := fillSeriesGaps(nil, from, to, bucketHour)
	if len(serie) != 3 {
		t.Fatalf("se esperaban 3 horas y llegaron %d", len(serie))
	}
	if !serie[0].StartsAt.Equal(time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)) {
		t.Errorf("la serie debería arrancar en las 13:00, llegó %s", serie[0].StartsAt)
	}
}
