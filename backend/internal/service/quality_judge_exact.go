package service

import (
	"encoding/json"
	"io"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This contract is independent of the reference answer: only the question and
// these format instructions may be sent to the account being tested.
const qualityExactAnswerContract = "只输出一个最终答案值，不要推理、解释、前缀、单位或 Markdown。数字直接输出；文本使用 JSON 字符串；结构化答案使用完整 JSON 对象或数组，并严格遵循题目要求的字段。"

var qualityExactNumberPattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:[.][0-9]*)?|[.][0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func qualityExactJudgeConfigured(cfg *PelicanTestConfig) bool {
	return cfg != nil && cfg.Quality != nil && cfg.Quality.Judge != nil && cfg.Quality.Judge.Kind == "exact"
}

func qualityExactUnknown(reason string) *QualityJudgment {
	return &QualityJudgment{Kind: "exact", RulesVersion: "1", Verdict: "unknown", Reason: reason}
}

func judgeQualityExact(cfg *PelicanTestConfig, answer string) *QualityJudgment {
	result := qualityExactUnknown("judge_exact_invalid_config")
	if !qualityExactJudgeConfigured(cfg) {
		return result
	}
	judge := cfg.Quality.Judge
	if judge.GroupID != 0 || judge.ModelID != "" || judge.Prompt != "" {
		return result
	}
	if len(answer) > 64000 {
		result.Reason = "judge_input_too_large"
		return result
	}
	reference, ok := qualityExactValue(cfg.Quality.ExpectedAnswer)
	if !ok {
		// Existing custom text references are allowed, but unquoted prose is
		// never evidence of a wrong answer. A different text answer must be an
		// explicit JSON string under the delivery contract.
		text, valid := qualityExactUnwrap(cfg.Quality.ExpectedAnswer)
		if !valid || len(text) > 4000 || strings.ContainsAny(text, "\r\n") || strings.ContainsAny(text, "{}[]\"`") {
			result.Reason = "judge_exact_invalid_reference"
			return result
		}
		reference = text
	}
	candidate, ok := qualityExactValue(answer)
	if !ok {
		text, valid := qualityExactUnwrap(answer)
		if expected, isText := reference.(string); valid && isText && text == expected {
			candidate, ok = text, true
		}
	}
	if !ok {
		result.Reason = "judge_exact_ambiguous_answer"
		return result
	}
	equal, complete := qualityExactCompare(reference, candidate)
	if !complete {
		result.Reason = "judge_exact_answer_shape_mismatch"
		return result
	}
	result.Verdict, result.Reason = "incorrect", "judge_exact_mismatch"
	if equal {
		result.Verdict, result.Reason = "correct", "judge_exact_match"
	}
	return result
}

func qualityExactUnwrap(value string) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r >= '\uff01' && r <= '\uff5e' {
			return r - 0xfee0
		}
		return r
	}, value))
	if strings.HasPrefix(value, "```") {
		header, body, found := strings.Cut(value, "\n")
		if !found || !strings.HasSuffix(body, "```") {
			return "", false
		}
		switch strings.TrimSpace(strings.TrimPrefix(header, "```")) {
		case "", "json", "text", "plaintext", "txt":
		default:
			return "", false
		}
		value = strings.TrimSpace(strings.TrimSuffix(body, "```"))
	}
	return value, value != "" && !strings.Contains(value, "```")
}

func qualityExactNumber(value string) (*big.Rat, bool) {
	if len(value) > 4000 || !qualityExactNumberPattern.MatchString(value) {
		return nil, false
	}
	// Bound exponent expansion as well as source size before using big.Rat.
	if index := strings.IndexAny(value, "eE"); index >= 0 {
		exponent, err := strconv.Atoi(value[index+1:])
		if err != nil || exponent < -1000 || exponent > 1000 {
			return nil, false
		}
	}
	// An integer with leading zeroes is still decimal, never Go's base-0 octal.
	if !strings.ContainsAny(value, ".eE") {
		value += ".0"
	}
	return new(big.Rat).SetString(value)
}

func qualityExactValue(value string) (any, bool) {
	value, ok := qualityExactUnwrap(value)
	if !ok || len(value) > 4000 {
		return nil, false
	}
	if number, ok := qualityExactNumber(value); ok {
		return number, true
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	parsed, ok := qualityExactJSONValue(decoder, 0)
	if !ok {
		return nil, false
	}
	_, err := decoder.Token()
	return parsed, err == io.EOF
}

// Decode recursively to reject duplicate keys and excessive nesting, neither of
// which is an unambiguous final answer. Numbers retain exact decimal precision.
func qualityExactJSONValue(decoder *json.Decoder, depth int) (any, bool) {
	if depth > 32 {
		return nil, false
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return nil, false
	}
	switch value := token.(type) {
	case json.Number:
		return qualityExactNumber(string(value))
	case string, bool:
		return value, true
	case json.Delim:
		switch value {
		case '{':
			object := map[string]any{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return nil, false
				}
				if _, exists := object[name]; exists {
					return nil, false
				}
				child, ok := qualityExactJSONValue(decoder, depth+1)
				if !ok {
					return nil, false
				}
				object[name] = child
			}
			end, err := decoder.Token()
			return object, err == nil && end == json.Delim('}')
		case '[':
			var array []any
			for decoder.More() {
				child, ok := qualityExactJSONValue(decoder, depth+1)
				if !ok {
					return nil, false
				}
				array = append(array, child)
			}
			end, err := decoder.Token()
			return array, err == nil && end == json.Delim(']')
		}
	}
	return nil, false
}

// Shape mismatch can indicate missing output; only compare complete values of
// the same shape. This keeps malformed/partial answers out of failure actions.
func qualityExactCompare(reference, candidate any) (equal, complete bool) {
	switch expected := reference.(type) {
	case *big.Rat:
		actual, ok := candidate.(*big.Rat)
		return ok && expected.Cmp(actual) == 0, ok
	case string:
		actual, ok := candidate.(string)
		return ok && expected == actual, ok
	case bool:
		actual, ok := candidate.(bool)
		return ok && expected == actual, ok
	case map[string]any:
		actual, ok := candidate.(map[string]any)
		if !ok || len(expected) != len(actual) {
			return false, false
		}
		equal = true
		for key, value := range expected {
			other, exists := actual[key]
			match, complete := qualityExactCompare(value, other)
			if !exists || !complete {
				return false, false
			}
			equal = equal && match
		}
		return equal, true
	case []any:
		actual, ok := candidate.([]any)
		if !ok || len(expected) != len(actual) {
			return false, false
		}
		equal = true
		for index, value := range expected {
			match, complete := qualityExactCompare(value, actual[index])
			if !complete {
				return false, false
			}
			equal = equal && match
		}
		return equal, true
	}
	return false, false
}
