// Package logger configura los destinos de registro de la aplicación.
package logger

import (
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Loggers reúne los registros especializados de infraestructura. General se
// escribe en consola y archivo; HTTP y WhatsApp se mantienen separados para
// facilitar su consulta sin mezclar tráfico ni eventos del canal.
type Loggers struct {
	General  *slog.Logger
	HTTP     *slog.Logger
	WhatsApp *slog.Logger
	closers  []io.Closer
}

const (
	maxSizeMB  = 5
	maxBackups = 20
	maxAgeDays = 28
)

// Init crea logs/ y configura rotación automática de archivos. Los archivos
// se comprimen al rotar y se conservan durante maxAgeDays días.
func Init() (*Loggers, error) {
	if err := os.MkdirAll("logs", 0o755); err != nil {
		return nil, err
	}

	generalFile := rotatingFile("general.log")
	httpFile := rotatingFile("http.log")
	whatsAppFile := rotatingFile("whatsapp.log")
	generalWriter := io.MultiWriter(os.Stdout, generalFile)
	general := newLogger(generalWriter, "general")

	// Mantiene los paquetes que usan el logger estándar dentro del registro
	// general, incluido el manejo de errores fatales del arranque.
	log.SetOutput(generalWriter)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	log.SetPrefix("[STD] ")
	slog.SetDefault(general)

	return &Loggers{
		General:  general,
		HTTP:     newLogger(httpFile, "http"),
		WhatsApp: newLogger(whatsAppFile, "whatsapp"),
		closers:  []io.Closer{generalFile, httpFile, whatsAppFile},
	}, nil
}

// Close cierra los archivos de log para que el proceso los libere al apagarse.
func (l *Loggers) Close() error {
	var firstErr error
	for _, closer := range l.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func rotatingFile(name string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   filepath.Join("logs", name),
		MaxSize:    maxSizeMB,
		MaxBackups: maxBackups,
		MaxAge:     maxAgeDays,
		Compress:   true,
	}
}

func newLogger(w io.Writer, component string) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level:     slog.LevelInfo,
		AddSource: true,
	})).With("component", component)
}
