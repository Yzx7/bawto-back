package scheduler

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNextOccurrencesRespetaLaZonaHoraria(t *testing.T) {
	lima, err := time.LoadLocation("America/Lima")
	if err != nil {
		t.Skipf("tzdata no disponible: %v", err)
	}
	from := time.Date(2026, 7, 28, 20, 0, 0, 0, time.UTC) // 15:00 en Lima
	next, err := NextOccurrences("0 9 * * *", lima, from, 3)
	if err != nil {
		t.Fatalf("NextOccurrences: %v", err)
	}
	if len(next) != 3 {
		t.Fatalf("esperaba 3 ocurrencias, got %d", len(next))
	}
	// Las 09:00 de Lima son las 14:00 UTC, y deben salir en días consecutivos.
	for i, occurrence := range next {
		if occurrence.Hour() != 14 || occurrence.Minute() != 0 {
			t.Fatalf("ocurrencia %d no es 09:00 de Lima: %s", i, occurrence)
		}
	}
	if !next[1].Equal(next[0].Add(24*time.Hour)) || !next[2].Equal(next[1].Add(24*time.Hour)) {
		t.Fatalf("las ocurrencias no son diarias: %v", next)
	}
}

func TestNextOccurrencesRechazaCronInvalido(t *testing.T) {
	if _, err := NextOccurrences("no soy un cron", time.UTC, time.Now(), 5); err == nil {
		t.Fatal("un cron inválido debe fallar aquí y no al publicar")
	}
	if err := ValidateCron("0 9 * * *"); err != nil {
		t.Fatalf("ValidateCron rechazó un cron válido: %v", err)
	}
}

func TestRenderTemplateBodySustituyeLosParametrosDeMeta(t *testing.T) {
	body := "Hola {{1}}, tu recibo de {{2}} vence el {{3}}."
	got := renderTemplateBody(body, []string{"Ana", "S/ 90", "2026-07-31"})
	want := "Hola Ana, tu recibo de S/ 90 vence el 2026-07-31."
	if got != want {
		t.Fatalf("render inesperado:\n got %q\nwant %q", got, want)
	}
	// Sin parámetros se devuelve el cuerpo tal cual: media plantilla rellenada
	// confundiría más que la plantilla cruda.
	if renderTemplateBody(body, nil) != body {
		t.Fatal("sin parámetros el cuerpo no debe tocarse")
	}
}

func TestTemplateBodyIgnoraHeaderYFooter(t *testing.T) {
	components := json.RawMessage(`[
		{"type":"HEADER","text":"Aviso"},
		{"type":"BODY","text":"Cuerpo real {{1}}"},
		{"type":"FOOTER","text":"Responde STOP"}
	]`)
	if got := templateBody(components); got != "Cuerpo real {{1}}" {
		t.Fatalf("templateBody: %q", got)
	}
	if got := templateBody(json.RawMessage(`no es json`)); got != "" {
		t.Fatalf("un catálogo ilegible debe devolver vacío, no romper: %q", got)
	}
}

func TestFillSchedulePreviewSeparaLoRecuperableDeLoPerdido(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	// Seis horas paradas con un cron horario: 07:00…12:00 quedaron pendientes.
	// El corte es el mismo que aplica el scheduler (`Before(now-ventana)`), así
	// que la de las 10:00 entra por ser exactamente el límite: el preview no
	// puede prometer menos de lo que el worker va a encolar.
	lastTick := now.Add(-6 * time.Hour)
	out := PreviewSchedule{LastTickAt: &lastTick}
	if err := fillSchedulePreview(&out, "0 * * * *", time.UTC, 2*time.Hour, now); err != nil {
		t.Fatalf("fillSchedulePreview: %v", err)
	}
	if out.CatchupPending+out.CatchupDiscard != 6 {
		t.Fatalf("esperaba 6 ocurrencias perdidas, got %d+%d", out.CatchupPending, out.CatchupDiscard)
	}
	if out.CatchupPending != 3 {
		t.Fatalf("la ventana de 2h debe recuperar 10:00, 11:00 y 12:00, got %d", out.CatchupPending)
	}
	if out.CatchupDiscard != 3 {
		t.Fatalf("07:00, 08:00 y 09:00 deben verse como descartadas, got %d", out.CatchupDiscard)
	}
	if len(out.NextOccurrences) != 5 {
		t.Fatalf("esperaba 5 próximas ocurrencias, got %d", len(out.NextOccurrences))
	}
}

func TestFillSchedulePreviewSinTickPrevioNoInventaCatchup(t *testing.T) {
	out := PreviewSchedule{}
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if err := fillSchedulePreview(&out, "0 * * * *", time.UTC, 2*time.Hour, now); err != nil {
		t.Fatalf("fillSchedulePreview: %v", err)
	}
	if out.CatchupPending != 0 || out.CatchupDiscard != 0 {
		t.Fatalf("un flujo que nunca ha corrido no tiene nada que recuperar: %+v", out)
	}
}
