package main

import (
	"os"
	"path/filepath"
	"strings"
)

func memoryGraphCSVEnvWithFallback(primary string, fallbackEnv string, fallback string) []string {
	raw := strings.TrimSpace(os.Getenv(primary))
	if raw == "" && strings.TrimSpace(fallbackEnv) != "" {
		raw = strings.TrimSpace(os.Getenv(fallbackEnv))
	}
	if raw == "" {
		raw = fallback
	}
	return csvLowerListEnv(primary, raw)
}

func (m *memoryStore) memoryGraphArtifactExcluded(project string, fileName string, topicPath string) (bool, string) {
	if m == nil || !m.policy.graphExcludeLowValue {
		return false, ""
	}
	fileName = strings.TrimSpace(fileName)
	topicPath = normalizeTopic(topicPath)
	lowerFile := strings.ToLower(filepath.ToSlash(fileName))
	base := strings.ToLower(filepath.Base(lowerFile))

	if topicPath != "" {
		for _, prefix := range m.policy.graphExcludeTopicPrefixes {
			prefix = strings.Trim(strings.ToLower(strings.TrimSpace(prefix)), "/")
			if prefix != "" && (topicPath == prefix || strings.HasPrefix(topicPath, prefix+"/")) {
				return true, "excluded_topic_prefix:" + prefix
			}
		}
	}
	if lowerFile != "" {
		for _, pattern := range m.policy.graphExcludeFilePatterns {
			if globMatches(pattern, lowerFile) {
				return true, "excluded_file_pattern:" + pattern
			}
		}
		for _, suffix := range m.policy.graphExcludeFileSuffixes {
			suffix = strings.ToLower(strings.TrimSpace(suffix))
			if suffix != "" && strings.HasSuffix(lowerFile, suffix) {
				return true, "excluded_file_suffix:" + suffix
			}
		}
		if !strings.Contains(lowerFile, "/") && strings.HasSuffix(base, ".json") {
			for _, prefix := range m.policy.graphExcludeRootJSON {
				prefix = strings.ToLower(strings.TrimSpace(prefix))
				if prefix != "" && strings.HasPrefix(base, prefix) {
					return true, "excluded_root_json_prefix:" + prefix
				}
			}
		}
	}
	return false, ""
}

func (m *memoryStore) memoryGraphEdgeExcluded(edge memoryEdgeEntry) (bool, string) {
	if m == nil {
		return false, ""
	}
	sourceProject, sourceFile, _, _, err := canonicalMemoryID(edge.SourceID)
	if err == nil {
		topic := edge.TopicPath
		if strings.TrimSpace(topic) == "" {
			topic = deriveTopicFromFile(sourceFile)
		}
		if excluded, reason := m.memoryGraphArtifactExcluded(sourceProject, sourceFile, topic); excluded {
			return true, "source_" + reason
		}
	}
	targetProject, targetFile, _, _, err := canonicalMemoryID(edge.TargetID)
	if err == nil {
		topic := deriveTopicFromFile(targetFile)
		if excluded, reason := m.memoryGraphArtifactExcluded(targetProject, targetFile, topic); excluded {
			return true, "target_" + reason
		}
	}
	return false, ""
}
