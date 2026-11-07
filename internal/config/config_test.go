package config

import "testing"

func mapEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadFallbacksAndValidate(t *testing.T) {
	c, err := Load(mapEnv(map[string]string{
		"TELEGRAM_TOKEN": "t", "ALLOWED_USER_IDS": "1, 2", "TZ": "UTC",
		"LLM_BASE_URL": "http://x/v1/", "LLM_MODEL": "m", "LLM_EXTRA_BODY": `{"a":1}`,
		"EMBED_BASE_URL": "http://e/v1", "EMBED_MODEL": "bge",
		"VISION_ENABLED": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if errs := c.Validate(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if c.LLM.BaseURL != "http://x/v1" || !c.AllowedIDs[2] {
		t.Fatalf("bad parse %+v", c.LLM)
	}
	if c.Maint.Model != "m" || c.Maint.ExtraBody["a"] == nil || c.Vision == nil || c.Vision.Model != "m" {
		t.Fatal("MAINT/VISION must fall back to LLM")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(mapEnv(map[string]string{"TZ": "Nope/Zone", "TOP_K": "x", "LLM_EXTRA_BODY": "[1"})); err == nil {
		t.Fatal("expected errors")
	}
	c, _ := Load(mapEnv(nil))
	if len(c.Validate()) < 3 {
		t.Fatal("missing settings must be reported")
	}
}

func TestAPIValidation(t *testing.T) {
	c, _ := Load(mapEnv(map[string]string{"API_ADDR": ":8088", "LLM_BASE_URL": "http://x", "LLM_MODEL": "m", "EMBED_BASE_URL": "http://e", "EMBED_MODEL": "e"}))
	if len(c.Validate()) != 1 || c.APIModel != "companion" {
		t.Fatalf("API without key must be rejected: %v", c.Validate())
	}
	c.APIKey = "k"
	if len(c.Validate()) != 0 {
		t.Fatal("API-only mode (no Telegram token) must be valid")
	}
}
