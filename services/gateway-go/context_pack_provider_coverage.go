package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
)

const providerCoverageSuffix = ".provider-coverage.v1.json"
const providerCoverageMaxBytes = 512 * 1024

type providerCoverageRow struct {
	ClaimRef string `json:"claim_ref"`
	Mask     int64  `json:"field_mask"`
}

type providerCoverageFile struct {
	Version int                   `json:"version"`
	Rows    []providerCoverageRow `json:"rows"`
}

// Keep canonical outcomes readable by pre-coverage binaries. Presence metadata
// is observational, not new outcome authority or part of Utility claim identity.
func providerCoverageLegacyOutcome(entry map[string]any) map[string]any {
	legacy := cloneJSONMap(entry)
	delete(legacy, "provider_usage_field_mask")
	for _, field := range []string{"provider_prompt_tokens", "provider_completion_tokens", "provider_total_tokens"} {
		if value, present := legacy[field]; present {
			if count, valid := contextPackOutcomeBoundedCount(value, contextPackOutcomeMaxProviderTotalTokens); valid && count == 0 {
				delete(legacy, field)
			}
		}
	}
	return legacy
}

func contextPackOutcomeClaimsEqual(existing, candidate map[string]any) bool {
	if _, covered := existing["provider_usage_field_mask"]; !covered {
		candidate = providerCoverageLegacyOutcome(candidate)
	}
	return contextPackOutcomeLogicalClaimDigest(existing) == contextPackOutcomeLogicalClaimDigest(candidate)
}

// Coverage belongs to these exact persisted bytes, including the server receipt
// timestamp, not the lossy logical projection used to recognize legacy retries.
func providerCoverageRecordRef(entry map[string]any) string {
	raw, err := json.Marshal(providerCoverageLegacyOutcome(entry))
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(raw))
}

// All sidecar access shares the quality ledger mutex. It is bounded separately
// and ignored by old binaries, so rollback never changes canonical v1 bytes.
func (l *contextPackQualityLedger) loadProviderCoverageLocked() error {
	if l.providerCoverageLoaded {
		return l.providerCoverageError
	}
	l.providerCoverageLoaded = true
	l.providerCoverageRows = nil
	l.providerCoverageIndex = map[string]int64{}
	name := l.path + providerCoverageSuffix
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > providerCoverageMaxBytes {
		l.providerCoverageError = errors.New("provider_coverage_file_invalid")
		return l.providerCoverageError
	}
	file, err := os.Open(name)
	if err != nil {
		l.providerCoverageError = errors.New("provider_coverage_read_failed")
		return l.providerCoverageError
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		l.providerCoverageError = errors.New("provider_coverage_file_changed")
		return l.providerCoverageError
	}
	raw, err := io.ReadAll(io.LimitReader(file, providerCoverageMaxBytes+1))
	if err != nil || len(raw) > providerCoverageMaxBytes {
		l.providerCoverageError = errors.New("provider_coverage_read_failed")
		return l.providerCoverageError
	}
	var saved providerCoverageFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&saved)
	var extra any
	if err != nil || decoder.Decode(&extra) != io.EOF || saved.Version != 1 || len(saved.Rows) > 4096 {
		l.providerCoverageError = errors.New("provider_coverage_schema_invalid")
		return l.providerCoverageError
	}
	// This owner-written cache has one canonical encoding. Reject ambiguous
	// duplicate keys, null coercions and hand-edited shapes rather than guessing.
	canonical, err := json.Marshal(saved)
	var compact bytes.Buffer
	if err != nil || json.Compact(&compact, raw) != nil || !bytes.Equal(compact.Bytes(), canonical) {
		l.providerCoverageError = errors.New("provider_coverage_encoding_invalid")
		return l.providerCoverageError
	}
	for _, row := range saved.Rows {
		if !utilitySHA256DigestValid(row.ClaimRef) || row.Mask < 1 || row.Mask > 7 || l.providerCoverageIndex[row.ClaimRef] != 0 {
			l.providerCoverageIndex = nil
			l.providerCoverageError = errors.New("provider_coverage_row_invalid")
			return l.providerCoverageError
		}
		l.providerCoverageIndex[row.ClaimRef] = row.Mask
	}
	// Re-acknowledge bytes before using a sidecar whose last writer may have
	// stopped after rename but before directory sync.
	if err := l.writeProviderCoverageLocked(raw); err != nil {
		l.providerCoverageIndex = nil
		l.providerCoverageError = errors.New("provider_coverage_durability_unavailable")
		return l.providerCoverageError
	}
	l.providerCoverageRows = saved.Rows
	return nil
}

func (l *contextPackQualityLedger) saveProviderCoverageLocked(entry map[string]any) error {
	mask, valid := contextPackOutcomeBoundedCount(entry["provider_usage_field_mask"], 7)
	if !valid || mask == 0 {
		return nil
	}
	if l.providerCoverageError != nil {
		switch l.providerCoverageError.Error() {
		case "provider_coverage_read_failed", "provider_coverage_file_changed", "provider_coverage_durability_unavailable":
			// Retry one bounded read and durability acknowledgement after transient
			// I/O. Invalid schemas remain quarantined, never overwritten as repair.
			l.providerCoverageLoaded = false
			l.providerCoverageError = nil
		}
	}
	if err := l.loadProviderCoverageLocked(); err != nil {
		return err
	}
	ref := providerCoverageRecordRef(entry)
	if prior := l.providerCoverageIndex[ref]; prior != 0 && prior != mask {
		return errUtilityOutcomeConflict
	}
	rows := append([]providerCoverageRow{}, l.providerCoverageRows...)
	if l.providerCoverageIndex[ref] == 0 {
		rows = append(rows, providerCoverageRow{ClaimRef: ref, Mask: mask})
	}
	limit := clampInt(l.maxSamples, 20, 4096)
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	raw, err := json.Marshal(providerCoverageFile{Version: 1, Rows: rows})
	if err != nil || len(raw) > providerCoverageMaxBytes {
		return errors.New("provider_coverage_size_limit")
	}
	if err := l.writeProviderCoverageLocked(raw); err != nil {
		return err
	}
	l.providerCoverageRows = rows
	l.providerCoverageIndex = make(map[string]int64, len(rows))
	for _, row := range rows {
		l.providerCoverageIndex[row.ClaimRef] = row.Mask
	}
	return nil
}

func (l *contextPackQualityLedger) writeProviderCoverageLocked(raw []byte) error {
	writer := l.writeFile
	if writer == nil {
		writer = writeOwnerOnlyDurableAtomicFile
	}
	if err := writer(l.path+providerCoverageSuffix, raw, false); err != nil {
		l.providerCoverageError = errors.New("provider_coverage_durability_unavailable")
		return l.providerCoverageError
	}
	return nil
}

func providerCoverageOperationalStatus(l *contextPackQualityLedger) map[string]any {
	status := map[string]any{"enabled": false, "loaded": false, "healthy": false, "rows": 0, "max_bytes": providerCoverageMaxBytes, "error_code": ""}
	if l == nil {
		return status
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	status["enabled"] = l.enabled
	status["loaded"] = l.providerCoverageLoaded
	status["healthy"] = l.enabled && l.providerCoverageLoaded && l.providerCoverageError == nil
	status["rows"] = len(l.providerCoverageRows)
	if l.providerCoverageError != nil {
		status["error_code"] = l.providerCoverageError.Error()
	}
	return status
}

func (l *contextPackQualityLedger) withProviderCoverage(rows []map[string]any) []map[string]any {
	if l == nil || !l.enabled {
		return rows
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.loadProviderCoverageLocked() != nil {
		return rows
	}
	for index, row := range rows {
		if anyToString(row["schema_id"]) != contextPackQualityOutcomeSchemaID {
			continue
		}
		mask := l.providerCoverageIndex[providerCoverageRecordRef(row)]
		if mask == 0 {
			continue
		}
		covered := cloneJSONMap(row)
		covered["provider_usage_field_mask"] = mask
		for bit, field := range map[int64]string{1: "provider_prompt_tokens", 2: "provider_completion_tokens", 4: "provider_total_tokens"} {
			if mask&bit != 0 || bit == 4 {
				if _, exists := covered[field]; !exists {
					covered[field] = 0
				}
			}
		}
		if !contextPackQualityLegacyOutcomeIdentifiersUnsafe(covered) {
			rows[index] = covered
		}
	}
	return rows
}
