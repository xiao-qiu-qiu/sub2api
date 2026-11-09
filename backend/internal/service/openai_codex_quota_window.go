package service

import "math"

// Canonical snapshots without a duration predate window metadata and remain
// readable. An explicitly invalid duration must block raw fallback as well:
// old writers could persist the same image quota in both sets of fields.
func openAICodexCanonicalWindowValid(extra map[string]any, window string) bool {
	var expected float64
	switch window {
	case "5h":
		expected = 300
	case "7d":
		expected = 10080
	default:
		return false
	}
	key := "codex_" + window + "_window_minutes"
	if _, exists := extra[key]; !exists {
		return true
	}
	minutes, ok := resolveAccountExtraNumber(extra, key)
	return ok && minutes == expected
}

// extractOpenAICodexUsageWindow pairs a percentage with its source fields so
// callers also read the matching reset. Named canonical fields take precedence;
// raw-only historical snapshots use the same classification as new writes.
func extractOpenAICodexUsageWindow(extra map[string]any, window string) (usedPercent float64, source string, ok bool) {
	if !openAICodexCanonicalWindowValid(extra, window) {
		return 0, "", false
	}
	usedKey := "codex_" + window + "_used_percent"
	if _, exists := extra[usedKey]; exists {
		used, valid := resolveAccountExtraNumber(extra, usedKey)
		return used, window, valid && !math.IsNaN(used) && !math.IsInf(used, 0)
	}

	snapshot := &OpenAICodexUsageSnapshot{
		PrimaryWindowMinutes:   openAICodexExtraWindowMinutes(extra, "primary"),
		SecondaryWindowMinutes: openAICodexExtraWindowMinutes(extra, "secondary"),
	}
	if used, valid := resolveAccountExtraNumber(extra, "codex_primary_used_percent"); valid {
		snapshot.PrimaryUsedPercent = &used
	}
	if used, valid := resolveAccountExtraNumber(extra, "codex_secondary_used_percent"); valid {
		snapshot.SecondaryUsedPercent = &used
	}
	normalized := snapshot.Normalize()
	if normalized == nil {
		return 0, "", false
	}
	used := normalized.Used5hPercent
	if window == "7d" {
		used = normalized.Used7dPercent
	}
	if used == nil || math.IsNaN(*used) || math.IsInf(*used, 0) {
		return 0, "", false
	}
	source = "secondary"
	if used == snapshot.PrimaryUsedPercent {
		source = "primary"
	}
	return *used, source, true
}

// Keep explicit zero/unknown/malformed durations present. Dropping them would
// activate Normalize's legacy mapping for snapshots that are not legacy data.
func openAICodexExtraWindowMinutes(extra map[string]any, source string) *int {
	key := "codex_" + source + "_window_minutes"
	if _, exists := extra[key]; !exists {
		return nil
	}
	minutes := 0
	if value, ok := resolveAccountExtraNumber(extra, key); ok && (value == 300 || value == 10080) {
		minutes = int(value)
	}
	return &minutes
}
