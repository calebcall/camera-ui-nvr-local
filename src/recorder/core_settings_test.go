package recorder

import (
	"reflect"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// local is the stored per-camera config the plugin used before core owned
// these settings — every test below asserts what survives from it.
func local() RecordingConfig {
	return RecordingConfig{
		Mode:          RecordingModeContinuous,
		RetentionDays: 14,
		PreRollS:      5,
		PostRollS:     10,
		Roles:         []string{"high-resolution"},
	}
}

func TestApplyCoreRecordingSettings_AbsentSettingsKeepLocalConfig(t *testing.T) {
	// Core always defaults mode to "continuous" when it has settings for a
	// camera, so an empty Mode is how "core sent nothing" is detected. This
	// is what keeps an older core from silently disabling recording.
	got := applyCoreRecordingSettings(local(), sdk.CameraRecordingSettings{})

	if !reflect.DeepEqual(got, local()) {
		t.Fatalf("expected the local config untouched, got %+v", got)
	}
}

func TestApplyCoreRecordingSettings_SourcesBecomeRoles(t *testing.T) {
	got := applyCoreRecordingSettings(local(), sdk.CameraRecordingSettings{
		Enabled: true,
		Mode:    sdk.RecordingModeContinuous,
		Sources: []sdk.RecordingSource{sdk.RecordingSourceHigh, sdk.RecordingSourceMid, sdk.RecordingSourceLow},
	})

	want := []string{"high-resolution", "mid-resolution", "low-resolution"}
	if !reflect.DeepEqual(got.Roles, want) {
		t.Fatalf("expected %v, got %v", want, got.Roles)
	}
}

func TestApplyCoreRecordingSettings_MapsModes(t *testing.T) {
	cases := []struct {
		name string
		in   sdk.RecordingMode
		want RecordingMode
	}{
		// Core spells it "event"; this plugin spells it "events". Matching
		// on the plugin's own spelling would silently fall through to off.
		{"event maps to events", sdk.RecordingModeEvent, RecordingModeEvents},
		{"continuous passes through", sdk.RecordingModeContinuous, RecordingModeContinuous},
		// "record only when started manually" has no automatic-recording
		// equivalent here, so it must not be treated as continuous.
		{"adhoc is not continuous", sdk.RecordingModeAdhoc, RecordingModeOff},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := applyCoreRecordingSettings(local(), sdk.CameraRecordingSettings{
				Enabled: true,
				Mode:    c.in,
				Sources: []sdk.RecordingSource{sdk.RecordingSourceHigh},
			})
			if got.Mode != c.want {
				t.Fatalf("expected %q, got %q", c.want, got.Mode)
			}
		})
	}
}

func TestApplyCoreRecordingSettings_DisabledWins(t *testing.T) {
	got := applyCoreRecordingSettings(local(), sdk.CameraRecordingSettings{
		Enabled: false,
		Mode:    sdk.RecordingModeContinuous,
		Sources: []sdk.RecordingSource{sdk.RecordingSourceHigh},
	})

	if got.Mode != RecordingModeOff {
		t.Fatalf("enabled=false must stop recording, got mode %q", got.Mode)
	}
}

func TestApplyCoreRecordingSettings_PreBufferBecomesPreRoll(t *testing.T) {
	got := applyCoreRecordingSettings(local(), sdk.CameraRecordingSettings{
		Enabled:   true,
		Mode:      sdk.RecordingModeEvent,
		PreBuffer: 22,
		Sources:   []sdk.RecordingSource{sdk.RecordingSourceHigh},
	})

	if got.PreRollS != 22 {
		t.Fatalf("expected preBuffer to become preRollS, got %d", got.PreRollS)
	}
}

// postRollS and retentionDays have no core equivalent, so they must keep
// coming from the plugin's own storage.
func TestApplyCoreRecordingSettings_KeepsPluginOnlyFields(t *testing.T) {
	got := applyCoreRecordingSettings(local(), sdk.CameraRecordingSettings{
		Enabled: true,
		Mode:    sdk.RecordingModeContinuous,
		Sources: []sdk.RecordingSource{sdk.RecordingSourceLow},
	})

	if got.PostRollS != 10 {
		t.Errorf("expected postRollS 10 from local config, got %d", got.PostRollS)
	}
	if got.RetentionDays != 14 {
		t.Errorf("expected retentionDays 14 from local config, got %d", got.RetentionDays)
	}
}

// An empty Sources list with a populated Mode means core has settings but no
// tiers selected. Recording nothing is a legitimate choice and must not be
// papered over with the local default.
func TestApplyCoreRecordingSettings_EmptySourcesRecordNothing(t *testing.T) {
	got := applyCoreRecordingSettings(local(), sdk.CameraRecordingSettings{
		Enabled: true,
		Mode:    sdk.RecordingModeContinuous,
		Sources: []sdk.RecordingSource{},
	})

	if len(got.Roles) != 0 {
		t.Fatalf("expected no roles, got %v", got.Roles)
	}
}

func TestApplyCoreRecordingSettings_IgnoresUnknownSources(t *testing.T) {
	got := applyCoreRecordingSettings(local(), sdk.CameraRecordingSettings{
		Enabled: true,
		Mode:    sdk.RecordingModeContinuous,
		Sources: []sdk.RecordingSource{"ultra", sdk.RecordingSourceHigh},
	})

	want := []string{"high-resolution"}
	if !reflect.DeepEqual(got.Roles, want) {
		t.Fatalf("expected %v, got %v", want, got.Roles)
	}
}

// coreDefaults is core's own schema default for a camera — what a brand-new
// camera arrives carrying, and what core's migration left on every existing
// one. Until 5.12.0 this exact payload was discarded as "never configured".
func coreDefaults() sdk.CameraRecordingSettings {
	return sdk.CameraRecordingSettings{
		Enabled:   true,
		Mode:      sdk.RecordingModeContinuous,
		PreBuffer: 10,
		Sources: []sdk.RecordingSource{
			sdk.RecordingSourceHigh,
			sdk.RecordingSourceMid,
			sdk.RecordingSourceLow,
		},
	}
}

// TestApplyCoreRecordingSettings_DefaultPayloadStillWins is the regression for
// the trapdoor. Core's default payload is the most common real configuration
// there is — continuous, all three tiers — and it has to beat the plugin's
// stored key like any other core value. Discarding it made that one choice
// unexpressible: selecting it in the UI produced a payload indistinguishable
// from the migration's, so the plugin's own value stood instead.
func TestApplyCoreRecordingSettings_DefaultPayloadStillWins(t *testing.T) {
	off := local()
	off.Mode = RecordingModeOff

	got := applyCoreRecordingSettings(off, coreDefaults())

	if got.Mode != RecordingModeContinuous {
		t.Fatalf("core said continuous; plugin's stored %q must not win, got %q", off.Mode, got.Mode)
	}
	want := []string{"high-resolution", "mid-resolution", "low-resolution"}
	if !reflect.DeepEqual(got.Roles, want) {
		t.Fatalf("expected core's tiers %v, got %v", want, got.Roles)
	}
}

// TestApplyCoreRecordingSettings_FreshCameraRecords is the same bug in the
// shape a new user meets it. A brand-new camera has no plugin config, so
// readRecordingConfig hands back the schema default of "off" — and core sends
// its default "continuous". The camera reported continuous in the UI and
// recorded nothing.
func TestApplyCoreRecordingSettings_FreshCameraRecords(t *testing.T) {
	fresh := local()
	fresh.Mode = RecordingModeOff // what recordingConfigSchema defaults to

	if got := applyCoreRecordingSettings(fresh, coreDefaults()).Mode; got != RecordingModeContinuous {
		t.Fatalf("a fresh camera must follow core and record, got %q", got)
	}
}

// An edited payload was already honored before 5.12.0; it still is.
func TestApplyCoreRecordingSettings_EditedSettingsOverrideLocal(t *testing.T) {
	off := local()
	off.Mode = RecordingModeOff

	edited := coreDefaults()
	edited.Sources = []sdk.RecordingSource{sdk.RecordingSourceHigh, sdk.RecordingSourceMid}

	got := applyCoreRecordingSettings(off, edited)

	if got.Mode != RecordingModeContinuous {
		t.Fatalf("expected core's mode once it was edited, got %q", got.Mode)
	}
	want := []string{"high-resolution", "mid-resolution"}
	if !reflect.DeepEqual(got.Roles, want) {
		t.Fatalf("expected %v, got %v", want, got.Roles)
	}
}
