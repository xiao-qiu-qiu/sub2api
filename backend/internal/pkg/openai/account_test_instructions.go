package openai

// AccountTestInstructions is the default agent prompt bundled with Codex CLI
// 0.167.1 (gpt-6.1-sol). The existing descriptor has the identical prompt, so
// account tests can reuse it without changing legacy gateway prompt selection.
// Source: openai/codex, rust-v0.167.1, commit
// 0a94398fa60c4fac0e7659e4213a0a36e613dd46,
// codex-rs/models-manager/models.json: model_messages.instructions_template.
var AccountTestInstructions = CodexBaseInstructionsForModel("gpt-6.1-sol")
