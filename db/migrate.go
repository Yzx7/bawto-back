package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migrador versionado. Sustituye el "aplicar los .sql a mano" que había hasta
// ahora: los archivos de db/migrations van embebidos en el binario, se aplican
// en orden, cada uno en su propia transacción, y queda registro en
// `schema_migrations` de qué se aplicó y con qué contenido.
//
// Se ejecuta desde `cmd/migrate`, no en el arranque del servidor. Motivo
// concreto (ver DEPLOY.md): dos instancias del backend comparten la misma base
// —una de ellas es la PC de desarrollo con código a medio hacer— y migrar en el
// arranque dejaría que cualquiera de las dos aplique DDL a producción sin que
// nadie lo pida.

//go:embed migrations/*.sql
var migrationFS embed.FS

// baselineDirective marca una migración que se registra como aplicada **sin
// ejecutarse**. Es para las migraciones históricas que ya se aplicaron a mano y
// cuyo efecto está también en schema.sql: volver a ejecutarlas fallaría (001
// referencia columnas que ella misma elimina).
const baselineDirective = "-- migrate:baseline"

// advisoryLockKey serializa dos migradores simultáneos. Es una optimización:
// la corrección la da el registro en schema_migrations dentro de la misma
// transacción que la migración.
const advisoryLockKey int64 = 8410231147

// Migration es un archivo numerado de db/migrations.
type Migration struct {
	Version  int
	Name     string
	File     string
	SQL      string
	Checksum string
	Baseline bool
}

// AppliedMigration es una fila de schema_migrations.
type AppliedMigration struct {
	Version  int
	Name     string
	Checksum string
}

// MigrationStatus cruza lo que hay en disco con lo que hay en la base.
type MigrationStatus struct {
	Migration
	Applied bool
	// Drift: la migración ya aplicada tiene hoy un contenido distinto. Es un
	// error, no un aviso: significa que alguien editó un archivo ya aplicado y
	// nadie sabe qué está corriendo realmente en producción.
	Drift bool
}

// LoadMigrations lee y ordena las migraciones embebidas.
func LoadMigrations() ([]Migration, error) {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: leer migrations: %w", err)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("migrate: versión %d duplicada (%s y %s)", version, prev, e.Name())
		}
		seen[version] = e.Name()

		raw, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("migrate: leer %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, Migration{
			Version:  version,
			Name:     name,
			File:     e.Name(),
			SQL:      string(raw),
			Checksum: hex.EncodeToString(sum[:]),
			Baseline: strings.Contains(string(raw), baselineDirective),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// parseMigrationName acepta `NNN_nombre_legible.sql`.
func parseMigrationName(file string) (int, string, error) {
	base := strings.TrimSuffix(file, ".sql")
	num, rest, ok := strings.Cut(base, "_")
	if !ok {
		return 0, "", fmt.Errorf("migrate: %q no sigue el formato NNN_nombre.sql", file)
	}
	version, err := strconv.Atoi(num)
	if err != nil || version <= 0 {
		return 0, "", fmt.Errorf("migrate: %q no empieza por un número de versión válido", file)
	}
	return version, rest, nil
}

// ensureMigrationsTable crea el registro. Es la única sentencia que el migrador
// ejecuta fuera de la transacción de una migración.
func ensureMigrationsTable(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version     INTEGER     PRIMARY KEY,
		    name        TEXT        NOT NULL,
		    checksum    TEXT        NOT NULL,
		    baseline    BOOLEAN     NOT NULL DEFAULT FALSE,
		    applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
	return err
}

// Status devuelve el estado de cada migración sin aplicar nada.
func Status(ctx context.Context, pool *pgxpool.Pool) ([]MigrationStatus, error) {
	files, err := LoadMigrations()
	if err != nil {
		return nil, err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: conexión: %w", err)
	}
	defer conn.Release()

	if err := ensureMigrationsTable(ctx, conn); err != nil {
		return nil, fmt.Errorf("migrate: crear schema_migrations: %w", err)
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return nil, err
	}
	out := make([]MigrationStatus, 0, len(files))
	for _, m := range files {
		st := MigrationStatus{Migration: m}
		if a, ok := applied[m.Version]; ok {
			st.Applied = true
			st.Drift = a.Checksum != m.Checksum
		}
		out = append(out, st)
	}
	return out, nil
}

// Migrate aplica las migraciones pendientes en orden. Devuelve cuántas aplicó.
//
// Garantías:
//   - cada migración corre en su propia transacción junto con su registro, así
//     que una migración a medias no queda marcada como aplicada;
//   - re-ejecutar el comando no hace nada (idempotente);
//   - si una migración ya aplicada cambió de contenido, aborta con error en vez
//     de seguir: nadie puede saber qué hay realmente en la base.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (int, error) {
	files, err := LoadMigrations()
	if err != nil {
		return 0, err
	}

	// Conexión dedicada: el advisory lock es por sesión y con pgxpool la
	// conexión vuelve al pool con el lock puesto si no se sostiene.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("migrate: conexión: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return 0, fmt.Errorf("migrate: advisory lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, advisoryLockKey) }()

	if err := ensureMigrationsTable(ctx, conn); err != nil {
		return 0, fmt.Errorf("migrate: crear schema_migrations: %w", err)
	}
	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return 0, err
	}

	count := 0
	for _, m := range files {
		if a, ok := applied[m.Version]; ok {
			if a.Checksum != m.Checksum {
				return count, fmt.Errorf(
					"migrate: la migración %03d_%s ya está aplicada pero su contenido cambió "+
						"(registrado %s…, archivo %s…). Una migración aplicada es inmutable: "+
						"crea una nueva en vez de editarla",
					m.Version, m.Name, a.Checksum[:8], m.Checksum[:8])
			}
			continue
		}
		if err := applyOne(ctx, conn, m, log); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func applyOne(ctx context.Context, conn *pgxpool.Conn, m Migration, log *slog.Logger) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %03d begin: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if !m.Baseline {
		if err := rejectOwnTransaction(m); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return fmt.Errorf("migrate: %03d_%s falló: %w", m.Version, m.Name, err)
		}
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_migrations(version, name, checksum, baseline) VALUES ($1,$2,$3,$4)`,
		m.Version, m.Name, m.Checksum, m.Baseline); err != nil {
		return fmt.Errorf("migrate: %03d registrar: %w", m.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: %03d commit: %w", m.Version, err)
	}
	if log != nil {
		if m.Baseline {
			log.Info("migración registrada como baseline (no se ejecuta)", "version", m.Version, "name", m.Name)
		} else {
			log.Info("migración aplicada", "version", m.Version, "name", m.Name)
		}
	}
	return nil
}

// rejectOwnTransaction impide que una migración abra su propia transacción: un
// COMMIT dentro del bloque cerraría la transacción del migrador y dejaría el
// resto de la migración sin protección.
func rejectOwnTransaction(m Migration) error {
	for _, line := range strings.Split(m.SQL, "\n") {
		switch strings.ToUpper(strings.TrimSpace(line)) {
		case "BEGIN;", "COMMIT;", "ROLLBACK;", "START TRANSACTION;":
			return fmt.Errorf(
				"migrate: %03d_%s contiene BEGIN/COMMIT propios; el migrador ya envuelve "+
					"cada archivo en una transacción", m.Version, m.Name)
		}
	}
	return nil
}

func appliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[int]AppliedMigration, error) {
	rows, err := conn.Query(ctx, `SELECT version, name, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: leer schema_migrations: %w", err)
	}
	defer rows.Close()

	out := map[int]AppliedMigration{}
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum); err != nil {
			return nil, err
		}
		out[a.Version] = a
	}
	return out, rows.Err()
}
