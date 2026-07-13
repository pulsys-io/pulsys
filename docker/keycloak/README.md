# Keycloak dev realm (LOCAL DEVELOPMENT ONLY)

`pulsys-realm.json` is a **frictionless local development** realm. It exists so
you can run the admin plane end-to-end on your laptop without configuring an
identity provider.

> [!WARNING]
> This realm ships an **insecure default user** (`admin@pulsys.local` /
> `admin`), a public client, and `localhost` redirect URIs. It is for local
> development only. **Never import it into a production Keycloak.**

For production OIDC, use the hardened realm and provider guides:

- `deploy/keycloak/` - hardened production Keycloak realm (no default users).
- `docs/oidc.md` - production Keycloak setup, Amazon Cognito, and AWS IAM Identity Center.

## Usage

The realm is imported automatically when you bring up the local admin-plane
stack (see `DEVELOPMENT.md`). To import manually:

```bash
# inside a dev Keycloak container
/opt/keycloak/bin/kc.sh import --file /realms/pulsys-realm.json
```

Then sign in to the admin console at `http://localhost:3000` with
`admin@pulsys.local` / `admin`.
