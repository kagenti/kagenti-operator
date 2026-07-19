package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestAgentSkill_IDField(t *testing.T) {
	skill := AgentSkill{
		ID:   "skill-001",
		Name: "test-skill",
	}

	data, err := json.Marshal(skill)
	if err != nil {
		t.Fatalf("failed to marshal AgentSkill: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if m["id"] != "skill-001" {
		t.Errorf("expected id to be 'skill-001', got %v", m["id"])
	}
}

func TestAgentSkill_IDOptional(t *testing.T) {
	skill := AgentSkill{
		Name: "test-skill",
	}

	data, err := json.Marshal(skill)
	if err != nil {
		t.Fatalf("failed to marshal AgentSkill: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if _, ok := m["id"]; ok {
		t.Error("expected id to be omitted from JSON when empty")
	}
}

func TestAgentSkill_TagsField(t *testing.T) {
	skill := AgentSkill{
		Name: "test-skill",
		Tags: []string{"weather", "forecast"},
	}

	data, err := json.Marshal(skill)
	if err != nil {
		t.Fatalf("failed to marshal AgentSkill: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	tags, ok := m["tags"].([]interface{})
	if !ok {
		t.Fatal("expected tags to be a slice")
	}
	if len(tags) != 2 || tags[0] != "weather" || tags[1] != "forecast" {
		t.Errorf("expected tags [weather, forecast], got %v", tags)
	}
}

func TestAgentSkill_TagsOptional(t *testing.T) {
	skill := AgentSkill{
		Name: "test-skill",
	}

	data, err := json.Marshal(skill)
	if err != nil {
		t.Fatalf("failed to marshal AgentSkill: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if _, ok := m["tags"]; ok {
		t.Error("expected tags to be omitted from JSON when empty")
	}
}

func TestAgentSkill_ExamplesField(t *testing.T) {
	skill := AgentSkill{
		Name:     "test-skill",
		Examples: []string{"What is the weather in NYC?", "Get forecast for London"},
	}

	data, err := json.Marshal(skill)
	if err != nil {
		t.Fatalf("failed to marshal AgentSkill: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	examples, ok := m["examples"].([]interface{})
	if !ok {
		t.Fatal("expected examples to be a slice")
	}
	if len(examples) != 2 || examples[0] != "What is the weather in NYC?" {
		t.Errorf("expected examples with 2 items, got %v", examples)
	}
}

func TestAgentSkill_ExamplesOptional(t *testing.T) {
	skill := AgentSkill{
		Name: "test-skill",
	}

	data, err := json.Marshal(skill)
	if err != nil {
		t.Fatalf("failed to marshal AgentSkill: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if _, ok := m["examples"]; ok {
		t.Error("expected examples to be omitted from JSON when empty")
	}
}
