package basispoints

import (
	"encoding/json"
	"fmt"
	"strings"
)

type unsupportedNativeToolError struct{ name string }

func newUnsupportedNativeToolError(name string) unsupportedNativeToolError {
	return unsupportedNativeToolError{name: safeDiagnosticToolName(name)}
}

func safeDiagnosticToolName(name string) string {
	if len(name) > 128 || strings.ContainsAny(name, "/~$") {
		return "[redacted]"
	}
	return safeSchemaName(name)
}

func (e unsupportedNativeToolError) Error() string {
	if e.name == "" {
		return "basispoints returned an unsupported native tool; no tool was executed"
	}
	return fmt.Sprintf("basispoints returned an unsupported native tool %q; no tool was executed", e.name)
}

// BridgeFailure describes local conversion failure, never an upstream HTTP
// rejection. Only bounded structural metadata may be added to Details.
type BridgeFailure struct {
	Message               string
	FailureStage          string
	RejectedTool          string
	RepairSkippedReason   string
	RepairFailureReason   string
	RepairToolIndex       int
	RepairAttempt         int
	UpstreamTerminalEvent string
}

func (f *BridgeFailure) Details() map[string]any {
	result := object{"type": "server_error", "code": "basispoints_protocol_error", "message": f.Message, "failure_stage": f.FailureStage}
	if f.RejectedTool != "" {
		result["rejected_tool"] = f.RejectedTool
	}
	if f.RepairSkippedReason != "" {
		result["repair_skipped_reason"] = f.RepairSkippedReason
	}
	if f.RepairFailureReason != "" {
		result["repair_failure_reason"] = f.RepairFailureReason
		result["repair_tool_index"] = f.RepairToolIndex
		result["repair_attempt"] = f.RepairAttempt
	}
	if f.UpstreamTerminalEvent != "" {
		result["upstream_terminal_event"] = f.UpstreamTerminalEvent
	}
	return result
}

// ParseBridgeFailure consumes the bridge's normalized output. Raw upstream
// failures pass through classifyUpstreamFailure first, which drops these fields.
func ParseBridgeFailure(raw []byte) *BridgeFailure {
	var payload object
	if decode(raw, &payload) != nil || text(payload["type"]) != "response.failed" {
		return nil
	}
	response, _ := payload["response"].(object)
	detail, _ := response["error"].(object)
	stage := text(detail["failure_stage"])
	if text(detail["code"]) != "basispoints_protocol_error" || (stage != "bridge_tool_validation" && stage != "bridge_protocol_conversion") {
		return nil
	}
	failure := &BridgeFailure{Message: text(detail["message"]), FailureStage: stage,
		RejectedTool: safeSchemaName(text(detail["rejected_tool"])), RepairSkippedReason: text(detail["repair_skipped_reason"]),
		UpstreamTerminalEvent: text(detail["upstream_terminal_event"])}
	if reason := text(detail["repair_failure_reason"]); stage == "bridge_tool_validation" && reason != "" {
		indexNumber, _ := detail["repair_tool_index"].(json.Number)
		attemptNumber, _ := detail["repair_attempt"].(json.Number)
		index, indexErr := indexNumber.Int64()
		attempt, attemptErr := attemptNumber.Int64()
		if indexErr == nil && attemptErr == nil && index >= 0 && index < maxRepairToolBatch && attempt >= 1 && attempt <= maxToolRepairs {
			switch reason {
			case repairOperationMismatch, repairTargetChanged, repairRawPayloadMismatch, repairCorrectedCallInvalid:
				failure.RepairFailureReason = reason
				failure.RepairToolIndex = int(index)
				failure.RepairAttempt = int(attempt)
			}
		}
	}
	return failure
}
