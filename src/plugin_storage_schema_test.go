package main

import (
	"slices"
	"testing"

	sdk "github.com/cameraui/sdk/go"

	"github.com/calebcall/plugins/camera-ui-nvr-local/src/describe"
)

// schemaByKey finds the one schema field declaring key, failing the test when
// no field does. Every assertion below goes through this rather than indexing
// the slice, because StorageSchema's order is presentation detail (it is what
// the settings form renders top-to-bottom) and a test that pinned indices
// would break on any reordering without a single real regression.
func schemaByKey(t *testing.T, schemas []sdk.JsonSchema, key string) sdk.JsonSchema {
	t.Helper()
	for _, s := range schemas {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("StorageSchema has no field with key %q", key)
	return sdk.JsonSchema{}
}

// TestStorageSchema_AIDescriptionFields_AreDeclaredWithMatchingDefaults pins
// the settings form to the constants src/describe reads at runtime. The two
// halves are separately written and separately easy to get wrong: the schema's
// DefaultValue is what a fresh install SHOWS in the form, while
// describe.Load's fallback is what the reader USES when a key was never
// written. If those disagree, the form displays one number and the feature
// behaves as another — the kind of bug nobody finds by looking at either file
// alone.
func TestStorageSchema_AIDescriptionFields_AreDeclaredWithMatchingDefaults(t *testing.T) {
	p := &NVRPlugin{}
	schemas := p.StorageSchema()

	for _, tc := range []struct {
		key      string
		wantType sdk.JsonSchemaType
		// wantDef is nil for the two fields that deliberately declare no
		// DefaultValue (the API key and the label allow-list), where empty is
		// a meaningful value rather than a placeholder for something else.
		wantDef any
	}{
		{describe.KeyEnabled, sdk.JsonSchemaTypeBoolean, false},
		{describe.KeyProvider, sdk.JsonSchemaTypeString, describe.DefaultProvider},
		{describe.KeyBaseURL, sdk.JsonSchemaTypeString, describe.DefaultOllamaBaseURL},
		{describe.KeyAPIKey, sdk.JsonSchemaTypeString, nil},
		{describe.KeyModelOpenAI, sdk.JsonSchemaTypeString, describe.DefaultModelOpenAI},
		{describe.KeyModelGemini, sdk.JsonSchemaTypeString, describe.DefaultModelGemini},
		{describe.KeyModelOllama, sdk.JsonSchemaTypeString, describe.DefaultModelOllama},
		{describe.KeyFrameCount, sdk.JsonSchemaTypeNumber, float64(describe.DefaultFrameCount)},
		{describe.KeyLabels, sdk.JsonSchemaTypeString, nil},
		{describe.KeyMinConfidence, sdk.JsonSchemaTypeNumber, float64(0)},
		{describe.KeyTimeoutSeconds, sdk.JsonSchemaTypeNumber, float64(describe.DefaultTimeoutSeconds)},
		{describe.KeyQueueDepth, sdk.JsonSchemaTypeNumber, float64(describe.DefaultQueueDepth)},
	} {
		t.Run(tc.key, func(t *testing.T) {
			s := schemaByKey(t, schemas, tc.key)
			if s.Type != tc.wantType {
				t.Errorf("Type = %q, want %q", s.Type, tc.wantType)
			}
			if tc.wantDef != nil && s.DefaultValue != tc.wantDef {
				t.Errorf("DefaultValue = %v (%T), want %v (%T)", s.DefaultValue, s.DefaultValue, tc.wantDef, tc.wantDef)
			}
			if s.Store == nil || !*s.Store {
				t.Error("Store must be true so the setting survives a restart")
			}
			if s.Title == "" {
				t.Error("Title must not be empty; it is the form label")
			}

		})
	}
}

// TestStorageSchema_AIFields_AreGatedBehindTheEnableToggle asserts the
// progressive-disclosure contract: an install that has not turned the feature
// on sees one checkbox, not nine fields plus a button it has no use for. The
// toggle itself must stay unconditional, or enabling the feature would be
// impossible — the field that reveals everything else cannot hide behind
// itself.
func TestStorageSchema_AIFields_AreGatedBehindTheEnableToggle(t *testing.T) {
	p := &NVRPlugin{}
	schemas := p.StorageSchema()

	gated := []string{
		describe.KeyProvider, describe.KeyBaseURL, describe.KeyAPIKey,
		describe.KeyModelOpenAI, describe.KeyModelGemini, describe.KeyModelOllama,
		describe.KeyFrameCount, describe.KeyLabels, describe.KeyMinConfidence,
		describe.KeyTimeoutSeconds, describe.KeyQueueDepth,
	}
	for _, key := range gated {
		s := schemaByKey(t, schemas, key)
		if len(s.Condition) == 0 {
			t.Errorf("%s has no Condition; it must be hidden until the feature is enabled", key)
			continue
		}
		c := s.Condition[0]
		if c.Key != describe.KeyEnabled {
			t.Errorf("%s condition key = %q, want %q", key, c.Key, describe.KeyEnabled)
		}
		if c.Value != true {
			t.Errorf("%s condition value = %v, want true", key, c.Value)
		}
	}

	if toggle := schemaByKey(t, schemas, describe.KeyEnabled); len(toggle.Condition) != 0 {
		t.Error("the enable toggle must not itself be conditional")
	}
}

// TestStorageSchema_AIFields_ShareOneGroup keeps the AI fields collapsed into a
// single tab of /settings/recordings rather than sprawling down the page among
// the existing recording settings.
func TestStorageSchema_AIFields_ShareOneGroup(t *testing.T) {
	p := &NVRPlugin{}
	schemas := p.StorageSchema()
	for _, key := range []string{
		describe.KeyEnabled, describe.KeyProvider, describe.KeyBaseURL, describe.KeyAPIKey,
		describe.KeyModelOpenAI, describe.KeyModelGemini, describe.KeyModelOllama,
		describe.KeyFrameCount, describe.KeyLabels, describe.KeyMinConfidence,
		describe.KeyTimeoutSeconds, describe.KeyQueueDepth,
	} {
		if got := schemaByKey(t, schemas, key).Group; got != genAIGroup {
			t.Errorf("%s Group = %q, want %q", key, got, genAIGroup)
		}
	}
}

// TestStorageSchema_EveryVisibleField_CarriesAGroup is the load-bearing test for
// the tabbed layout. The frontend (CuiSchema.vue) renders one tab per distinct
// non-empty group and renders every UNGROUPED non-hidden field loose beneath the
// whole tab strip. So a field that forgets its group does not fail visibly in any
// test that only checks its own properties — it just quietly reappears stranded
// under the tabs, which is precisely the layout this change exists to fix.
//
// Hidden fields are exempt: CuiSchema excludes them from both the tab strip and
// the ungrouped list, so grouping instanceId would only risk creating a tab with
// nothing visible in it.
func TestStorageSchema_EveryVisibleField_CarriesAGroup(t *testing.T) {
	p := &NVRPlugin{}
	for _, s := range p.StorageSchema() {
		if s.Hidden {
			continue
		}
		if s.Group == "" {
			t.Errorf("%s (%s) has no Group; it would render stranded below the tab strip", s.Key, s.Type)
		}
	}
}

// TestStorageSchema_GroupOrder_PutsStorageFirst pins the tab order, which the
// frontend derives from declaration order rather than from anything explicit
// (CuiSchema.vue's groupTabs appends each group the first time it is seen). It
// also selects the first group as the default tab, so this is what decides which
// tab a user actually lands on.
//
// Storage first is deliberate: it is the tab that matters on every install,
// whereas GenAI is opt-in and off by default. Reordering the fields would
// silently reorder the tabs, so this asserts the whole sequence rather than just
// the first entry.
func TestStorageSchema_GroupOrder_PutsStorageFirst(t *testing.T) {
	p := &NVRPlugin{}

	var groups []string
	for _, s := range p.StorageSchema() {
		if s.Hidden || s.Group == "" {
			continue
		}
		if len(groups) == 0 || groups[len(groups)-1] != s.Group {
			if !slices.Contains(groups, s.Group) {
				groups = append(groups, s.Group)
			}
		}
	}

	want := []string{storageGroup, detectionsGroup, genAIGroup}
	if !slices.Equal(groups, want) {
		t.Errorf("tab order = %v, want %v", groups, want)
	}
}

// TestStorageSchema_NoMotionOrAudioNotifyToggle pins a deliberate omission so a
// later well-meaning edit does not "complete the set".
//
// store.EventHasDetections excludes motion-only and audio-only events from
// notifying before the filter is ever consulted, so such a toggle would either
// do nothing at all or require changing what notifies — a behavior change that
// has no business arriving inside a filtering feature.
func TestStorageSchema_NoMotionOrAudioNotifyToggle(t *testing.T) {
	p := &NVRPlugin{}
	for _, s := range p.StorageSchema() {
		switch s.Key {
		case "notifyMotion", "notifyAudio":
			t.Errorf("%q must not exist: motion-only and audio-only events never notify, so it would be a dead control", s.Key)
		}
	}
}

// TestStorageSchema_ProviderScopedFields_AreGatedOnTheProvider covers the
// provider-conditional half of the form: the base URL and each model field must
// be visible for exactly one provider.
//
// Both conditions matter and both are asserted. Dropping the enabled condition
// would expose these on an install that never turned the feature on; dropping
// the provider condition would show all three model fields at once, each
// labelled "Model", which is worse than the single shared field this replaced.
func TestStorageSchema_ProviderScopedFields_AreGatedOnTheProvider(t *testing.T) {
	p := &NVRPlugin{}
	schemas := p.StorageSchema()

	for _, tc := range []struct {
		key          string
		wantProvider string
	}{
		{describe.KeyBaseURL, describe.ProviderOllama},
		{describe.KeyModelOpenAI, describe.ProviderOpenAI},
		{describe.KeyModelGemini, describe.ProviderGemini},
		{describe.KeyModelOllama, describe.ProviderOllama},
	} {
		t.Run(tc.key, func(t *testing.T) {
			s := schemaByKey(t, schemas, tc.key)
			if len(s.Condition) != 2 {
				t.Fatalf("Condition = %+v, want exactly 2 (enabled AND provider)", s.Condition)
			}

			var sawEnabled, sawProvider bool
			for _, c := range s.Condition {
				switch c.Key {
				case describe.KeyEnabled:
					sawEnabled = true
					if c.Value != true {
						t.Errorf("enabled condition value = %v, want true", c.Value)
					}
				case describe.KeyProvider:
					sawProvider = true
					if c.Value != tc.wantProvider {
						t.Errorf("provider condition value = %v, want %q", c.Value, tc.wantProvider)
					}
				default:
					t.Errorf("unexpected condition key %q", c.Key)
				}
			}
			if !sawEnabled || !sawProvider {
				t.Errorf("Condition = %+v, want one on %q and one on %q", s.Condition, describe.KeyEnabled, describe.KeyProvider)
			}
		})
	}
}

// TestStorageSchema_Provider_OffersExactlyTheSupportedProviders keeps the
// dropdown and the resolver in agreement. An enum value describe.Load does not
// recognize would silently fall back to OpenAI — i.e. a user could pick
// something and get billed by someone else entirely.
func TestStorageSchema_Provider_OffersExactlyTheSupportedProviders(t *testing.T) {
	p := &NVRPlugin{}
	s := schemaByKey(t, p.StorageSchema(), describe.KeyProvider)

	want := []string{describe.ProviderOpenAI, describe.ProviderOllama, describe.ProviderGemini}
	if !slices.Equal(s.Enum, want) {
		t.Errorf("Enum = %v, want %v", s.Enum, want)
	}
}

// TestStorageSchema_LegacyModelKey_IsNotDeclared guards the one-way nature of
// the legacy key. describe.KeyModel is still READ as an upgrade fallback, but
// declaring it here would put a second field labelled "Model" on the form and
// let the frontend write to it, resurrecting exactly the shared-model-across-
// providers problem the per-provider keys exist to prevent.
func TestStorageSchema_LegacyModelKey_IsNotDeclared(t *testing.T) {
	p := &NVRPlugin{}
	for _, s := range p.StorageSchema() {
		if s.Key == describe.KeyModel {
			t.Errorf("legacy key %q must not be declared in the schema; it is read-only for upgrades", describe.KeyModel)
		}
	}
}

// TestStorageSchema_ExistingFields_SurviveTheAIAdditions guards against the
// most plausible way to break this change: replacing the returned slice
// instead of appending to it. Dropping instanceId in particular would not fail
// loudly — it would silently regenerate the instance UUID on every restart and
// flush the frontend's event cache each time.
func TestStorageSchema_ExistingFields_SurviveTheAIAdditions(t *testing.T) {
	p := &NVRPlugin{}
	schemas := p.StorageSchema()
	for _, key := range []string{instanceIDStorageKey, nvrQuotaGBStorageKey, recordingPathStorageKey} {
		schemaByKey(t, schemas, key) // fails the test if absent
	}
}

// TestStorageSchema_TestConnectionButton_IsWiredToAHandler checks the submit
// field is actually actionable. A submit-type field with a nil OnClick renders
// as a button that silently does nothing when pressed, which is worse than no
// button at all for a feature whose whole purpose is diagnosing a
// misconfigured endpoint.
func TestStorageSchema_TestConnectionButton_IsWiredToAHandler(t *testing.T) {
	p := &NVRPlugin{}
	for _, s := range p.StorageSchema() {
		if s.Type == sdk.JsonSchemaTypeSubmit {
			if s.OnClick == nil {
				t.Error("the submit field has no OnClick handler")
			}
			if s.Group != genAIGroup {
				t.Errorf("submit Group = %q, want %q", s.Group, genAIGroup)
			}
			if len(s.Condition) == 0 || s.Condition[0].Key != describe.KeyEnabled {
				t.Errorf("submit Condition = %+v, want it gated behind %q", s.Condition, describe.KeyEnabled)
			}
			return
		}
	}
	t.Error("StorageSchema has no submit field for testing the AI connection")
}

// TestProbeFrame_Always_ReturnsDecodableJPEGBytes covers the one piece of
// testAIConnection that can be exercised without a configured endpoint. The
// probe image is the whole point of the Test Connection button: sending a real
// picture is what distinguishes a text-only model (which would pass any
// connectivity check and then fail on every actual event) from a working
// vision model, so it must genuinely be a decodable JPEG and not, say, an
// empty buffer that some servers happen to accept.
func TestProbeFrame_Always_ReturnsDecodableJPEGBytes(t *testing.T) {
	frame, err := probeFrame()
	if err != nil {
		t.Fatalf("probeFrame: %v", err)
	}
	if len(frame) == 0 {
		t.Fatal("probeFrame returned no bytes")
	}
	// JPEG's SOI marker. Checked explicitly rather than by decoding, because
	// the client base64-encodes these bytes into a data URL and the receiving
	// server sniffs exactly this prefix to decide the media type.
	if frame[0] != 0xFF || frame[1] != 0xD8 {
		t.Errorf("frame does not start with the JPEG SOI marker: % x", frame[:2])
	}
}

// TestStorageSchema_FieldsCarryNoHelpText pins the deliberately terse settings
// UI. Every field's Description renders as help text under its input, and with
// three tabs plus a per-camera panel that turned the settings pages into a wall
// of prose. Titles plus the README carry the meaning now.
//
// The single exception is the AI-descriptions master toggle: that line is a cost
// and privacy disclosure — enabling it ships frames of the user's property to a
// third party and bills them per event — not an explanation of what the control
// does. Deleting it should be a deliberate decision, so this test makes doing so
// fail.
func TestStorageSchema_FieldsCarryNoHelpText(t *testing.T) {
	p := &NVRPlugin{}

	for _, s := range p.StorageSchema() {
		if s.Key == describe.KeyEnabled {
			if s.Description == "" {
				t.Error("the AI enable toggle must keep its cost/privacy disclosure")
			}
			continue
		}
		if s.Description != "" {
			t.Errorf("%s has help text %q; settings are intentionally terse", s.Key, s.Description)
		}
	}
}

// TestStorageSchema_TitlesAreShort keeps titles readable inside a tab, where the
// tab itself already supplies the context a long title used to carry.
func TestStorageSchema_TitlesAreShort(t *testing.T) {
	const maxTitleLen = 24

	p := &NVRPlugin{}
	for _, s := range p.StorageSchema() {
		if s.Hidden {
			continue
		}
		if s.Title == "" {
			t.Errorf("%s has no Title; it is the only label the field has now", s.Key)
		}
		if len(s.Title) > maxTitleLen {
			t.Errorf("%s title %q is %d chars, want <= %d", s.Key, s.Title, len(s.Title), maxTitleLen)
		}
	}
}

// TestCameraNotifySchema_IsTerse applies the same rule to the per-camera panel.
func TestCameraNotifySchema_IsTerse(t *testing.T) {
	for _, s := range cameraNotifySchema() {
		if s.Description != "" {
			t.Errorf("%s has help text %q; the per-camera panel is terse too", s.Key, s.Description)
		}
		if s.Title == "" {
			t.Errorf("%s has no Title", s.Key)
		}
	}
}

// TestStorageSchema_NotifyTypes_IsOneMultiSelectDefaultingToEverything replaces
// the five per-type booleans this used to assert. The frontend draws each
// boolean as its own bordered row, so five of them filled the Detections tab
// with empty space; an enum with Multiple renders as a single control.
//
// DefaultValue must be every option: a fresh install has to notify exactly as it
// did before this setting existed, and a default of "none" would silently
// silence everything on upgrade.
func TestStorageSchema_NotifyTypes_IsOneMultiSelectDefaultingToEverything(t *testing.T) {
	p := &NVRPlugin{}
	s := schemaByKey(t, p.StorageSchema(), notifyTypesKey)

	if s.Type != sdk.JsonSchemaTypeString {
		t.Errorf("Type = %q, want %q (an enum field is a string type)", s.Type, sdk.JsonSchemaTypeString)
	}
	if !s.Multiple {
		t.Error("Multiple must be true, or the frontend renders a single-select")
	}
	if !slices.Equal(s.Enum, notifyTypeOptions) {
		t.Errorf("Enum = %v, want %v", s.Enum, notifyTypeOptions)
	}

	def, ok := s.DefaultValue.([]string)
	if !ok {
		t.Fatalf("DefaultValue = %#v, want []string", s.DefaultValue)
	}
	if !slices.Equal(def, notifyTypeOptions) {
		t.Errorf("DefaultValue = %v, want every option %v", def, notifyTypeOptions)
	}
	if s.Group != detectionsGroup {
		t.Errorf("Group = %q, want %q", s.Group, detectionsGroup)
	}
	if len(s.Condition) != 0 {
		t.Errorf("Condition = %+v, want none", s.Condition)
	}
}

// TestStorageSchema_DetectionsTab_IsASingleControl is the anti-regression guard
// for the layout complaint that prompted this: the tab must not drift back to a
// field per detection type.
func TestStorageSchema_DetectionsTab_IsASingleControl(t *testing.T) {
	p := &NVRPlugin{}

	var keys []string
	for _, s := range p.StorageSchema() {
		if s.Group == detectionsGroup && !s.Hidden {
			keys = append(keys, s.Key)
		}
	}

	if !slices.Equal(keys, []string{notifyTypesKey}) {
		t.Errorf("Detections tab fields = %v, want exactly [%s]", keys, notifyTypesKey)
	}
}

// TestStorageSchema_LegacyNotifyKeys_AreNotDeclared keeps the pre-5.7 boolean
// keys read-only. Declaring one would put a stray toggle back on the form and
// let the frontend write to a key that only exists as a migration fallback.
func TestStorageSchema_LegacyNotifyKeys_AreNotDeclared(t *testing.T) {
	legacy := map[string]bool{
		notifyPersonKey: true, notifyVehicleKey: true, notifyAnimalKey: true,
		notifyPackageKey: true, notifyOtherKey: true,
	}

	p := &NVRPlugin{}
	for _, s := range p.StorageSchema() {
		if legacy[s.Key] {
			t.Errorf("legacy key %q must not be declared; it is read-only for migration", s.Key)
		}
	}
	for _, s := range cameraNotifySchema() {
		if legacy[s.Key] {
			t.Errorf("legacy key %q must not be declared on the per-camera schema", s.Key)
		}
	}
}

// TestCameraNotifySchema_IsOverridePlusOneControl mirrors the global guard for
// the per-camera panel: an override toggle and one multi-select, nothing more.
func TestCameraNotifySchema_IsOverridePlusOneControl(t *testing.T) {
	var keys []string
	for _, s := range cameraNotifySchema() {
		keys = append(keys, s.Key)
	}

	if !slices.Equal(keys, []string{notifyOverrideKey, notifyTypesKey}) {
		t.Errorf("per-camera fields = %v, want [%s %s]", keys, notifyOverrideKey, notifyTypesKey)
	}
}
