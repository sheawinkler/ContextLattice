package main

func requestIncludesEphemeralMemory(request map[string]any) bool {
	return anyToBool(request["include_ephemeral"]) ||
		anyToBool(request["include_ephemeral_memory"]) ||
		anyToBool(request["include_test_memory"])
}

func lifecycleSurfacesByDefault(lifecycle string) bool {
	switch normalizeMemoryLifecycle(lifecycle) {
	case "ephemeral", "test":
		return false
	default:
		return true
	}
}

func shouldSurfaceMemoryLifecycle(lifecycle string, includeEphemeral bool) bool {
	if includeEphemeral {
		return true
	}
	return lifecycleSurfacesByDefault(lifecycle)
}
