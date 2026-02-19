# SCIM + SSO scaffolding

## SCIM
SCIM endpoints are scaffolded but provisioning is not enabled yet. Enable with:

```
SCIM_ENABLED=true
```

Endpoints:
- `GET /api/scim/v2/Users`
- `GET /api/scim/v2/Groups`

### Tokens
Generate a SCIM token from the Settings page or:

`POST /api/workspace/scim-tokens`

Tokens are returned once and stored hashed. Use:

`Authorization: Bearer <token>`

## SSO
OAuth providers (Google/GitHub) are auto-enabled when env vars are set. Full SAML/SCIM
provisioning is the next step; the data model includes `SsoConnection` for future wiring.
