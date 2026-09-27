package config

import "testing"

func TestHearsayConfigurationRequiresDelegatedCredentialReferences(t *testing.T) {
	valid := func() Hearsay {
		return Hearsay{URL: "https://memory.example/api", Principal: "owner", TokenEnv: "OWNER_TOKEN", Agents: map[string]HearsayAgent{"worker": {ID: "worker", TokenEnv: "WORKER_TOKEN"}, "observer": {ID: "observer", TokenEnv: "OBSERVER_TOKEN"}, "orchestrator": {ID: "chief", TokenEnv: "CHIEF_TOKEN"}}}
	}
	if err := valid().validate("config.toml"); err != nil {
		t.Fatal(err)
	}
	if err := (Hearsay{}).validate("config.toml"); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Hearsay){
		func(h *Hearsay) { h.URL = "https://user:secret@memory.example" },
		func(h *Hearsay) { h.URL = "https://memory.example?token=secret" },
		func(h *Hearsay) { h.TokenEnv = "inline secret" },
		func(h *Hearsay) { delete(h.Agents, "worker") },
		func(h *Hearsay) { h.Agents["steward"] = HearsayAgent{ID: "owner", TokenEnv: "OWNER_TOKEN"} },
		func(h *Hearsay) { h.Principal = "owner\r\nHeader: secret" },
	} {
		h := valid()
		mutate(&h)
		if err := h.validate("config.toml"); err == nil {
			t.Fatal("invalid Hearsay configuration accepted")
		}
	}
}
