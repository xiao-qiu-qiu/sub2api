package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var errResponsesTestRefused = errors.New(pelicanErrRefused + ": Responses gateway test response contains a refusal")

type responsesTestContent struct {
	Type string  `json:"type"`
	Text *string `json:"text"`
}

type responsesTestOutput struct {
	ID      string                 `json:"id"`
	Type    string                 `json:"type"`
	Role    string                 `json:"role"`
	Status  string                 `json:"status"`
	Content []responsesTestContent `json:"content"`
}

func (item responsesTestOutput) hasRefusal() bool {
	for _, part := range item.Content {
		if part.Type == "refusal" {
			return true
		}
	}
	return false
}

type responsesTestEvent struct {
	Type         string                `json:"type"`
	OutputIndex  *int                  `json:"output_index"`
	ContentIndex *int                  `json:"content_index"`
	ItemID       string                `json:"item_id"`
	ResponseID   string                `json:"response_id"`
	Delta        *string               `json:"delta"`
	Text         *string               `json:"text"`
	Item         *responsesTestOutput  `json:"item"`
	Part         *responsesTestContent `json:"part"`
	Response     *struct {
		ID     string                `json:"id"`
		Status string                `json:"status"`
		Output []responsesTestOutput `json:"output"`
	} `json:"response"`
}

// Publish only a completed, consistent answer. In particular, provisional
// deltas must never reach Pelican's answer judge when the final output differs.
func responsesGatewayAccountTestAnswer(wire string) (string, error) {
	fail := func(reason string) error {
		return fmt.Errorf("Responses gateway test response %s", reason)
	}
	var textEvents []responsesTestEvent
	var completed *responsesTestEvent
	var responseID string
	itemIDs := make(map[int]string)
	itemIndexes := make(map[string]int)
	bindItem := func(index int, id string) error {
		if index < 0 {
			return fail("has an invalid output index")
		}
		if id == "" {
			return nil
		}
		if previous, ok := itemIDs[index]; ok && previous != id {
			return fail("mixes output item identities")
		}
		if previous, ok := itemIndexes[id]; ok && previous != index {
			return fail("reuses an output item identity")
		}
		itemIDs[index], itemIndexes[id] = id, index
		return nil
	}
	var data []string
	ended := false
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = nil
		if ended {
			return fail("contains data after the stream terminator")
		}
		if strings.TrimSpace(payload) == "[DONE]" {
			ended = true
			return nil
		}
		if completed != nil {
			return fail("contains events after completion")
		}
		var event responsesTestEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil || event.Type == "" {
			return fail("contains an invalid SSE event")
		}
		// A refusal can accompany otherwise valid answer text, or appear only
		// in streaming events. Never send any part of such a response to a judge.
		if event.Type == "response.refusal.delta" || event.Type == "response.refusal.done" ||
			event.Part != nil && event.Part.Type == "refusal" || event.Item != nil && event.Item.hasRefusal() {
			return errResponsesTestRefused
		}
		ids := []string{event.ResponseID}
		if event.Response != nil {
			for _, item := range event.Response.Output {
				if item.hasRefusal() {
					return errResponsesTestRefused
				}
			}
			ids = append(ids, event.Response.ID)
		}
		for _, id := range ids {
			if id != "" {
				if responseID != "" && responseID != id {
					return fail("mixes response identities")
				}
				responseID = id
			}
		}
		switch event.Type {
		case "response.output_item.added", "response.output_item.done":
			if event.OutputIndex != nil && event.Item != nil {
				return bindItem(*event.OutputIndex, event.Item.ID)
			}
		case "response.output_text.delta", "response.output_text.done":
			if event.Type == "response.output_text.delta" && event.Delta == nil ||
				event.Type == "response.output_text.done" && event.Text == nil {
				return fail("contains a text event without text")
			}
			textEvents = append(textEvents, event)
		case "response.completed":
			if event.Response == nil || event.Response.Output == nil ||
				event.Response.Status != "" && event.Response.Status != "completed" {
				return fail("has no completed final output")
			}
			completed = &event
		case "response.failed", "response.incomplete", "response.cancelled", "error":
			return fail("ended with " + event.Type)
		}
		return nil
	}
	for _, line := range strings.Split(wire, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			if err := flush(); err != nil {
				return "", err
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := flush(); err != nil {
		return "", err
	}
	if completed == nil {
		return "", fail("ended before completion")
	}

	type partKey struct{ output, content int }
	finalTexts := make(map[partKey]string)
	var answer strings.Builder
	for index, item := range completed.Response.Output {
		if err := bindItem(index, item.ID); err != nil {
			return "", err
		}
		if item.Type != "message" {
			continue
		}
		if item.Role != "" && item.Role != "assistant" || item.Status != "" && item.Status != "completed" {
			return "", fail("contains an unfinished or non-assistant message")
		}
		for contentIndex, part := range item.Content {
			if part.Type != "output_text" {
				continue
			}
			if part.Text == nil {
				return "", fail("contains a final text part without text")
			}
			finalTexts[partKey{index, contentIndex}] = *part.Text
			answer.WriteString(*part.Text)
		}
	}
	type observedText struct {
		delta    strings.Builder
		hasDelta bool
		done     *string
	}
	observed := make(map[partKey]*observedText)
	for _, event := range textEvents {
		if event.OutputIndex != nil {
			if err := bindItem(*event.OutputIndex, event.ItemID); err != nil {
				return "", err
			}
		}
	}
	for _, event := range textEvents {
		outputIndex := event.OutputIndex
		if outputIndex == nil && event.ItemID != "" {
			index, ok := itemIndexes[event.ItemID]
			if !ok {
				return "", fail("contains an unrecognized output item")
			}
			outputIndex = &index
		}
		var key partKey
		matches := 0
		for candidate := range finalTexts {
			if (outputIndex == nil || candidate.output == *outputIndex) &&
				(event.ContentIndex == nil || candidate.content == *event.ContentIndex) {
				key, matches = candidate, matches+1
			}
		}
		// Legacy BPS events can omit indices. Accept only an unambiguous match
		// against the final output rather than assuming all text belongs to item 0.
		if matches != 1 {
			return "", fail("has missing or ambiguous text indices")
		}
		if err := bindItem(key.output, event.ItemID); err != nil {
			return "", err
		}
		part := observed[key]
		if part == nil {
			part = &observedText{}
			observed[key] = part
		}
		if event.Type == "response.output_text.delta" {
			if part.done != nil {
				return "", fail("contains text deltas after text completion")
			}
			part.hasDelta = true
			part.delta.WriteString(*event.Delta)
		} else {
			if part.done != nil {
				return "", fail("repeats text completion")
			}
			part.done = event.Text
		}
	}
	for key, part := range observed {
		final := finalTexts[key]
		if part.hasDelta && part.delta.String() != final || part.done != nil && *part.done != final {
			return "", fail("text is inconsistent between deltas, text completion and final output")
		}
	}
	if strings.TrimSpace(answer.String()) == "" {
		return "", fail("contains empty output")
	}
	return answer.String(), nil
}
