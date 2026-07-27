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
