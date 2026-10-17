package config

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// An explicit `status: 0` decodes to StatusNoHTTP and is written back as 0; an
// omitted status stays 0 (matches nothing) and stays omitted, in YAML and JSON.
func TestRequestScopedErrorRuleExplicitStatusZero(t *testing.T) {
	var rules []RequestScopedErrorRule
	src := "- {status: 0, match: [refused], action: continue}\n- {match: [refused], action: continue}\n- {status: 503, match: [x], action: continue}\n"
	if err := yaml.Unmarshal([]byte(src), &rules); err != nil {
		t.Fatal(err)
	}
	if got := []int{rules[0].Status, rules[1].Status, rules[2].Status}; got[0] != StatusNoHTTP || got[1] != 0 || got[2] != 503 {
		t.Fatalf("yaml statuses = %v, want [%d 0 503]", got, StatusNoHTTP)
	}
	out, err := yaml.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), "status: 0"); n != 1 || strings.Contains(string(out), "-1") {
		t.Fatalf("yaml round trip:\n%s", out)
	}

	raw, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	var back []RequestScopedErrorRule
	if err = json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back[0].Status != StatusNoHTTP || back[1].Status != 0 || back[2].Status != 503 {
		t.Fatalf("json round trip %s -> %+v", raw, back)
	}
}
