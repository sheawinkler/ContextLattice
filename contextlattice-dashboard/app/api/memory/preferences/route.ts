import { fetchOrchestrator } from "@/lib/orchestrator";
import { NextRequest, NextResponse } from "next/server";

export async function GET(req: NextRequest) {
  const { searchParams } = new URL(req.url);
  const project = searchParams.get("project");
  const userId = searchParams.get("user_id");
  const limit = searchParams.get("limit");

  const params = new URLSearchParams();
  if (project) params.set("project", project);
  if (userId) params.set("user_id", userId);
  if (limit) params.set("limit", limit);

  const path = params.toString() ? `/preferences?${params.toString()}` : "/preferences";
  const fallback = {
    enabled: false,
    preferences: {
      total: 0,
      positive: [],
      negative: [],
      notes: [],
      updated_at: null,
    },
    reason: "go_runtime_preferences_not_enabled",
  };

  try {
    const response = await fetchOrchestrator(path, { method: "GET" });
    if (!response.ok) {
      return NextResponse.json(fallback);
    }
    const data = await response.json();
    return NextResponse.json(data);
  } catch {
    return NextResponse.json(fallback);
  }
}
