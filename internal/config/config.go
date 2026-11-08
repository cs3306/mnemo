// Package config loads settings from environment variables.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Endpoint is an OpenAI-compatible service: base URL (without /chat/completions), key, model.
type Endpoint struct {
	BaseURL   string
	APIKey    string
	Model     string
	ExtraBody map[string]any
	Timeout   time.Duration
}

type Config struct {
	TelegramToken string
	TGAPIBase     string
	AllowedIDs    map[int64]bool
	DataDir       string
	ConfigDir     string // user-editable persona.md / profile.md; defaults to DataDir
	TZ            string
	Loc           *time.Location

	LLM         Endpoint
	Maint       Endpoint
	Vision      *Endpoint // nil when image reading is disabled
	VisionProbe bool      // check at startup that the vision model really reads images
	Embed       Endpoint
	Review      Endpoint // style review (REVIEW_*); falls back to MAINT_*

	Temperature      float64
	RecentTokens     int
	MemoryTokens     int
	TopK             int
	MinSim           float64
	ExtractEvery     int
	IdleMinutes      int
	ConsolidateHours int
	CoreMaxChars     int
	MaxPins          int
	AutoReembed      bool

	ReviewHours       int  // REVIEW_HOURS: minimum hours between automatic reviews; 0 disables them
	ReviewMinMessages int  // REVIEW_MIN_MESSAGES: new messages needed before an automatic review
	ReviewMaxChars    int  // REVIEW_MAX_CHARS: conversation text sent to the reviewer (newest kept)
	ReviewAutoApply   bool // REVIEW_AUTO_APPLY: apply proposals without /style approve
	StyleMaxChars     int  // STYLE_MAX_CHARS: hard cap on the style notes

	APIAddr  string // e.g. ":8088"; empty disables the HTTP API
	APIKey   string
	APIModel string // model name advertised by the API

	SearchEnabled     bool          // give the model web_search / web_fetch tools (DeepSeek Search/Fetch API)
	SearchBase        string        // default: LLM_BASE_URL without a trailing /v1
	SearchKey         string        // default: LLM_API_KEY
	SearchProvider    string        // "deepseek" (default) or "searxng"
	SearxngURL        string        // SEARXNG_URL, e.g. http://searxng:8080
	FetchAllowPrivate bool          // allow web_fetch to reach private/loopback addresses (searxng provider)
	PluginURLs        []string      // PLUGIN_URLS: base URLs of tool plugin servers (see README)
	PluginToken       string        // PLUGIN_TOKEN: bearer token sent to every plugin server
	PluginTimeout     time.Duration // PLUGIN_TIMEOUT seconds per tool call
	MaxToolRounds     int
	ToolGraceRounds   int // TOOL_GRACE_ROUNDS: the last rounds of a turn, announced to the model so it can wrap up

	PriceHit, PriceMiss, PriceOut float64 // per million tokens, optional (for the /status cost estimate)
	PriceCurrency                 string
}

type env struct {
	get  func(string) string
	errs []string
}

func (e *env) str(name, def string) string {
	if v := strings.TrimSpace(e.get(name)); v != "" {
		return v
	}
	return def
}

func (e *env) boolean(name string, def bool) bool {
	v := strings.ToLower(e.str(name, ""))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func (e *env) integer(name string, def int) int {
	v := e.str(name, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Sprintf("%s must be an integer", name))
		return def
	}
	return n
}

func (e *env) float(name string, def float64) float64 {
	v := e.str(name, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		e.errs = append(e.errs, fmt.Sprintf("%s must be a number", name))
		return def
	}
	return f
}

func (e *env) jsonObj(name string) map[string]any {
	v := e.str(name, "")
	if v == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		e.errs = append(e.errs, fmt.Sprintf("%s is not a valid JSON object", name))
		return nil
	}
	return m
}

// endpoint reads PREFIX_BASE_URL / _API_KEY / _MODEL / _EXTRA_BODY / _TIMEOUT, falling back to fb.
func (e *env) endpoint(prefix string, fb *Endpoint) Endpoint {
	var d Endpoint
	d.Timeout = 300 * time.Second
	if fb != nil {
		d = *fb
	}
	ep := Endpoint{
		BaseURL: strings.TrimRight(e.str(prefix+"_BASE_URL", d.BaseURL), "/"),
		APIKey:  e.str(prefix+"_API_KEY", d.APIKey),
		Model:   e.str(prefix+"_MODEL", d.Model),
		Timeout: time.Duration(e.float(prefix+"_TIMEOUT", d.Timeout.Seconds()) * float64(time.Second)),
	}
	if m := e.jsonObj(prefix + "_EXTRA_BODY"); m != nil {
		ep.ExtraBody = m
	} else {
		ep.ExtraBody = d.ExtraBody
	}
	return ep
}

// Load reads the configuration. getenv is os.Getenv in production; tests pass a map lookup.
func Load(getenv func(string) string) (*Config, error) {
	e := &env{get: getenv}
	c := &Config{
		TelegramToken: e.str("TELEGRAM_TOKEN", ""),
		TGAPIBase:     strings.TrimRight(e.str("TELEGRAM_API_BASE", "https://api.telegram.org"), "/"),
		AllowedIDs:    map[int64]bool{},
		DataDir:       e.str("DATA_DIR", "/data"),
		TZ:            e.str("TZ", "UTC"),
	}
	for _, p := range strings.Split(e.str("ALLOWED_USER_IDS", ""), ",") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil {
			e.errs = append(e.errs, "ALLOWED_USER_IDS must be comma-separated integers")
			break
		}
		c.AllowedIDs[id] = true
	}
	loc, err := time.LoadLocation(c.TZ)
	if err != nil {
		e.errs = append(e.errs, fmt.Sprintf("TZ %q is not a valid time zone", c.TZ))
		loc = time.UTC
	}
	c.Loc = loc
	c.ConfigDir = e.str("CONFIG_DIR", c.DataDir)

	c.LLM = e.endpoint("LLM", nil)
	c.Maint = e.endpoint("MAINT", &c.LLM)
	if e.boolean("VISION_ENABLED", false) {
		v := e.endpoint("VISION", &c.LLM)
		c.Vision = &v
		c.VisionProbe = e.boolean("VISION_PROBE", true)
	}
	c.Embed = e.endpoint("EMBED", nil)
	c.Review = e.endpoint("REVIEW", &c.Maint)
	if _, ok := c.Review.ExtraBody["max_tokens"]; !ok {
		// some OpenAI-compatible providers (e.g. Claude's compatibility layer) want an explicit limit
		eb := map[string]any{"max_tokens": 8000}
		for k, v := range c.Review.ExtraBody {
			eb[k] = v
		}
		c.Review.ExtraBody = eb
	}

	c.Temperature = e.float("TEMPERATURE", 0.8)
	c.RecentTokens = e.integer("RECENT_TOKENS", 3000)
	c.MemoryTokens = e.integer("MEMORY_TOKENS", 1200)
	c.TopK = e.integer("TOP_K", 8)
	c.MinSim = e.float("MIN_SIM", 0.35)
	c.ExtractEvery = e.integer("EXTRACT_EVERY", 3)
	c.IdleMinutes = e.integer("IDLE_MINUTES", 20)
	c.ConsolidateHours = e.integer("CONSOLIDATE_HOURS", 20)
	c.CoreMaxChars = e.integer("CORE_MAX_CHARS", 600)
	c.MaxPins = e.integer("MAX_PINS", 5)
	c.AutoReembed = e.boolean("AUTO_REEMBED", true)
	c.ReviewHours = e.integer("REVIEW_HOURS", 24)
	c.ReviewMinMessages = e.integer("REVIEW_MIN_MESSAGES", 20)
	c.ReviewMaxChars = e.integer("REVIEW_MAX_CHARS", 30000)
	c.ReviewAutoApply = e.boolean("REVIEW_AUTO_APPLY", false)
	c.StyleMaxChars = e.integer("STYLE_MAX_CHARS", 800)
	c.APIAddr = e.str("API_ADDR", "")
	c.APIKey = e.str("API_KEY", "")
	c.APIModel = e.str("API_MODEL_NAME", "companion")
	c.SearchEnabled = e.boolean("SEARCH_ENABLED", false)
	c.SearchBase = strings.TrimSuffix(strings.TrimRight(e.str("SEARCH_BASE_URL", c.LLM.BaseURL), "/"), "/v1")
	c.SearchKey = e.str("SEARCH_API_KEY", c.LLM.APIKey)
	c.SearchProvider = strings.ToLower(e.str("SEARCH_PROVIDER", "deepseek"))
	c.SearxngURL = strings.TrimRight(e.str("SEARXNG_URL", ""), "/")
	c.FetchAllowPrivate = e.boolean("FETCH_ALLOW_PRIVATE", false)
	for _, u := range strings.Split(e.str("PLUGIN_URLS", ""), ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			c.PluginURLs = append(c.PluginURLs, u)
		}
	}
	c.PluginToken = e.str("PLUGIN_TOKEN", "")
	c.PluginTimeout = time.Duration(e.float("PLUGIN_TIMEOUT", 180) * float64(time.Second))
	c.MaxToolRounds = e.integer("MAX_TOOL_ROUNDS", 4)
	c.ToolGraceRounds = e.integer("TOOL_GRACE_ROUNDS", 3)
	c.PriceHit = e.float("PRICE_CACHE_HIT", 0)
	c.PriceMiss = e.float("PRICE_CACHE_MISS", 0)
	c.PriceOut = e.float("PRICE_OUTPUT", 0)
	c.PriceCurrency = e.str("PRICE_CURRENCY", "元")

	for _, u := range c.PluginURLs {
		if pu, err := url.Parse(u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			e.errs = append(e.errs, fmt.Sprintf("PLUGIN_URLS entry %q must be an http(s) URL", u))
		}
	}
	switch c.SearchProvider {
	case "deepseek":
		if c.SearchEnabled && c.SearchBase == "" {
			e.errs = append(e.errs, "SEARCH_ENABLED needs SEARCH_BASE_URL or LLM_BASE_URL")
		}
	case "searxng":
		if c.SearchEnabled {
			if u, err := url.Parse(c.SearxngURL); c.SearxngURL == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				e.errs = append(e.errs, "SEARCH_PROVIDER=searxng needs SEARXNG_URL (an http(s) URL, e.g. http://searxng:8080)")
			}
		}
	default:
		e.errs = append(e.errs, fmt.Sprintf("SEARCH_PROVIDER must be deepseek or searxng, got %q", c.SearchProvider))
	}
	if len(e.errs) > 0 {
		return c, fmt.Errorf("invalid configuration: %s", strings.Join(e.errs, "; "))
	}
	return c, nil
}

// Validate reports missing required settings for running the bot.
func (c *Config) Validate() []string {
	var errs []string
	if c.TelegramToken == "" && c.APIAddr == "" {
		errs = append(errs, "set TELEGRAM_TOKEN (bot token from @BotFather) and/or API_ADDR (HTTP API)")
	}
	if c.APIAddr != "" && c.APIKey == "" {
		errs = append(errs, "API_KEY is required when API_ADDR is set")
	}
	if c.LLM.BaseURL == "" || c.LLM.Model == "" {
		errs = append(errs, "LLM_BASE_URL and LLM_MODEL are required")
	}
	if c.Embed.BaseURL == "" || c.Embed.Model == "" {
		errs = append(errs, "EMBED_BASE_URL and EMBED_MODEL are required")
	}
	return errs
}
