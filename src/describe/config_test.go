package describe

import (
	"testing"
	"time"

	sdk "github.com/cameraui/sdk/go"
)

// Compile-time proof that the real storage type still satisfies ConfigGetter.
// This is the whole reason ConfigGetter's GetValue is variadic (see its doc
// comment): a non-variadic `GetValue(string, any) any` reads more naturally but
// silently fails to match *sdk.DeviceStorage, and without this assertion that
// mismatch would only surface at wire-up time in plugin.go rather than here.
var _ ConfigGetter = (*sdk.DeviceStorage)(nil)

// fakeGetter is a ConfigGetter over a plain map, resolving absent keys to the
// caller's fallback exactly as sdk.DeviceStorage.GetValue does for a key with
// no stored value and no schema default. It deliberately does NOT coerce or
// validate anything, so each test below drives Load with precisely the value
// (and Go type) it wants to exercise.
type fakeGetter map[string]any

func (f fakeGetter) GetValue(key string, fallback ...any) any {
	if v, ok := f[key]; ok {
		return v
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return nil
}

// TestLoad_NothingStored_ReturnsDefaults pins the out-of-the-box configuration
// of a fresh install. The Enabled assertion is the important one: enabling this
// feature with default settings ships recorded frames to a third party and
// costs money per event, so "off until asked for" is a correctness requirement,
// not a preference.
func TestLoad_NothingStored_ReturnsDefaults(t *testing.T) {
	c := Load(fakeGetter{})

	if c.Enabled {
		t.Error("Enabled = true, want false (the feature must ship off)")
	}
	if c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, DefaultBaseURL)
	}
	if c.Model != DefaultModel {
		t.Errorf("Model = %q, want %q", c.Model, DefaultModel)
	}
	if c.FrameCount != DefaultFrameCount {
		t.Errorf("FrameCount = %d, want %d", c.FrameCount, DefaultFrameCount)
	}
	if c.Timeout != DefaultTimeoutSeconds*time.Second {
		t.Errorf("Timeout = %v, want %v", c.Timeout, DefaultTimeoutSeconds*time.Second)
	}
	if c.MinConfidence != 0 {
		t.Errorf("MinConfidence = %v, want 0", c.MinConfidence)
	}
	if len(c.Labels) != 0 {
		t.Errorf("Labels = %v, want empty (allow every label)", c.Labels)
	}
	if c.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", c.APIKey)
	}
}

// TestLoad_CoercesEveryNumericWidthMsgpackMayReturn covers the same hazard
// NVRPlugin.nvrQuotaGB exists to handle: a schema DefaultValue arrives as a
// float64, but a value that round-tripped through msgpack can come back as any
// integer width msgpack chose on the wire. A missed case would silently fall
// back to the default and quietly ignore the user's setting.
func TestLoad_CoercesEveryNumericWidthMsgpackMayReturn(t *testing.T) {
	for _, tc := range []struct {
		name string
		val  any
	}{
		{"float64", float64(6)},
		{"float32", float32(6)},
		{"int", int(6)},
		{"int8", int8(6)},
		{"int16", int16(6)},
		{"int32", int32(6)},
		{"int64", int64(6)},
		{"uint", uint(6)},
		{"uint8", uint8(6)},
		{"uint16", uint16(6)},
		{"uint32", uint32(6)},
		{"uint64", uint64(6)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Load(fakeGetter{KeyFrameCount: tc.val})
			if c.FrameCount != 6 {
				t.Errorf("FrameCount = %d, want 6", c.FrameCount)
			}
		})
	}
}

// TestLoad_NonNumericValue_FallsBackToDefault proves an unusable stored value
// (a hand-written string, say) degrades to the default rather than to a zero
// that would clamp to the minimum and silently change behaviour.
func TestLoad_NonNumericValue_FallsBackToDefault(t *testing.T) {
	c := Load(fakeGetter{KeyFrameCount: "lots", KeyTimeoutSeconds: nil})

	if c.FrameCount != DefaultFrameCount {
		t.Errorf("FrameCount = %d, want %d", c.FrameCount, DefaultFrameCount)
	}
	if c.Timeout != DefaultTimeoutSeconds*time.Second {
		t.Errorf("Timeout = %v, want %v", c.Timeout, DefaultTimeoutSeconds*time.Second)
	}
}

// TestLoad_OutOfRangeValues_AreClamped proves every numeric setting is pulled
// into its advertised bounds rather than rejected. A stored value can predate a
// bound or be written straight through SetValue, bypassing the schema's
// Minimum/Maximum entirely, and a clamped value always beats refusing to run.
func TestLoad_OutOfRangeValues_AreClamped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store fakeGetter
		check func(t *testing.T, c Config)
	}{
		{
			"frame count below minimum",
			fakeGetter{KeyFrameCount: float64(0)},
			func(t *testing.T, c Config) {
				if c.FrameCount != 1 {
					t.Errorf("FrameCount = %d, want 1", c.FrameCount)
				}
			},
		},
		{
			"frame count above maximum",
			fakeGetter{KeyFrameCount: float64(99)},
			func(t *testing.T, c Config) {
				if c.FrameCount != 8 {
					t.Errorf("FrameCount = %d, want 8", c.FrameCount)
				}
			},
		},
		{
			"timeout below minimum",
			fakeGetter{KeyTimeoutSeconds: float64(1)},
			func(t *testing.T, c Config) {
				if c.Timeout != 10*time.Second {
					t.Errorf("Timeout = %v, want 10s", c.Timeout)
				}
			},
		},
		{
			"timeout above maximum",
			fakeGetter{KeyTimeoutSeconds: float64(9999)},
			func(t *testing.T, c Config) {
				if c.Timeout != 600*time.Second {
					t.Errorf("Timeout = %v, want 600s", c.Timeout)
				}
			},
		},
		{
			"min confidence above one",
			fakeGetter{KeyMinConfidence: float64(5)},
			func(t *testing.T, c Config) {
				if c.MinConfidence != 1 {
					t.Errorf("MinConfidence = %v, want 1", c.MinConfidence)
				}
			},
		},
		{
			"min confidence below zero",
			fakeGetter{KeyMinConfidence: float64(-1)},
			func(t *testing.T, c Config) {
				if c.MinConfidence != 0 {
					t.Errorf("MinConfidence = %v, want 0", c.MinConfidence)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, Load(tc.store))
		})
	}
}

// TestLoad_ParsesCommaSeparatedLabelAllowList proves the allow-list is
// normalized at load time, so AllowsLabels compares like against like and the
// user's spacing and casing in a free-text settings field don't matter.
func TestLoad_ParsesCommaSeparatedLabelAllowList(t *testing.T) {
	c := Load(fakeGetter{KeyLabels: " Person , VEHICLE ,, dog "})

	want := []string{"person", "vehicle", "dog"}
	if len(c.Labels) != len(want) {
		t.Fatalf("Labels = %v, want %v", c.Labels, want)
	}
	for i := range want {
		if c.Labels[i] != want[i] {
			t.Errorf("Labels[%d] = %q, want %q", i, c.Labels[i], want[i])
		}
	}
}

// TestLoad_TrimsWhitespaceAndTrailingSlashFromStrings covers the two ways a
// copy-pasted endpoint breaks a request: stray whitespace (fatal in an HTTP
// header, for the API key) and a trailing slash on the base URL, which would
// otherwise produce ".../v1//chat/completions".
func TestLoad_TrimsWhitespaceAndTrailingSlashFromStrings(t *testing.T) {
	c := Load(fakeGetter{
		KeyBaseURL: "  http://localhost:11434/v1/  ",
		KeyModel:   "  qwen2.5vl:7b  ",
		KeyAPIKey:  "  sk-abc  ",
	})

	if c.BaseURL != "http://localhost:11434/v1" {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, "http://localhost:11434/v1")
	}
	if c.Model != "qwen2.5vl:7b" {
		t.Errorf("Model = %q", c.Model)
	}
	if c.APIKey != "sk-abc" {
		t.Errorf("APIKey = %q", c.APIKey)
	}
}

// TestLoad_BlankStringsFallBackToDefaults proves a field the user cleared to
// whitespace behaves like one never set, rather than producing a Config that
// Validate rejects. Only BaseURL and Model do this — an empty APIKey is
// legitimate (a local Ollama needs none).
func TestLoad_BlankStringsFallBackToDefaults(t *testing.T) {
	c := Load(fakeGetter{KeyBaseURL: "   ", KeyModel: "\t"})

	if c.BaseURL != DefaultBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, DefaultBaseURL)
	}
	if c.Model != DefaultModel {
		t.Errorf("Model = %q, want %q", c.Model, DefaultModel)
	}
}

// TestConfig_Validate_RequiresBaseURLAndModel pins the two fields no request
// can be built without. Both are impossible to reach through Load (which
// substitutes defaults), so this guards Configs assembled directly in code.
func TestConfig_Validate_RequiresBaseURLAndModel(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"complete", Config{BaseURL: "http://x/v1", Model: "m"}, false},
		{"missing base url", Config{Model: "m"}, true},
		{"missing model", Config{BaseURL: "http://x/v1"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Error("Validate() = nil, want an error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestConfig_AllowsLabels_GatesOnTheAllowList covers the gate that decides
// whether an event costs money at all. The two asymmetric cases are the ones
// worth pinning: an empty allow-list permits an event with no labels, while a
// non-empty one rejects it, because "describe everything" and "describe only
// these" imply opposite answers for an event that matches nothing.
func TestConfig_AllowsLabels_GatesOnTheAllowList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		allow  []string
		labels []string
		want   bool
	}{
		{"empty allow-list permits anything", nil, []string{"cat"}, true},
		{"empty allow-list permits no labels at all", nil, nil, true},
		{"match", []string{"person"}, []string{"person"}, true},
		{"match is case insensitive", []string{"person"}, []string{"PERSON"}, true},
		{"match ignores surrounding space", []string{"person"}, []string{" person "}, true},
		{"one of several matches", []string{"person", "vehicle"}, []string{"cat", "vehicle"}, true},
		{"no match", []string{"person"}, []string{"cat", "dog"}, false},
		{"no labels against a non-empty allow-list", []string{"person"}, nil, false},
		{"blank labels are not a match", []string{"person"}, []string{"", "  "}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Labels: tc.allow}
			if got := c.AllowsLabels(tc.labels); got != tc.want {
				t.Errorf("AllowsLabels(%v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// TestQueueDepth_DefaultsAndClamps covers the one setting read outside Config,
// because it sizes a channel once at construction rather than per event.
func TestQueueDepth_DefaultsAndClamps(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store fakeGetter
		want  int
	}{
		{"default", fakeGetter{}, DefaultQueueDepth},
		{"configured", fakeGetter{KeyQueueDepth: float64(20)}, 20},
		{"clamped low", fakeGetter{KeyQueueDepth: float64(0)}, 1},
		{"clamped high", fakeGetter{KeyQueueDepth: float64(1000)}, 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := QueueDepth(tc.store); got != tc.want {
				t.Errorf("QueueDepth() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestLoad_NothingStored_DefaultsToOpenAI pins the provider default alongside
// what it implies. The provider is not just a label: it decides the endpoint and
// the model, so a wrong default here silently points a fresh install at the
// wrong service.
func TestLoad_NothingStored_DefaultsToOpenAI(t *testing.T) {
	c := Load(fakeGetter{})

	if c.Provider != ProviderOpenAI {
		t.Errorf("Provider = %q, want %q", c.Provider, ProviderOpenAI)
	}
	if c.BaseURL != DefaultOpenAIBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, DefaultOpenAIBaseURL)
	}
	if c.Model != DefaultModelOpenAI {
		t.Errorf("Model = %q, want %q", c.Model, DefaultModelOpenAI)
	}
}

// TestLoad_EachProvider_ResolvesItsOwnEndpointAndModel is the core of the
// provider mapping: picking a provider must be sufficient to reach it, with no
// other field required.
func TestLoad_EachProvider_ResolvesItsOwnEndpointAndModel(t *testing.T) {
	for _, tc := range []struct {
		provider    string
		wantBaseURL string
		wantModel   string
	}{
		{ProviderOpenAI, DefaultOpenAIBaseURL, DefaultModelOpenAI},
		{ProviderGemini, DefaultGeminiBaseURL, DefaultModelGemini},
		{ProviderOllama, DefaultOllamaBaseURL, DefaultModelOllama},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			c := Load(fakeGetter{KeyProvider: tc.provider})

			if c.Provider != tc.provider {
				t.Errorf("Provider = %q, want %q", c.Provider, tc.provider)
			}
			if c.BaseURL != tc.wantBaseURL {
				t.Errorf("BaseURL = %q, want %q", c.BaseURL, tc.wantBaseURL)
			}
			if c.Model != tc.wantModel {
				t.Errorf("Model = %q, want %q", c.Model, tc.wantModel)
			}
		})
	}
}

// TestLoad_HostedProviders_IgnoreAStoredBaseURL is the test that makes provider
// switching safe. KeyBaseURL persists after a user has been on Ollama, so if the
// hosted providers read it, selecting OpenAI would keep POSTing to localhost —
// or worse, a stale hosted URL would be sent an API key meant for someone else.
// Only Ollama may read that field.
func TestLoad_HostedProviders_IgnoreAStoredBaseURL(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     string
	}{
		{ProviderOpenAI, DefaultOpenAIBaseURL},
		{ProviderGemini, DefaultGeminiBaseURL},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			c := Load(fakeGetter{
				KeyProvider: tc.provider,
				KeyBaseURL:  "http://leftover-ollama.local:11434/v1",
			})
			if c.BaseURL != tc.want {
				t.Errorf("BaseURL = %q, want %q (the stored value must be ignored)", c.BaseURL, tc.want)
			}
		})
	}
}

// TestLoad_OllamaBaseURL_IsNormalizedAndFallsBack covers the one provider whose
// base URL is a real user-supplied field.
func TestLoad_OllamaBaseURL_IsNormalizedAndFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored any
		want   string
	}{
		{"trimmed and de-slashed", "  http://box.local:11434/v1/  ", "http://box.local:11434/v1"},
		{"blank falls back", "   ", DefaultOllamaBaseURL},
		{"absent falls back", nil, DefaultOllamaBaseURL},
		{"wrong type falls back", 42, DefaultOllamaBaseURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := fakeGetter{KeyProvider: ProviderOllama}
			if tc.stored != nil {
				g[KeyBaseURL] = tc.stored
			}
			if got := Load(g).BaseURL; got != tc.want {
				t.Errorf("BaseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoad_ProviderValue_IsNormalizedAndValidated keeps a stored provider that
// the resolver does not recognize from becoming a silent OpenAI charge. Falling
// back is the right behavior (a Config that cannot be loaded would disable the
// feature outright), but it must be the DOCUMENTED fallback rather than an
// accident of string comparison.
func TestLoad_ProviderValue_IsNormalizedAndValidated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stored any
		want   string
	}{
		{"exact", ProviderGemini, ProviderGemini},
		{"mixed case", "OpenAI", ProviderOpenAI},
		{"padded", "  ollama  ", ProviderOllama},
		{"unknown", "anthropic", DefaultProvider},
		{"empty", "", DefaultProvider},
		{"wrong type", 7, DefaultProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Load(fakeGetter{KeyProvider: tc.stored}).Provider; got != tc.want {
				t.Errorf("Provider = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoad_LegacyConfigWithoutProvider_InfersOllamaFromTheBaseURL covers the
// 5.3.0 upgrade path, and it is the highest-consequence test in this file.
//
// 5.3.0 had no provider setting, only a free-text base URL. A user who pointed
// it at a local Ollama has KeyBaseURL set and KeyProvider unset. Defaulting that
// to OpenAI would redirect a deliberately local, free, private setup to a paid
// API that also ships frames of their property off-site — without them changing
// anything. So an unset provider plus a non-OpenAI base URL reads as Ollama.
func TestLoad_LegacyConfigWithoutProvider_InfersOllamaFromTheBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name         string
		storedURL    any
		wantProvider string
		wantBaseURL  string
	}{
		{
			"local ollama url infers ollama",
			"http://localhost:11434/v1",
			ProviderOllama,
			"http://localhost:11434/v1",
		},
		{
			"any other host infers ollama",
			"http://gpu-box.lan:8000/v1",
			ProviderOllama,
			"http://gpu-box.lan:8000/v1",
		},
		{
			"the openai default stays openai",
			DefaultOpenAIBaseURL,
			ProviderOpenAI,
			DefaultOpenAIBaseURL,
		},
		{
			"the openai default with a trailing slash still stays openai",
			DefaultOpenAIBaseURL + "/",
			ProviderOpenAI,
			DefaultOpenAIBaseURL,
		},
		{
			"nothing stored at all stays openai",
			nil,
			ProviderOpenAI,
			DefaultOpenAIBaseURL,
		},
		{
			"a blank base url stays openai",
			"  ",
			ProviderOpenAI,
			DefaultOpenAIBaseURL,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := fakeGetter{}
			if tc.storedURL != nil {
				g[KeyBaseURL] = tc.storedURL
			}

			c := Load(g)
			if c.Provider != tc.wantProvider {
				t.Errorf("Provider = %q, want %q", c.Provider, tc.wantProvider)
			}
			if c.BaseURL != tc.wantBaseURL {
				t.Errorf("BaseURL = %q, want %q", c.BaseURL, tc.wantBaseURL)
			}
		})
	}
}

// TestLoad_ExplicitProvider_BeatsTheLegacyInference makes the inference strictly
// a migration aid. Once the user has chosen a provider it is authoritative —
// including choosing OpenAI while an Ollama base URL is still stored, which is
// exactly what happens the first time someone switches away from Ollama.
func TestLoad_ExplicitProvider_BeatsTheLegacyInference(t *testing.T) {
	c := Load(fakeGetter{
		KeyProvider: ProviderOpenAI,
		KeyBaseURL:  "http://localhost:11434/v1",
	})

	if c.Provider != ProviderOpenAI {
		t.Errorf("Provider = %q, want %q", c.Provider, ProviderOpenAI)
	}
	if c.BaseURL != DefaultOpenAIBaseURL {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, DefaultOpenAIBaseURL)
	}
}

// TestLoad_ModelKeys_PreferProviderSpecificThenLegacyThenDefault pins the
// three-step model resolution. The legacy step exists so a model chosen under
// 5.3.0 is not silently discarded on upgrade; the per-provider step exists so
// switching providers cannot carry the wrong model along.
func TestLoad_ModelKeys_PreferProviderSpecificThenLegacyThenDefault(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store fakeGetter
		want  string
	}{
		{
			"provider specific wins over legacy",
			fakeGetter{KeyProvider: ProviderOpenAI, KeyModelOpenAI: "gpt-5.6-terra", KeyModel: "old-model"},
			"gpt-5.6-terra",
		},
		{
			"legacy is used when the provider key is empty",
			fakeGetter{KeyProvider: ProviderOpenAI, KeyModel: "gpt-5.6-terra"},
			"gpt-5.6-terra",
		},
		{
			"legacy applies to gemini too",
			fakeGetter{KeyProvider: ProviderGemini, KeyModel: "gemini-3.5-flash"},
			"gemini-3.5-flash",
		},
		{
			"blank provider key falls through to legacy",
			fakeGetter{KeyProvider: ProviderOllama, KeyModelOllama: "   ", KeyModel: "llava:13b"},
			"llava:13b",
		},
		{
			"blank both falls through to the provider default",
			fakeGetter{KeyProvider: ProviderOllama, KeyModelOllama: "  ", KeyModel: "  "},
			DefaultModelOllama,
		},
		{
			"values are trimmed",
			fakeGetter{KeyProvider: ProviderOllama, KeyModelOllama: "  llava:13b  "},
			"llava:13b",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Load(tc.store).Model; got != tc.want {
				t.Errorf("Model = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoad_SwitchingProvider_DoesNotLeakAnotherProvidersModel is the behavioral
// reason the model is stored per provider rather than in one shared key. With a
// single key, configuring Ollama and then selecting Gemini would send
// "qwen2.5vl:7b" to Google and fail on every event until someone noticed.
//
// The legacy key is deliberately absent here: it is a cross-provider fallback by
// design, so including it would mask the very leak being tested.
func TestLoad_SwitchingProvider_DoesNotLeakAnotherProvidersModel(t *testing.T) {
	stored := fakeGetter{
		KeyModelOpenAI: "gpt-5.6-terra",
		KeyModelOllama: "qwen2.5vl:7b",
	}

	for _, tc := range []struct {
		provider string
		want     string
	}{
		{ProviderOpenAI, "gpt-5.6-terra"},
		{ProviderOllama, "qwen2.5vl:7b"},
		// Never configured, so it must land on its own default rather than
		// inheriting either of the two above.
		{ProviderGemini, DefaultModelGemini},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			stored[KeyProvider] = tc.provider
			if got := Load(stored).Model; got != tc.want {
				t.Errorf("Model = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoad_EveryProvider_ProducesAValidConfig guards against a provider that
// resolves to something Validate rejects, which would disable the feature with a
// log line rather than an obvious failure. Picking any provider from the dropdown
// must be enough to make the feature runnable.
func TestLoad_EveryProvider_ProducesAValidConfig(t *testing.T) {
	for _, provider := range []string{ProviderOpenAI, ProviderOllama, ProviderGemini} {
		t.Run(provider, func(t *testing.T) {
			if err := Load(fakeGetter{KeyProvider: provider}).Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}
