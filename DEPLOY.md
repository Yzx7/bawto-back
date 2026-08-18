# Deploy del backend (Bawto)

Estado actual del despliegue en producción y cómo actualizarlo. Backend,
frontend y topología verificados el 2026-08-07 desplegando de verdad: el
procedimiento del frontend estaba sin documentar y hubo que reconstruirlo
leyendo los scripts del server.

**Último despliegue: 2026-08-17.** Backend `e7af2f8`, SHA256
`8c631e9d876ef56ad801042766b8e44b278ef9a033e8118da4843b8dfe57f0f4`, verificado
en los tres sitios —el binario local, el disco del servidor y
`/proc/156475/exe`—; el anterior quedó en `/opt/bawto/bawto-backend.pre-e7af2f8`
(para `57c0793`). Frontend `7caa92d` en la imagen `bawto-frontend:20260817-1`;
la anterior, `bawto-frontend:20260816-1`, sigue en el server para revertir.

**Sin migraciones**: ninguno de los commits toca `db/migrations/`, así que el
esquema no cambió y no hizo falta respaldo previo.

Trae tres cosas:

- **El id de una arista lo calcula el servidor** (`engine.NormalizeEdgeIDs`).
  Antes, un flujo escrito por MCP sin `edges[].id` validaba, se ejecutaba bien y
  el editor lo dibujaba sin una sola conexión.
- **El razonamiento del Copilot llega en vivo al panel**: `copilot/provider.go`
  pasó a `Messages.NewStreaming` y el razonamiento vuelve a estar encendido en
  los pasos de diseño; solo el paso terminal lo apaga, porque es el único que
  fuerza una tool por nombre.
- **Chat de prueba del borrador** (`POST /bots/:botId/flows/:flowId/test/turns`).
  Ejecuta el grafo abierto en el editor sin publicar ni enviar por WhatsApp. El
  agente corre de verdad y cuesta créditos; las herramientas se simulan.

Despliegue anterior, 2026-08-15: backend `57c0793`, SHA256
`919dc6389f40b6c74a5f141e06070c68a5ece1f1d2cd42a48304fca874af221f`, con la
**identidad por BSUID** (migración **030 aplicada**, respaldo previo en
`/var/lib/postgresql/backups/sacs_chatbots-pre-030-20260815.dump`) y el arreglo
de la vista previa de audiencia (`models/flow_audience_preview.go`), que
reventaba con `cannot scan NULL into *string` al encontrar un contacto con
`phone_normalized` NULL y ahora cae al BSUID (`channel_user_id`) como identidad
mostrable.

> **La 030 y el binario van juntos.** Retira `chats.contact` y
> `chats.contact_name`, así que un binario anterior falla contra el esquema
> nuevo y el nuevo falla contra el viejo. Se desplegó parando el servicio,
> migrando y arrancando ya con el binario nuevo; revertir exige restaurar el
> respaldo, no solo mover el binario.

Frontend `b8d7462` en la imagen `bawto-frontend:20260815-6` —el puerto del nodo
de entrada del editor pasa a llamarse `out`, que es como ya lo nombraban el
resto del editor y el catálogo del MCP: sin ese id, una arista escrita con
`sourceHandle:"out"` no resolvía su handle y React Flow la descartaba sin log,
dejando la Entrada suelta en el lienzo aunque el flujo corriera bien—; la imagen
anterior, `bawto-frontend:20260815-5`, sigue en el server para revertir.

Novedades recientes: la identidad por BSUID (migración 030), el servicio MCP en
`POST /mcp/flows`, la tool `dataset_query`, y la **segunda marca**
`fludix.yurirodrix.top` (migración 029, respaldo
`/var/lib/postgresql/backups/sacs_chatbots-pre-029-20260815-0154.dump`).

## Topología

```
Internet
   │ https://bawto.sistemuino.com
   ▼
nginx @ yuriser-contabo (80.190.72.130, SSH puerto 22)
   │
   ├─ bawto.sistemuino.com  (bawto.conf)
   │    ├─ /webhook/whatsapp (exacto) → bawto_backend
   │    │      ├─ primario: 10.12.12.2:3009 (PC por WireGuard, APP_ENV=dev)
   │    │      └─ backup:   127.0.0.1:3009 (server, APP_ENV=prod)
   │    ├─ /mcp/  → 127.0.0.1:3009 directo, SIN failover
   │    └─ resto  → Next.js 127.0.0.1:3010 (Docker)
   │                 ├─ Better Auth + JWKS
   │                 └─ /api/* → backend local 127.0.0.1:3009
   │
   └─ fludix.yurirodrix.top  (fludix.conf) — segunda marca
        ├─ /mcp/  → 127.0.0.1:3009
        └─ resto  → el MISMO contenedor Next.js
             (sin webhook: pertenece al dominio real)

Postgres 16 vive en el mismo server y escucha en 127.0.0.1 y 10.12.12.1.
```

> **Servidor migrado (2026-08-14).** El actual es **80.190.72.130**, VPN
> **10.12.12.1**, y la PC entra como **10.12.12.2**; SSH es el **puerto 22**, no
> el 22022. El anterior era `209.74.83.236` con VPN `10.11.12.x`.
>
> El `upstream bawto_backend` quedó apuntando a `10.11.12.2:3009` tras la
> migración, así que durante días **la PC no pudo ser primario** y todo lo
> atendió el backup. Corregido el 2026-08-14 a `10.12.12.2:3009` (respaldo
> `bawto.conf.bak-20260814-*`), verificado con la PC encendida: el webhook bajó
> de 3,4 s a **0,5 s** y las peticiones aparecen en `logs/whatsapp.log` de la PC.

> **Los 3 s no distinguen «mal configurado» de «PC apagada».** Windows descarta
> los paquetes al puerto cerrado en vez de rechazarlos, así que un upstream
> correcto con la PC apagada tarda exactamente lo mismo que un upstream que
> apunta a una IP inexistente. Medir el webhook solo demuestra algo **con el
> backend local encendido**; si no, hay que mirar la config y `ip addr` del
> túnel. La PC mantiene varias interfaces WireGuard a la vez (`wgYuri`
> 10.11.12.2 y `wgYuri2` 10.12.12.2 conviven), de modo que la IP vieja sigue
> existiendo localmente y tampoco delata el problema.

- El panel y sus rutas autenticadas siempre usan el backend local del server.
- Solo el webhook exacto conserva el failover PC → server.
- Con la PC apagada, el connect falla a los **3 s** y nginx reintenta en el
  backup. Aplica al POST de Meta gracias a
  `non_idempotent`; es seguro porque el procesamiento es idempotente por
  `messages.wa_id`.
- Ambas instancias usan **la misma base** (`sacs_chatbots`), así que el estado
  de chats/flujos es consistente al conmutar.

## Piezas en el server (`ssh root@10.12.12.1` · pública `80.190.72.130`)

| Pieza | Ruta | Notas |
|---|---|---|
| Binario | `/opt/bawto/bawto-backend` | Go, linux/amd64, corre como `www-data` |
| Config | `/opt/bawto/.env` | `chmod 600`; Postgres por `127.0.0.1`; bindea `127.0.0.1:3009` |
| Servicio | `/etc/systemd/system/bawto-backend.service` | `Restart=always`, enabled al boot |
| Frontend | `/opt/bawto-frontend` | releases `source-<etiqueta>/`, `.env` con `chmod 600`, contenedor `bawto-frontend` |
| Imagen frontend | consultar, no fijar aquí | `docker inspect --format '{{.Config.Image}}' bawto-frontend`. Next standalone, Node 22, límite de 512 MiB |
| nginx | `/etc/nginx/sites-available/bawto.conf` | frontend `:3010` + webhook con failover; backup `bawto.conf.bak.20260728-081909` |
| Swap | `/swapfile` | 2 GiB, persistente mediante `/etc/fstab` |

Comandos útiles en el server:

```bash
systemctl status bawto-backend        # estado
journalctl -u bawto-backend -f        # logs en vivo
curl -s http://127.0.0.1:3009/        # health local ({"ok":true,...,"env":"prod"})
docker ps --filter name=bawto-frontend # estado del panel
docker logs -f bawto-frontend          # logs del panel
curl -I http://127.0.0.1:3010/signin  # health local del frontend
nginx -t && systemctl reload nginx    # tras tocar bawto.conf
```

## Cómo actualizar el binario (redeploy)

Desde la PC (PowerShell, en `backend/`):

```powershell
$env:GOOS='linux'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
go build -trimpath -ldflags '-s -w' -o "$env:TEMP\bawto-backend" .
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED
scp -P 22 "$env:TEMP\bawto-backend" root@10.12.12.1:/opt/bawto/bawto-backend.new
ssh -p 22 root@10.12.12.1 "systemctl stop bawto-backend && cp -p /opt/bawto/bawto-backend /opt/bawto/bawto-backend.pre-<commit> && mv /opt/bawto/bawto-backend.new /opt/bawto/bawto-backend && chmod +x /opt/bawto/bawto-backend && chown www-data:www-data /opt/bawto/bawto-backend && systemctl start bawto-backend && sleep 3 && curl -s http://127.0.0.1:3009/"
```

(Se sube como `.new` y se mueve con el servicio parado porque Linux no deja
sobreescribir un binario en ejecución. Conviene dejar el anterior como
`bawto-backend.pre-<fase>` antes de mover: revertir es entonces un `mv`.)

**Comprobar el SHA256 en los tres sitios, no el health check.** Un `{"ok":true}`
dice que *algo* responde, no *qué código* corre: si el `mv` falló, si el servicio
arrancó del binario viejo o si el `scp` subió a medias, el health check sale en
200 igualmente.

```bash
sha256sum /opt/bawto/bawto-backend                                    # en disco
sha256sum /proc/$(systemctl show -p MainPID --value bawto-backend)/exe # el proceso vivo
```

Los dos tienen que coincidir con el de la PC
(`(Get-FileHash "$env:TEMP\bawto-backend" -Algorithm SHA256).Hash`). El del
proceso es el que de verdad importa: es el único que no se puede fingir.

Y mirar el arranque, porque **una credencial ausente no rompe el arranque:
degrada en silencio**. Tienen que salir **dos** líneas:

```bash
journalctl -u bawto-backend --since '-3 min' --no-pager | grep -E 'IA de texto|IA visual'
```

Si falta «IA visual habilitada», `MINIMAX_M3_API_KEY` no está en el `.env` del
server: el bot arranca sano, responde su mensaje de reserva y el agente visual no
funciona sin un solo error.

**Si el cambio toca el camino del webhook, reiniciar también la PC.** Es el
primario (§Topología), así que desplegar solo el server deja las dos instancias
con código distinto y el comportamiento depende de quién atienda cada mensaje.
Se comprueba en un segundo:

```bash
curl -s -o /dev/null -w '%{time_total}\n' https://bawto.sistemuino.com/webhook/whatsapp
```

~0,3 s significa que contesta el primario. ~3,1 s es el `proxy_connect_timeout`
al primario caído antes de la conmutación: el bot funciona, pero por el backup.

**Esto actualiza solo la instancia del server.** La PC es el *primario* del webhook
y corre su propio proceso (`go run .` en `backend/`), así que tras un redeploy las
dos instancias tienen código distinto hasta que se reinicie también la de la PC. Da
igual mientras el cambio no toque el camino del webhook —el panel siempre usa el
backend del server—, pero si lo toca hay que reiniciar las dos. Reiniciar la de la
PC es seguro en cualquier momento: nginx falla al primario en 3 s, cae al backup y
el procesamiento es idempotente por `messages.wa_id`.

Verificación pública básica:

```powershell
curl.exe -I https://bawto.sistemuino.com/signin
curl.exe -s https://bawto.sistemuino.com/api/auth/jwks
```

## Migraciones de base de datos

Las migraciones son archivos numerados en `db/migrations/`, embebidos en el
binario y aplicados por `cmd/migrate`. Cada una corre en su propia transacción y
queda registrada en `schema_migrations` con el hash de su contenido.

**No se aplican en el arranque del servidor, a propósito**: las dos instancias
del failover comparten la misma base y una de ellas es la PC de desarrollo. Si
el arranque migrara, encender la PC con código a medio hacer aplicaría DDL a
producción sin que nadie lo pidiera.

Son **flags, no subcomandos**: `-status` consulta y **sin argumentos** aplica.
`up` y `status` sueltos no existen, y el binario responde imprimiendo su ayuda,
que se parece lo bastante a una ejecución correcta como para dar por aplicada
una migración que sigue pendiente.

```bash
# En el server, con el .env de producción cargado:
cd /opt/bawto && set -a && . ./.env && set +a
./bawto-backend-migrate -status   # qué falta, sin tocar nada
./bawto-backend-migrate           # aplica lo pendiente
```

El `migrate` del server **no se actualiza solo** al desplegar el backend: son
dos binarios. Uno viejo no conoce las migraciones nuevas y las omite de
`-status` en silencio, sin marcarlas pendientes. Antes de fiarte de un `-status`
limpio, comprueba que su fecha sea posterior al commit que trajo la migración.

Desde la PC se compila igual que el binario principal, cambiando el paquete:

```powershell
$env:GOOS='linux'; $env:GOARCH='amd64'; $env:CGO_ENABLED='0'
go build -trimpath -ldflags '-s -w' -o "$env:TEMP\bawto-backend-migrate" ./cmd/migrate
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED
```

Reglas:

- **Respaldar antes** (`pg_dump`), sobre todo `bots`, `chats`, `flow_runs` y las
  tablas de datos.
- Una migración ya aplicada es **inmutable**. Si se edita su archivo, el migrador
  aborta con el hash viejo y el nuevo en el mensaje: hay que crear una migración
  nueva, no retocar la anterior.
- Las migraciones `001`–`003` llevan la directiva `-- migrate:baseline`: se
  aplicaron a mano antes de que existiera el migrador y su efecto ya está en
  `schema.sql`, así que se **registran sin ejecutarse**. La 001 fallaría si se
  reejecutara (referencia las columnas `bot_id` que ella misma elimina).
- Una migración no puede abrir su propia transacción (`BEGIN`/`COMMIT`): el
  migrador ya la envuelve y el `COMMIT` interno cerraría esa transacción. El
  migrador lo rechaza antes de ejecutar.
- Base nueva: `better-auth migrate` → `db/schema.sql` → `cmd/migrate`.

### Sincronizar el catálogo de templates

La API autenticada ofrece `POST /bots/:botId/templates/sync`. Para una operación o
despliegue sin sesión web puede usarse el comando equivalente desde una máquina con
acceso a Postgres y al `.env` del backend:

```bash
go run ./cmd/watemplates <botId> sync-catalog <wabaId>
```

Consulta todas las páginas de `/{WABA-ID}/message_templates`, actualiza
estado/categoría/calidad/componentes y marca como `DELETED` las plantillas que dejaron
de aparecer. No crea ni modifica plantillas en Meta.

### Publicar un flujo desde un archivo

El webhook ejecuta **la versión publicada** del flujo `message` de mayor
prioridad. `bots.flow` ya no existe (migración `011_drop_bots_flow`), así que no
hay copia paralela que pueda contradecirla.

```bash
./bawto-backend-migrate \
  -publish-flow-file ./waa.json \
  -bot-id <botId> \
  -flow-key flow_waa_isp \
  -author deploy
```

El comando valida con `engine.Validate`, normaliza, es idempotente por checksum y
relee la versión publicada de la base para comprobar que coincide con el archivo.

Un bot sin flujo `message` publicado no ejecuta nada: el webhook responde con el
eco. Pausar o archivar un flujo tiene ese efecto a propósito.

## Rollback de la migración multiflujo

Los flags están separados (PLAN §17) porque un flag único obligaría a apagar los
recordatorios para revertir el dispatcher, que es lo contrario de lo que se
querría en un incidente. Se ponen en `/opt/bawto/.env` y requieren
`systemctl restart bawto-backend`.

| Flag | Default | Qué apaga | Efecto al desactivar |
|---|---|---|---|
| `SCHEDULER_ENABLED` | `true` | Cron + worker de entrega | Deja de encolar y entregar; no borra runs |
| `MULTI_FLOW_DISPATCH_ENABLED` | `false` | Sesiones y selección de flujo | Vuelve a `chats.current_layer` (fase 7; hoy no lo lee nadie) |

`FLOWS_TABLE_ENABLED` **ya no existe**: la lectura desde `flows` dejó de ser
opcional al eliminarse `bots.flow`, y un flag cuyo apagado no tiene destino solo
sirve para confundir. El rollback de un flujo es restaurar su versión anterior
(`POST /bots/:botId/flows/:flowId/versions/:versionId/restore` + publicar), que es
más preciso que revertir el camino de lectura entero.

Los parámetros opcionales del scheduler son `SCHEDULER_CATCHUP_WINDOW=2h`,
`SCHEDULER_LOCK_TIMEOUT=10m`, `SCHEDULER_CHAT_POSTPONE=2h` y
`SCHEDULER_WABA_MPS=5`. La correlación sin cita usa
`REMINDER_CORRELATION_WINDOW=72h`.

Si hay que frenar los envíos programados: `SCHEDULER_ENABLED=false`. El webhook
entrante **sigue atendiendo**; solo se detiene el cron y la entrega saliente.

Qué **no** hay que borrar en un rollback:

- Las tablas `flows` y `flow_versions` ni sus filas. Son la **única** copia del
  grafo desde la migración `011`: borrarlas deja a los bots sin flujo y destruye
  el historial de versiones publicadas.
- Los `flow_runs`. Si alguno no debe entregarse, se marca `cancelled`; no se
  borra.
- El registro `schema_migrations`. Borrar una fila haría que el migrador intente
  reaplicar esa migración.

No existe un `down` por migración: las de esta fase son aditivas y el rollback
real es el flag. Revertir el esquema exigiría restaurar el respaldo.

## Diferencias del `.env` de producción vs dev

| Variable | Server (prod) | PC (dev) |
|---|---|---|
| `APP_ENV` | `prod` | `dev` |
| `SERVER_PORT` | `127.0.0.1:3009` (solo loopback; nginx es el frontdoor) | `:3009` |
| `DATABASE_URL` | host `127.0.0.1` (Postgres local) | host `10.12.12.1` (vía WireGuard) |
| `JWKS_URL` | `http://127.0.0.1:3010/api/auth/jwks` | frontend dev en `localhost:3000` |
| `CORS_ORIGINS` | `https://bawto.sistemuino.com` | origen local de desarrollo |

El resto (claves de cifrado, secretos de Meta, IA) es idéntico.

> Nota: `SERVER_PORT` acepta forma `host:puerto` desde el fix de
> `config/config.go` (commit `fa19769`); binarios anteriores solo aceptaban
> `:puerto`.

## Verificar el frontend: `next build` NO reproduce el contenedor

Aprendido rompiendo producción el 2026-08-15. La portada devolvía **500 en las
dos marcas** con un build que había pasado `tsc`, `eslint`, `next build` y
pruebas con `next start` en dos dominios.

Hacen falta **dos cosas a la vez** para que el fallo aparezca: el binario
**standalone** —que es lo que corre en el contenedor, no `next start`— y la
cabecera `X-Forwarded-Proto: https` que pone nginx. Con ambas, `nextUrl` mezcla
el esquema `https` con el host interno `localhost:3010`; ese par no existe, así
que un `NextResponse.rewrite` deja de ser interno, Next intenta salir a la red y
muere con `EPROTO: wrong version number`.

Antes de desplegar algo que dependa del proxy o del dominio:

```bash
cd frontend && pnpm exec next build
cp -r .next/static .next/standalone/.next/ && cp -r public .next/standalone/
cd .next/standalone && PORT=3315 HOSTNAME=127.0.0.1 node server.js
# y probar con la cabecera que pone nginx:
curl -s -o /dev/null -w '%{http_code}\n' \
  -H 'Host: fludix.yurirodrix.top' -H 'X-Forwarded-Proto: https' \
  http://127.0.0.1:3315/
```

La portada se elige con `rewrites` de `next.config.ts`, no desde `proxy.ts`:
actúan en el enrutado y no construyen ninguna URL absoluta, así que el problema
no puede darse.

## La sesión se resuelve por dominio

`lib/auth.ts` pasa `baseURL` como **objeto** con `allowedHosts`, no como cadena.
Es lo que activa la resolución por petición y hace que cada marca conserve su
dominio al iniciar sesión; con una cadena, quien entra por Fludix acaba en
Bawto.

Dos trampas comprobadas: **quitar `baseURL` no basta**, porque Better Auth lee
`BETTER_AUTH_URL` del entorno por su cuenta; y sin ninguna de las dos cae al
host del socket (`localhost:3010`). El `fallback` existe para que un host que no
resuelva acabe en el dominio real en vez de dejar a nadie sin entrar.

**Un dominio nuevo necesita dos registros, no uno**: entrar en `allowedHosts` y
darse de alta como URI de redirección autorizada en Google Cloud Console
(`https://<dominio>/api/auth/callback/google`). Sin lo segundo, Google rechaza
con `redirect_uri_mismatch`.

Las cookies se emiten por dominio y `bawto.sistemuino.com` y
`fludix.yurirodrix.top` no comparten sufijo: **cada marca tiene su sesión**.

## Certificados: el plugin de certbot tiene que estar instalado

`python3-certbot-nginx` **no estaba** el 2026-08-15, y los certificados
existentes se habían emitido con `authenticator = nginx`. Es decir, las
renovaciones venían fallando en silencio: `bibunmsm.lat` caducó el 2026-07-04 y
`bawto.sistemuino.com` habría perdido HTTPS en su siguiente renovación,
llevándose por delante el panel y el webhook.

```bash
certbot certificates | grep -E 'Certificate Name|Expiry'   # ninguno INVALID
certbot plugins | grep nginx                               # tiene que aparecer
```

Un dominio nuevo se añade con
`certbot --nginx -d <dominio> --redirect`, que escribe el bloque TLS en su
`.conf` y recarga nginx.

## Rotar secretos (hecho el 2026-08-17)

Se rotaron la **contraseña de PostgreSQL**, el **secreto de Better Auth** y el
**Google Client Secret**. `TOKEN_ENC_KEY` no se toca: los tokens de canal están
cifrados con ella y cambiarla sin redescifrar deja los bots sin poder enviar.

El orden importa, y no es el intuitivo:

1. **Respaldar** los cuatro `.env` (`backend/.env` y `frontend/.env.local` en la
   PC; `/opt/bawto/.env` y `/opt/bawto-frontend/.env` en el server).
2. **Escribir los cuatro `.env` con los valores nuevos primero**, con los
   servicios aún corriendo. Siguen vivos con sus conexiones ya abiertas: un
   `ALTER ROLE` solo afecta a conexiones **nuevas**. Hacerlo al revés alarga el
   corte a todo el tiempo que se tarde en editar ficheros.
3. **Rotar en PostgreSQL** y **borrar `jwks`** en la misma pasada.
4. **Reiniciar**: `systemctl restart bawto-backend` y recrear el contenedor. El
   contenedor usa `--env-file`, así que **no hace falta reconstruir la imagen**:
   ni `BETTER_AUTH_SECRET` ni `GOOGLE_CLIENT_SECRET` son `NEXT_PUBLIC_*`.

```bash
docker rm -f bawto-frontend
docker run -d --name bawto-frontend --network host \
  --env-file /opt/bawto-frontend/.env --restart always --memory 512m \
  bawto-frontend:<etiqueta-actual>
```

Cuatro cosas que cuesta descubrir a base de romperlas:

- **Borrar la fila de `jwks` es obligatorio.** Better Auth guarda ahí la clave
  privada cifrada con su secreto y la descifra al firmar
  (`plugins/jwt/sign.mjs:34`). Con el secreto nuevo y la fila vieja, **todo
  login** muere con `Failed to decrypt private key`. Vacía, se regenera sola al
  primer `/api/auth/jwks`.
- **No mandes la contraseña en claro a PostgreSQL.** Calcula el verificador
  SCRAM-SHA-256 en el cliente y pásale eso al `ALTER ROLE`. Con `log_statement`
  activo, un `ALTER ROLE ... PASSWORD '<texto>'` escribe la credencial nueva en
  el log del servidor: la misma fuga que la rotación viene a cerrar. El rol no
  necesita ser superusuario para cambiar su propia clave.
- **Verifica que la clave VIEJA ya no conecta.** Que la nueva funcione no prueba
  que la vieja dejara de hacerlo, y esa es la única pregunta que importa en una
  rotación.
- **Comprueba a qué servidor estás rotando con `inet_server_addr()`.** El
  Postgres viejo (`10.11.12.1`) sigue vivo y responde con una copia
  desactualizada sin fallar; tenía **la misma contraseña**. Elegir por el host
  del `.env` no basta.

Better Auth 1.6.23 admite rotación **sin cortar sesiones**: `secrets` versionado
(envoltorio `$ba$<v>$`) más `legacySecret` para lo ya cifrado, en
`context/secret-utils.mjs`. En dev se rotó en duro porque las sesiones no
importaban; con clientes reales, ese es el camino.

El backend no necesita nada especial: `middlewares/auth/auth.go:81` ya reintenta
con refresh forzado del JWKS cuando la validación falla, que es exactamente el
caso de una llave rotada.

**Lo que la rotación NO arregla sola:** si se genera un *Client ID* nuevo de
Google —y no solo un secreto—, el cliente nace **sin URIs de redirección**. Hay
que darlas de alta las tres (`bawto.sistemuino.com`, `fludix.yurirodrix.top` y
`localhost:3000`, todas con `/api/auth/callback/google`) o Google rechaza con
`redirect_uri_mismatch`. Y como `lib/auth.ts` fija
`emailAndPassword: { enabled: false }`, **no hay puerta trasera**: si Google
queda mal, no entra nadie.

## Caveats conocidos

1. **`fail_timeout=10s`** — tras un fallo del primario, nginx lo marca caído
   10 s; al reencender la PC puede tardar hasta ~10 s en volver al primario.

El scheduler ya no es un caveat: ambas instancias pueden ejecutar workers de
entrega con `FOR UPDATE SKIP LOCKED`, mientras una conexión dedicada mantiene el
advisory lock de descubrimiento. `run_key` único sigue siendo la garantía dura.

## Frontend: release y actualización

El contenedor publica solo en loopback mediante `--network host`; nginx es el
único frontdoor. El `.env` de producción usa:

- `BETTER_AUTH_URL=https://bawto.sistemuino.com`
- `GO_API_URL=http://127.0.0.1:3009`
- Postgres local (`127.0.0.1`)

No reutilizar el puerto `3000`: pertenece al servicio Vox en este servidor.

### El mecanismo

El frontend **no se construye en la PC**: se sube el código y la imagen se
construye en el server. Cada release vive en `/opt/bawto-frontend` con tres
piezas que comparten etiqueta `<AAAAMMDD>-<n>`:

| Pieza | Qué es |
|---|---|
| `source-<etiqueta>.tar.gz` | el código subido |
| `source-<etiqueta>/` | el código extraído (a esto apunta el symlink `current`) |
| `deploy-<etiqueta>.sh` | el script que construye y conmuta |
| `deploy-<etiqueta>.log` | la salida de `docker build` |

El script hace, en este orden: aborta si la etiqueta ya existe —para no pisar un
release—, extrae el tarball, carga el `.env` del server, construye la imagen,
para y recrea el contenedor, y sondea `http://127.0.0.1:3010/signin` durante 30 s.
Si no queda sano, **vuelve solo** a la imagen anterior, que capturó al empezar con
`docker inspect`. Imprime `FRONTEND_HEALTHY` o `FRONTEND_ROLLED_BACK`.

La construcción debe pasar `--build-arg NEXT_PUBLIC_APP_VERSION="$release"`.
Ese valor aparece en el sidebar y el footer público, así que el frontend visible
identifica la imagen activa sin depender de una constante editada a mano.

### Cómo publicar un release

Desde la PC, en `frontend/`. El tarball se genera con `git archive HEAD` **a
propósito**: así solo viaja lo commiteado, y no el árbol de trabajo con cambios a
medias. Es la misma regla que para el binario del backend.

```bash
REL=$(date +%Y%m%d)-1        # sube el -n si ya publicaste hoy
git archive --format=tar.gz -o /tmp/source-$REL.tar.gz HEAD
scp -P 22 /tmp/source-$REL.tar.gz root@10.12.12.1:/opt/bawto-frontend/
```

En el server, el script nuevo sale del anterior cambiando una línea; no hay que
escribirlo a mano:

```bash
cd /opt/bawto-frontend
ANTERIOR=deploy-20260807-1.sh          # el último que se usó
sed "s/^release=.*$/release=$REL/" $ANTERIOR > deploy-$REL.sh
chmod +x deploy-$REL.sh
diff $ANTERIOR deploy-$REL.sh          # debe diferir SOLO en la línea release=
./deploy-$REL.sh
```

Verificación pública:

```powershell
curl.exe -I https://bawto.sistemuino.com/signin
curl.exe -s https://bawto.sistemuino.com/api/auth/jwks
```
