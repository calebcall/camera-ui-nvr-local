package main

import (
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
		{describe.KeyBaseURL, sdk.JsonSchemaTypeString, describe.DefaultBaseURL},
		{describe.KeyAPIKey, sdk.JsonSchemaTypeString, nil},
		{describe.KeyModel, sdk.JsonSchemaTypeString, describe.DefaultModel},
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
			if s.Description == "" {
				t.Error("Description must not be empty; it is the help text")
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
		describe.KeyBaseURL, describe.KeyAPIKey, describe.KeyModel,
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

// TestStorageSchema_AIFields_ShareOneGroup keeps the nine new fields collapsed
// into a single section of /settings/recordings rather than sprawling down the
// page among the existing recording settings.
func TestStorageSchema_AIFields_ShareOneGroup(t *testing.T) {
	p := &NVRPlugin{}
	schemas := p.StorageSchema()
	for _, key := range []string{
		describe.KeyEnabled, describe.KeyBaseURL, describe.KeyAPIKey, describe.KeyModel,
		describe.KeyFrameCount, describe.KeyLabels, describe.KeyMinConfidence,
		describe.KeyTimeoutSeconds, describe.KeyQueueDepth,
	} {
		if got := schemaByKey(t, schemas, key).Group; got != aiDescriptionsGroup {
			t.Errorf("%s Group = %q, want %q", key, got, aiDescriptionsGroup)
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
			if s.Group != aiDescriptionsGroup {
				t.Errorf("submit Group = %q, want %q", s.Group, aiDescriptionsGroup)
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
