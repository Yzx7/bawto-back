// Programa efímero de SOLO LECTURA: lista los objetos de datos que las
// organizaciones han creado de verdad, con sus campos y cuántos registros
// tienen. Sirve para proponer una semilla a partir del uso real y no de una
// idea. Se borra al terminar.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		fmt.Println("falta DATABASE_URL")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		fmt.Println("no conecta:", err)
		os.Exit(1)
	}
	defer pool.Close()

	var srv string
	_ = pool.QueryRow(ctx, `select coalesce(host(inet_server_addr()),'socket')`).Scan(&srv)
	fmt.Printf("servidor=%s\n\n", srv)

	rows, err := pool.Query(ctx, `
		SELECT o.id::text, coalesce(g.name,'?'), o.key, coalesce(o.name,''),
		       (SELECT count(*) FROM data_records r WHERE r.object_id = o.id)
		  FROM data_objects o
		  LEFT JOIN organizations g ON g.id = o.org_id
		 ORDER BY g.name, o.key`)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	type obj struct {
		id, org, key, name string
		records            int
	}
	var objs []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.id, &o.org, &o.key, &o.name, &o.records); err == nil {
			objs = append(objs, o)
		}
	}
	rows.Close()

	orgActual := ""
	for _, o := range objs {
		if o.org != orgActual {
			fmt.Printf("\n=== %s ===\n", o.org)
			orgActual = o.org
		}
		fmt.Printf("\n  %-26s %-28s %d registros\n", o.key, "("+o.name+")", o.records)

		frows, err := pool.Query(ctx, `
			SELECT key, coalesce(type,''), coalesce(required,false)
			  FROM data_fields WHERE object_id = $1::uuid ORDER BY created_at`, o.id)
		if err != nil {
			continue
		}
		for frows.Next() {
			var k, t string
			var req bool
			if err := frows.Scan(&k, &t, &req); err == nil {
				marca := " "
				if req {
					marca = "*"
				}
				fmt.Printf("      %s %-22s %s\n", marca, k, t)
			}
		}
		frows.Close()
	}
	fmt.Println("\n(* = obligatorio)")
}
