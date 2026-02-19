import { authenticateScimToken, extractScimToken } from "@/lib/auth/scim";

const SCIM_JSON = "application/scim+json";

function scimResponse(body: any, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": SCIM_JSON },
  });
}

export async function GET(request: Request) {
  if (process.env.SCIM_ENABLED !== "true") {
    return scimResponse({ detail: "SCIM is disabled." }, 503);
  }
  const token = extractScimToken(request);
  if (!token) {
    return scimResponse({ detail: "Missing SCIM token" }, 401);
  }
  const record = await authenticateScimToken(token);
  if (!record) {
    return scimResponse({ detail: "Invalid SCIM token" }, 401);
  }

  return scimResponse({
    schemas: ["urn:ietf:params:scim:api:messages:2.0:ListResponse"],
    totalResults: 0,
    itemsPerPage: 0,
    startIndex: 1,
    Resources: [],
    note: "SCIM Users endpoint scaffold; provisioning not yet enabled.",
  });
}

export async function POST() {
  if (process.env.SCIM_ENABLED !== "true") {
    return scimResponse({ detail: "SCIM is disabled." }, 503);
  }
  return scimResponse(
    { detail: "SCIM provisioning not enabled yet." },
    501,
  );
}
