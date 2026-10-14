package cliproxy

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestSessionAffinityRebindOnConfig(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		yaml      string
		wantRaw   string
		wantQuota bool
	}{
		{name: "default", yaml: "routing:\n  session-affinity: true\n", wantRaw: "", wantQuota: false},
		{name: "any", yaml: "routing:\n  session-affinity: true\n  session-affinity-rebind-on: any\n", wantRaw: "any", wantQuota: false},
		{name: "quota", yaml: "routing:\n  session-affinity: true\n  session-affinity-rebind-on: \" Quota \"\n", wantRaw: " Quota ", wantQuota: true},
		{name: "invalid falls back to any", yaml: "routing:\n  session-affinity: true\n  session-affinity-rebind-on: cooldown\n", wantRaw: "cooldown", wantQuota: false},
		{name: "ignored without session affinity", yaml: "routing:\n  session-affinity-rebind-on: quota\n", wantRaw: "quota", wantQuota: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw := []byte("config-version: 8\n" + testCase.yaml)
			if errValidate := internalconfig.ValidateV8Config(raw); errValidate != nil {
				t.Fatalf("ValidateV8Config: %v", errValidate)
			}
			cfg, errParse := internalconfig.ParseConfigBytes(raw)
			if errParse != nil {
				t.Fatalf("ParseConfigBytes: %v", errParse)
			}
			if got := cfg.Routing.SessionAffinityRebindOn; got != testCase.wantRaw {
				t.Fatalf("SessionAffinityRebindOn = %q, want %q", got, testCase.wantRaw)
			}
			if got := normalizedRoutingRuntimeState(cfg).sessionAffinityRebindOnQuota; got != testCase.wantQuota {
				t.Fatalf("sessionAffinityRebindOnQuota = %t, want %t", got, testCase.wantQuota)
			}
		})
	}
}
