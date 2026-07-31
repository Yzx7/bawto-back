// Herramienta de un solo uso: lista orgs/bots y (opcional) conecta un canal WhatsApp.
//
//	go run ./cmd/wachan                 → lista orgs y bots
//	go run ./cmd/wachan -bot <id> -pnid <phoneNumberID> -phone <phone> -token <token>
//	                                     → cifra el token y conecta el canal ("wsp")
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/Yzx7/sacs-chatbots/channels/whatsapp"
	"github.com/Yzx7/sacs-chatbots/helpers"
	"github.com/Yzx7/sacs-chatbots/models"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	_ = godotenv.Load()
	botID := flag.String("bot", "", "bot id a conectar")
	pnid := flag.String("pnid", "", "phone_number_id")
	phone := flag.String("phone", "", "display phone (opcional)")
	token := flag.String("token", "", "access token (se cifra)")
	migrate := flag.Bool("migrate", false, "aplica migraciones idempotentes")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		fmt.Println("ERR pool:", err)
		os.Exit(1)
	}
	defer pool.Close()

	if *migrate {
		for _, stmt := range []string{
			`ALTER TABLE chats ADD COLUMN IF NOT EXISTS handoff_until TIMESTAMPTZ`,
			`ALTER TABLE chats ADD COLUMN IF NOT EXISTS last_read_at TIMESTAMPTZ`,
		} {
			if _, err := pool.Exec(ctx, stmt); err != nil {
				fmt.Println("ERR migrate:", err)
				os.Exit(1)
			}
			fmt.Println("OK:", stmt)
		}
		return
	}

	if *botID != "" && *pnid != "" && *token != "" {
		cph, err := helpers.NewCipher(os.Getenv("TOKEN_ENC_KEY"))
		if err != nil {
			fmt.Println("ERR cipher:", err)
			os.Exit(1)
		}
		enc, err := cph.Encrypt(*token)
		if err != nil {
			fmt.Println("ERR encrypt:", err)
			os.Exit(1)
		}
		if err := models.UpdateBotChannel(ctx, pool, *botID, "wsp", *pnid, *phone, enc); err != nil {
			fmt.Println("ERR connect:", err)
			os.Exit(1)
		}
		fmt.Printf("OK canal conectado: bot=%s channel=wsp channel_id=%s\n", *botID, *pnid)
		return
	}

	if *botID != "" {
		fmt.Println("== CHATS del bot ==")
		crows, _ := pool.Query(ctx, `SELECT id::text, contact, COALESCE(contact_name,'-'), COALESCE(current_layer::text,'null') FROM chats WHERE bot_id=$1::uuid ORDER BY updated_at DESC`, *botID)
		for crows.Next() {
			var id, cid, cname, layer string
			_ = crows.Scan(&id, &cid, &cname, &layer)
			fmt.Printf("  chat=%s contact=%s (%s) state=%s\n", id, cid, cname, layer)
		}
		crows.Close()
		fmt.Println("== ÚLTIMOS MENSAJES ==")
		mrows, _ := pool.Query(ctx, `SELECT m.created_at, m.from_me, m.type, COALESCE(m.body,'') FROM messages m JOIN chats c ON c.id=m.chat_id WHERE c.bot_id=$1::uuid ORDER BY m.created_at DESC LIMIT 12`, *botID)
		for mrows.Next() {
			var t time.Time
			var out bool
			var kind, body string
			_ = mrows.Scan(&t, &out, &kind, &body)
			dir := "IN "
			if out {
				dir = "OUT"
			}
			fmt.Printf("  %s [%s/%s] %s\n", t.Format("15:04:05"), dir, kind, body)
		}
		mrows.Close()

		// Info del canal en Meta (mismo camino que GET /bots/:id/channel del panel).
		fmt.Println("== CANAL (info en Meta) ==")
		var chid string
		var tok []byte
		if err := pool.QueryRow(ctx, `SELECT COALESCE(channel_id,''), token_enc FROM bots WHERE id=$1::uuid`, *botID).Scan(&chid, &tok); err != nil {
			fmt.Println("  err:", err)
		} else if chid == "" || len(tok) == 0 {
			fmt.Println("  sin canal o sin token")
		} else if cph, err := helpers.NewCipher(os.Getenv("TOKEN_ENC_KEY")); err != nil {
			fmt.Println("  cipher:", err)
		} else if token, err := cph.Decrypt(tok); err != nil {
			fmt.Println("  decrypt:", err)
		} else if info, err := whatsapp.GetPhoneInfo(ctx, whatsapp.SendConfig{
			APIBase:       env("WHATSAPP_API_BASE", "https://graph.facebook.com"),
			Version:       env("WHATSAPP_API_VERSION", "v21.0"),
			PhoneNumberID: chid,
			Token:         token,
		}); err != nil {
			fmt.Println("  ERR:", err)
		} else {
			fmt.Printf("  numero=%s nombre=%q calidad=%s plataforma=%s verif=%s tput=%s id=%s\n",
				info.DisplayPhoneNumber, info.VerifiedName, info.QualityRating,
				info.PlatformType, info.CodeVerification, info.Throughput, info.PhoneNumberID)
		}
		return
	}

	// Listado
	fmt.Println("== ORGS ==")
	rows, _ := pool.Query(ctx, `SELECT id::text, name, owner_id::text FROM organizations ORDER BY created_at`)
	for rows.Next() {
		var id, name, owner string
		_ = rows.Scan(&id, &name, &owner)
		fmt.Printf("  %s | %s | owner=%s\n", id, name, owner)
	}
	rows.Close()

	fmt.Println("== BOTS ==")
	brows, _ := pool.Query(ctx, `SELECT id::text, org_id::text, name, channel, COALESCE(channel_id,'-'), (token_enc IS NOT NULL) FROM bots ORDER BY created_at`)
	for brows.Next() {
		var id, org, name, ch, chid string
		var hasTok bool
		_ = brows.Scan(&id, &org, &name, &ch, &chid, &hasTok)
		fmt.Printf("  %s | org=%s | %q | ch=%s chid=%s token=%v\n", id, org, name, ch, chid, hasTok)
	}
	brows.Close()
}
