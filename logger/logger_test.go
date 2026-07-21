package logger

import (
	"os"
	"strings"
	"testing"
)

func TestInitWritesGeneralLog(t *testing.T) {
	t.Chdir(t.TempDir())

	logs, err := Init()
	if err != nil {
		t.Fatalf("Init() error = %v", err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	logs.General.Info("registro de prueba")

	body, err := os.ReadFile("logs/general.log")
	if err != nil {
		t.Fatalf("ReadFile(general.log) error = %v", err)
	}
	if !strings.Contains(string(body), "registro de prueba") {
		t.Fatalf("general.log no contiene el registro: %q", body)
	}
}
