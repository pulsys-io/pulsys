# OIDC / SSO setup

The Pulsys admin console authenticates operators with **OpenID Connect**. Any
standards-compliant IdP works; this guide has step-by-step setup for the three we
test against:

| IdP | Best for |
|-----|----------|
| [Keycloak](#keycloak-production) | self-hosted environments, full control |
| [Amazon Cognito](#amazon-cognito) | quick start on AWS; what the EKS e2e uses |
| [AWS IAM Identity Center](#aws-iam-identity-center) | enterprise SSO across an AWS org |

> **The bundled dev realm is intentional.** For local development,
> `docker compose up` brings up a Keycloak with the `pulsys` realm and an
> `admin@pulsys.local / admin` user (`docker/keycloak/pulsys-realm.json`). That is
> a deliberate zero-config feature for the first run; never use it in production.
> The per-IdP sections below cover the hardened production path.

## How Pulsys consumes OIDC

OIDC configuration is stored per-tenant in the database, not in environment
variables. Write it once with the `pulsys-db` CLI (or let the Helm init Job do
it):

```bash
pulsys-db oidc configure \
  --tenant default \
  --issuer https://<idp>/realms/pulsys \
  --client-id pulsys-admin \
  --client-secret "$OIDC_CLIENT_SECRET" \
  --redirect-uri https://hf.example.com/auth/oidc/callback \
  --owner-groups pulsys:owner
```

At runtime the proxy only needs `PULSYS_OIDC_DISCOVERY_BASE` when the IdP is
reachable at a different (in-cluster) URL than the browser-facing issuer; the
`iss` claim is always validated against the configured issuer.

The Helm chart exposes these as first-class `oidc.*` values and runs the
`pulsys-db oidc configure` step in a pre-install/pre-upgrade Job:

```yaml
admin:
  enabled: true
oidc:
  enabled: true
  issuer: https://<idp>/realms/pulsys
  redirectURI: https://hf.example.com/auth/oidc/callback
  clientID: pulsys-admin
  existingSecret: pulsys-oidc       # Secret with key: client-secret
  ownerGroups: "pulsys:owner"
  # discoveryBase: http://keycloak.svc.cluster.local:8080/realms/pulsys
```

See [`deploy/charts/pulsys/README.md`](../deploy/charts/pulsys/README.md).

## Group → role mapping

Pulsys maps IdP **group** membership to tenant roles. Configure your IdP to emit
a `groups` claim (array of group names) in the ID token, and pass the owner
group(s) via `--owner-groups` / `oidc.ownerGroups`. Members of an owner group get
the tenant **owner** role on first login.

---

## Keycloak (production)

Stands up a hardened Keycloak realm. The realm template is
[`deploy/keycloak/pulsys-realm-prod.json`](../deploy/keycloak/pulsys-realm-prod.json),
which differs from the dev realm in every way that matters: no bootstrap user or
admin password, `sslRequired: external`, brute-force protection, short
access-token lifespan with refresh rotation, a strong password policy, a
**confidential** `pulsys-admin` client with **PKCE (S256)**, and a `groups`
protocol mapper.

**Prerequisites:** a Keycloak 26.x instance over HTTPS, `kcadm.sh` or the Admin
Console, and a DNS name + TLS cert for the console (e.g. `hf.example.com`).

**1. Prepare the realm file** (replace every `REPLACE_ME`):

```bash
cp deploy/keycloak/pulsys-realm-prod.json /tmp/pulsys-realm-prod.json
sed -i 's#REPLACE_ME_PULSYS_HOST#hf.example.com#g' /tmp/pulsys-realm-prod.json
SECRET="$(openssl rand -hex 32)"
sed -i "s#REPLACE_ME_WITH_A_GENERATED_SECRET#${SECRET}#g" /tmp/pulsys-realm-prod.json
echo "client secret: ${SECRET}   # store in your secret manager"
```

**2. Import the realm** (on startup with `kc.sh start --optimized --import-realm`,
or into a running server):

```bash
kcadm.sh config credentials --server https://keycloak.example.com \
  --realm master --user "$KC_ADMIN" --password "$KC_ADMIN_PASSWORD"
kcadm.sh create realms -f /tmp/pulsys-realm-prod.json
```

Never commit the substituted file: it now contains the client secret.

**3. Create users / federate.** The production realm ships no users. Either
federate from your corporate directory (LDAP / AD / another OIDC IdP) or create
users in the Admin Console and add them to `pulsys:owner`, `pulsys:admin`, or
`pulsys:viewer`. Membership in `pulsys:owner` grants the tenant owner role on
first login.

**4. Configure Pulsys.** Store the client secret in a Kubernetes Secret and let
the chart configure the database:

```bash
kubectl create secret generic pulsys-oidc --from-literal=client-secret="$SECRET"

helm upgrade --install pulsys deploy/charts/pulsys \
  --set proxy.publicBaseURL=https://hf.example.com \
  --set admin.enabled=true \
  --set postgres.existingSecret=pulsys-pg-app --set postgres.existingSecretKey=uri \
  --set oidc.enabled=true \
  --set oidc.issuer=https://keycloak.example.com/realms/pulsys \
  --set oidc.redirectURI=https://hf.example.com/auth/oidc/callback \
  --set oidc.clientID=pulsys-admin \
  --set oidc.existingSecret=pulsys-oidc \
  --set oidc.ownerGroups=pulsys:owner
```

**Hardening checklist:** Keycloak behind TLS (`sslRequired=external` or `all`);
Admin Console not public or IP-restricted; client secret in a secret manager;
brute-force protection on; token lifespans reviewed; Keycloak database backed up.

---

## Amazon Cognito

Fastest to stand up on AWS. This section covers wiring Cognito to the Helm chart
by hand (user pool, app client, and chart values).

**1. Create a user pool + app client:**

```bash
REGION=us-east-2
DOMAIN_HOST=hf.example.com

POOL_ID=$(aws cognito-idp create-user-pool --region "$REGION" --pool-name pulsys \
  --auto-verified-attributes email --query 'UserPool.Id' --output text)

# Confidential app client, authorization-code flow + PKCE:
read CLIENT_ID CLIENT_SECRET < <(aws cognito-idp create-user-pool-client \
  --region "$REGION" --user-pool-id "$POOL_ID" --client-name pulsys-admin \
  --generate-secret --allowed-o-auth-flows code \
  --allowed-o-auth-scopes openid email profile \
  --allowed-o-auth-flows-user-pool-client --supported-identity-providers COGNITO \
  --callback-urls "https://${DOMAIN_HOST}/auth/oidc/callback" \
  --query 'UserPoolClient.[ClientId,ClientSecret]' --output text)

# Hosted-UI domain (required so the issuer serves discovery):
aws cognito-idp create-user-pool-domain --region "$REGION" \
  --user-pool-id "$POOL_ID" --domain "pulsys-${POOL_ID##*_}"
```

The **issuer** is `https://cognito-idp.${REGION}.amazonaws.com/${POOL_ID}`; verify
discovery resolves with
`curl -s "https://cognito-idp.${REGION}.amazonaws.com/${POOL_ID}/.well-known/openid-configuration" | jq .issuer`.

**2. Groups → the `groups` claim.** Cognito emits membership in `cognito:groups`,
but Pulsys reads `groups`. The simplest path is to name the Cognito group exactly
`pulsys:owner` and add a pre-token-generation Lambda that copies `cognito:groups`
to `groups` (the CDK reference deploy installs this Lambda for you):

```bash
aws cognito-idp create-group --region "$REGION" --user-pool-id "$POOL_ID" --group-name "pulsys:owner"
aws cognito-idp admin-add-user-to-group --region "$REGION" \
  --user-pool-id "$POOL_ID" --username "$USER_EMAIL" --group-name "pulsys:owner"
```

**3. Configure Pulsys:**

```bash
kubectl create secret generic pulsys-oidc --from-literal=client-secret="$CLIENT_SECRET"

helm upgrade --install pulsys deploy/charts/pulsys \
  --set proxy.publicBaseURL="https://${DOMAIN_HOST}" \
  --set admin.enabled=true \
  --set postgres.existingSecret=pulsys-pg-app --set postgres.existingSecretKey=uri \
  --set oidc.enabled=true \
  --set oidc.issuer="https://cognito-idp.${REGION}.amazonaws.com/${POOL_ID}" \
  --set oidc.redirectURI="https://${DOMAIN_HOST}/auth/oidc/callback" \
  --set oidc.clientID="$CLIENT_ID" \
  --set oidc.existingSecret=pulsys-oidc \
  --set oidc.ownerGroups=pulsys:owner
```

---

## AWS IAM Identity Center

IAM Identity Center (successor to AWS SSO) is the enterprise option for an AWS
Organization. It exposes an OIDC-compatible interface to a registered
application. One step cannot be fully automated (assignment, below).

**1. Create a custom OAuth 2.0 application.** In the IAM Identity Center console:
Applications → Add application → "I have an application I want to set up
(custom)" → OAuth 2.0. Set the redirect URL to
`https://hf.example.com/auth/oidc/callback` and note the issuer, client ID, and
client secret. The issuer looks like
`https://identitycenter.amazonaws.com/ssoins-xxxxxxxxxxxx` (use the value the
console/discovery shows; do not hardcode it).

**2. Assign users/groups (MANUAL).** Group/user assignment to a custom-OAuth
application has no complete public API. In the console: open the application →
Assigned users and groups → assign your operator group (e.g. `pulsys-owner`);
under attribute mappings, map the group attribute into a `groups` claim. The CDK
reference deploy creates the application and prints the exact assignment
instructions rather than faking the assignment.

**3. Configure Pulsys:**

```bash
kubectl create secret generic pulsys-oidc --from-literal=client-secret="$IDC_CLIENT_SECRET"

helm upgrade --install pulsys deploy/charts/pulsys \
  --set proxy.publicBaseURL=https://hf.example.com \
  --set admin.enabled=true \
  --set postgres.existingSecret=pulsys-pg-app --set postgres.existingSecretKey=uri \
  --set oidc.enabled=true \
  --set oidc.issuer="$IDC_ISSUER" \
  --set oidc.redirectURI=https://hf.example.com/auth/oidc/callback \
  --set oidc.clientID="$IDC_CLIENT_ID" \
  --set oidc.existingSecret=pulsys-oidc \
  --set oidc.ownerGroups=pulsys-owner
```

---

## Break-glass: recovering owner access

A misconfigured IdP can lock everyone out: a wrong `groups` claim, a typo in
`--owner-groups`, or a rotated client secret can leave a tenant with no one who
can sign in as owner. Because Pulsys has no open mode and no local password, the
recovery path is database access, not a backdoor in the data plane.

`pulsys-db user grant-owner` promotes an existing tenant user to the owner role
and reactivates them, matching by email or OIDC subject, and writes a `system` /
`user.grant_owner` row to `audit_log` in the same transaction:

```bash
pulsys-db user grant-owner --tenant default --email alice@example.com
pulsys-db user grant-owner --tenant default --oidc-sub 6f1c...e2   # if email is ambiguous
```

- Run it from an operator host with `PULSYS_DB_DSN` (or `-dsn`) pointing at the
  admin database; it needs no access to the data plane.
- The user must already exist (signed in at least once, or pre-provisioned);
  `grant-owner` does not create accounts.
- Review the new audit row afterwards (`GET /admin/api/v1/audit` or query
  `audit_log`), then fix the underlying IdP/group configuration so JIT owner
  mapping works again.

## Verify

Browse to `https://hf.example.com`, sign in, complete the IdP login, and confirm
you land in the admin console with owner permissions. Check the audit log for the
login event.
