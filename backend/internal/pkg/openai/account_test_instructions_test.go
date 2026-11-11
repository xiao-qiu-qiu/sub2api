package openai

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestAccountTestInstructionsMatchesCodexRelease(t *testing.T) {
	// SHA-256 of gpt-6.1-sol's instructions_template from rust-v0.167.1.
	const wantHash = "e1bdd4f8f0df4b20f4a0ffc8a861ce819df45325d8cecdfb92e80379cf8d142e"
	if got := fmt.Sprintf("%x", sha256.Sum256([]byte(AccountTestInstructions))); got != wantHash {
		t.Fatalf("account test prompt no longer matches Codex CLI 0.167.1: got %s", got)
	}
	if !strings.HasPrefix(AccountTestInstructions, "You are Codex, an agent based on GPT-6.") {
		t.Fatal("account test prompt must use the current Codex agent identity")
	}
	if AccountTestInstructions == DefaultInstructions {
		t.Fatal("account tests must not reuse the legacy GPT-5-Codex prompt")
	}
}
