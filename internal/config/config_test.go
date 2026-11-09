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

func TestSearchProvider(t *testing.T) {
	load := func(extra map[string]string) (*Config, []string) {
		env := map[string]string{"API_ADDR": ":8088", "API_KEY": "k", "LLM_BASE_URL": "http://x", "LLM_MODEL": "m", "EMBED_BASE_URL": "http://e", "EMBED_MODEL": "e", "SEARCH_ENABLED": "true", "SEARCH_BASE_URL": "http://s"}
		for k, v := range extra {
			env[k] = v
		}
		c, err := Load(mapEnv(env))
		if err != nil {
			return nil, []string{err.Error()}
		}
		return c, c.Validate()
	}
	if c, errs := load(nil); len(errs) != 0 || c.SearchProvider != "deepseek" {
		t.Fatalf("default provider: %v", errs)
	}
	if _, errs := load(map[string]string{"SEARCH_PROVIDER": "searxng"}); len(errs) == 0 {
		t.Fatal("searxng without SEARXNG_URL must fail")
	}
	if _, errs := load(map[string]string{"SEARCH_PROVIDER": "searxng", "SEARXNG_URL": "localhost:8080"}); len(errs) == 0 {
		t.Fatal("SEARXNG_URL without scheme must fail")
	}
	c, errs := load(map[string]string{"SEARCH_PROVIDER": "SearXNG", "SEARXNG_URL": "http://searxng:8080/"})
	if len(errs) != 0 || c.SearchProvider != "searxng" || c.SearxngURL != "http://searxng:8080" {
		t.Fatalf("%+v %v", c, errs)
	}
	if _, errs := load(map[string]string{"SEARCH_PROVIDER": "bing"}); len(errs) == 0 {
		t.Fatal("unknown provider must fail")
	}
	if _, errs := load(map[string]string{"SEARCH_ENABLED": "false", "SEARCH_PROVIDER": "searxng"}); len(errs) != 0 {
		t.Fatalf("disabled search needs no URL: %v", errs)
	}
}

func TestPluginURLs(t *testing.T) {
	load := func(urls string) (*Config, error) {
		return Load(mapEnv(map[string]string{"API_ADDR": ":8088", "API_KEY": "k", "LLM_BASE_URL": "http://x", "LLM_MODEL": "m", "EMBED_BASE_URL": "http://e", "EMBED_MODEL": "e", "PLUGIN_URLS": urls}))
	}
	c, err := load(" http://mnemo-plugin:8090/ , ,http://b:1 ")
	if err != nil || len(c.PluginURLs) != 2 || c.PluginURLs[0] != "http://mnemo-plugin:8090" || c.PluginTimeout.Seconds() != 180 {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := load("mnemo-plugin:8090"); err == nil {
		t.Fatal("a URL without scheme must be rejected")
	}
	if c, err := load(""); err != nil || len(c.PluginURLs) != 0 {
		t.Fatal("empty means no plugins")
	}
}

func TestVoiceConfig(t *testing.T) {
	base := map[string]string{"TELEGRAM_TOKEN": "t", "LLM_BASE_URL": "http://x", "LLM_MODEL": "m", "EMBED_BASE_URL": "http://x", "EMBED_MODEL": "e"}
	load := func(extra map[string]string) *Config {
		c, err := Load(func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return base[k]
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := load(nil)
	if c.STT != nil || c.TTS != nil || len(c.Validate()) != 0 {
		t.Fatalf("voice must be off by default: %+v", c.Validate())
	}
	c = load(map[string]string{"STT_ENABLED": "true", "TTS_ENABLED": "true"})
	if len(c.Validate()) != 2 {
		t.Fatalf("missing URL/model must be reported: %v", c.Validate())
	}
	c = load(map[string]string{"STT_ENABLED": "1", "STT_BASE_URL": "http://s/v1/", "STT_MODEL": "w", "TTS_ENABLED": "1",
		"TTS_BASE_URL": "http://t/v1", "TTS_MODEL": "k", "TTS_EXTRA_BODY": `{"speed":1.2}`})
	if errs := c.Validate(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if c.STT.BaseURL != "http://s/v1" || c.TTSFormat != "mp3" || c.TTSMode != "auto" || c.TTSMaxChars != 500 || !c.TTSText || c.TTS.ExtraBody["speed"] != 1.2 {
		t.Fatalf("defaults wrong: %+v %+v", c, c.TTS)
	}
	c = load(map[string]string{"TTS_ENABLED": "1", "TTS_BASE_URL": "http://t", "TTS_MODEL": "k", "TTS_FORMAT": "wav", "TTS_MODE": "x"})
	if len(c.Validate()) != 2 {
		t.Fatalf("bad format/mode must be rejected: %v", c.Validate())
	}
}

func TestParseTTSVoices(t *testing.T) {
	v := parseTTSVoices(" mll = 软萌日常 ; serena=更俏皮;mll=dup;;bare ")
	if len(v) != 3 || v[0] != (TTSVoice{"mll", "软萌日常"}) || v[1].Name != "serena" || v[2] != (TTSVoice{"bare", ""}) {
		t.Fatalf("got %+v", v)
	}
	c, err := Load(mapEnv(map[string]string{"LLM_BASE_URL": "http://x/v1/", "LLM_MODEL": "m",
		"TTS_ENABLED": "1", "TTS_BASE_URL": "http://t", "TTS_MODEL": "k", "TTS_VOICES": "a=x;b=y"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.TTSVoice != "a" || len(c.TTSVoices) != 2 {
		t.Fatalf("default voice should be the first: %+v", c)
	}
}
