export function dashboardAuthRequired(): boolean {
  return process.env.AUTH_REQUIRED === "true";
}

export function dashboardClientAuthRequired(): boolean {
  return process.env.NEXT_PUBLIC_AUTH_REQUIRED === "true";
}

export function authDisabledPayload() {
  return {
    ok: false,
    error: "auth_disabled",
    authRequired: false,
  };
}

export function authDisabledResponse(status = 404): Response {
  return Response.json(authDisabledPayload(), {
    status,
    headers: {
      "cache-control": "no-store",
    },
  });
}
