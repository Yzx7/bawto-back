# Deploy del backend (Bawto)

Estado actual del despliegue en producción y cómo actualizarlo. Verificado
e2e el 2026-07-22.

## Topología

```
WhatsApp / navegador
        │ https
        ▼
nginx @ yuriser (209.74.83.236)          bawto.sistemuino.com
        │
        ├─ primario:  10.11.12.2:3009    ← PC de Gerson (WireGuard, APP_ENV=dev)
        └─ backup:    127.0.0.1:3009     ← backend en el server  (APP_ENV=prod)
                                            systemd: bawto-backend.service
        ▼
Postgres 16 (mismo server; escucha en 127.0.0.1 y 10.11.12.1)
```

- Con la PC encendida, nginx enruta **siempre al primario** (la respuesta de
  `GET /` dice `env: dev`).
- Con la PC apagada, el connect falla a los **3 s** y nginx reintenta en el
  backup (`env: prod`). Aplica también a POST (webhooks de Meta) gracias a
  `non_idempotent`; es seguro porque el procesamiento es idempotente por
  `messages.wa_id`.
- Ambas instancias usan **la misma base** (`sacs_chatbots`), así que el estado
  de chats/flujos es consistente al conmutar.

## Piezas en el server (`ssh root@209.74.83.236 -p 22022`)

| Pieza | Ruta | Notas |
|---|---|---|
| Binario | `/opt/bawto/bawto-backend` | Go, linux/amd64, corre como `www-data` |
| Config | `/opt/bawto/.env` | `chmod 600`; Postgres por `127.0.0.1`; bindea `127.0.0.1:3009` |
| Servicio | `/etc/systemd/system/bawto-backend.service` | `Restart=always`, enabled al boot |
| nginx | `/etc/nginx/sites-available/bawto.conf` | upstream `bawto_backend` (primario + backup); backup del original en `/root/bawto.conf.bak.*` |

Comandos útiles en el server:

```bash
systemctl status bawto-backend        # estado
journalctl -u bawto-backend -f        # logs en vivo
curl -s http://127.0.0.1:3009/        # health local ({"ok":true,...,"env":"prod"})
nginx -t && systemctl reload nginx    # tras tocar bawto.conf
```

## Cómo actualizar el binario (redeploy)

Desde la PC (PowerShell, en `backend/`):

```powershell
$env:GOOS='linux'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
go build -trimpath -ldflags '-s -w' -o "$env:TEMP\bawto-backend" .
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED
scp -P 22022 "$env:TEMP\bawto-backend" root@209.74.83.236:/opt/bawto/bawto-backend.new
ssh -p 22022 root@209.74.83.236 "systemctl stop bawto-backend && mv /opt/bawto/bawto-backend.new /opt/bawto/bawto-backend && chmod +x /opt/bawto/bawto-backend && chown www-data:www-data /opt/bawto/bawto-backend && systemctl start bawto-backend && sleep 2 && curl -s http://127.0.0.1:3009/"
```

(Se sube como `.new` y se mueve con el servicio parado porque Linux no deja
sobreescribir un binario en ejecución.)

Verificación del failover (la respuesta delata quién contesta):

```powershell
curl.exe -s https://bawto.sistemuino.com/   # env:dev = PC · env:prod = server
```

## Diferencias del `.env` de producción vs dev

| Variable | Server (prod) | PC (dev) |
|---|---|---|
| `APP_ENV` | `prod` | `dev` |
| `SERVER_PORT` | `127.0.0.1:3009` (solo loopback; nginx es el frontdoor) | `:3009` |
| `DATABASE_URL` | host `127.0.0.1` (Postgres local) | host `10.11.12.1` (vía WireGuard) |
| `JWKS_URL` | *(default `localhost:3000` — ver caveat)* | default |

El resto (claves de cifrado, secretos de Meta, IA) es idéntico.

> Nota: `SERVER_PORT` acepta forma `host:puerto` desde el fix de
> `config/config.go` (commit `fa19769`); binarios anteriores solo aceptaban
> `:puerto`.

## Caveats conocidos

1. **Scheduler duplicado** — con ambas instancias arriba, las dos corren el
   worker de cron contra la misma BD. Los runs son idempotentes por
   registro/contacto, pero falta un lock (p. ej. `pg_advisory_lock`) para que
   solo una instancia programe envíos.
2. **JWKS / rutas autenticadas** — el backend del server valida JWT contra el
   default `http://localhost:3000/api/auth/jwks`, donde no corre ningún
   frontend. En failover solo funciona el path del **webhook/bot** (que es lo
   crítico); el panel autenticado no. Se resuelve al desplegar el frontend en
   el server (ajustar entonces `JWKS_URL` en `/opt/bawto/.env`).
3. **`fail_timeout=10s`** — tras un fallo del primario, nginx lo marca caído
   10 s; al reencender la PC puede tardar hasta ~10 s en volver al primario.
