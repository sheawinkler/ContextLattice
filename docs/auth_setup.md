# Auth setup (NextAuth + Prisma)

## 1) Install deps
The dashboard already pins the dependencies in `memmcp-dashboard/package.json`.

## 2) Configure env
Copy and edit:

```bash
cp memmcp-dashboard/.env.example memmcp-dashboard/.env
```

Set at minimum:
- `DATABASE_URL`
- `NEXTAUTH_SECRET`
- `NEXTAUTH_URL`

## 3) Initialize database

```bash
cd memmcp-dashboard
npm run db:generate
npm run db:push
```

## 4) Run locally

```bash
npm run dev
```

## Notes
- `AUTH_REQUIRED=false` keeps the dashboard open for local use.
- Set `AUTH_REQUIRED=true` in production to require login.
- Optional OAuth providers (Google/GitHub) activate automatically when their env vars are set.
