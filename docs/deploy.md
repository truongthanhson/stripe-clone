# Deploy

One Linux VM with Docker runs the whole stack from `compose.prod.yaml`:

| service    | what it does                                                                 |
|------------|------------------------------------------------------------------------------|
| `postgres` | Postgres 17, data in the `pgdata` volume, not published to the host          |
| `migrate`  | one-shot: runs `migrate up` against the embedded migrations, then exits      |
| `api`      | the HTTP API on :8080 inside the network; starts only after `migrate` succeeds |
| `caddy`    | public :80/:443, gets and renews the TLS certificate, proxies to `api`       |

## First deploy

1. Point a DNS A/AAAA record for your domain at the VM and open ports 80 and 443.
2. On the VM:

   ```sh
   git clone https://github.com/truongthanhson/stripe-clone.git /srv/stripe-clone
   cd /srv/stripe-clone
   cp .env.example .env
   $EDITOR .env        # set DOMAIN and a long random POSTGRES_PASSWORD (openssl rand -hex 24)
   docker compose -f compose.prod.yaml --env-file .env up -d --build
   curl https://$DOMAIN/healthz   # {"database":"up","status":"ok"}
   ```

3. Create a merchant and a key with the admin tool from the same image:

   ```sh
   docker compose -f compose.prod.yaml run --rm --no-deps --entrypoint admin api create-merchant -name Acme -email ops@acme.test
   docker compose -f compose.prod.yaml run --rm --no-deps --entrypoint admin api issue-key -merchant 1 -mode live
   ```

## Update

```sh
git pull
docker compose -f compose.prod.yaml --env-file .env up -d --build
```

`migrate` runs again first; `api` is replaced only if it exits 0. Migrations must stay backward compatible with the
running version for the few seconds between the two.

## Backups

`scripts/backup.sh` writes a `pg_dump --format=custom` file to `BACKUP_DIR` and deletes dumps older than
`BACKUP_RETENTION_DAYS`. Install it nightly:

```sh
crontab -e
0 3 * * * cd /srv/stripe-clone && scripts/backup.sh >> backups/backup.log 2>&1
```

A backup on the same disk is not a backup: copy `BACKUP_DIR` off the machine (rclone, restic, or `scp` from another box).

### Restore

Test into a scratch database first:

```sh
C="docker compose -f compose.prod.yaml --env-file .env"
$C exec -T postgres createdb -U stripe restore_check
$C exec -T postgres pg_restore -U stripe -d restore_check --no-owner < backups/stripe-YYYYMMDDTHHMMSSZ.dump
$C exec -T postgres psql -U stripe -d restore_check -c 'select count(*) from merchants'
```

For a real restore, stop `api`, restore into `stripe` with `pg_restore --clean --if-exists`, then start `api` again.

## Try it locally

Set `DOMAIN=localhost` in `.env`. Caddy then issues a certificate from its own local CA, so use `curl -k`:

```sh
docker compose -f compose.prod.yaml --env-file .env up -d --build
curl -k https://localhost/healthz
```

The production stack uses its own compose project name (`stripe-clone-prod`), so it does not touch the dev Postgres from
`compose.yaml`.
