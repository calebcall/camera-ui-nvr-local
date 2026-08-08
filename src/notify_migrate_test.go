package main

import (
	"slices"
	"testing"

	sdk "github.com/cameraui/sdk/go"
)

// attachedCamera returns a fake camera storage with the per-camera schemas
// already declared, i.e. the state add() leaves it in just before the migration
// runs — including notifyTypes materialized to the schema default, which
// AddSchema does and which is the whole reason cameraNotifySettingsAreInert
// cannot test for absence.
func attachedCamera(values map[string]any) *fakeCameraSettings {
	s := newFakeCameraSettings()
	for k, v := range values {
		s.values[k] = v
	}
	for _, schema := range cameraNotifySchema() {
		_ = s.AddSchema(&schema)
	}
	return s
}

func typesOf(t *testing.T, s *fakeCameraSettings) []string {
	t.Helper()
	return stringSliceValue(s.GetValue(notifyTypesKey, nil))
}

// TestMigrateNotifyObjects_ImportsIntoTheNewKeys is the case the whole file
// exists for: a camera the closed plugin was told to notify about people only,
// still storing that choice, still being ignored.
func TestMigrateNotifyObjects_ImportsIntoTheNewKeys(t *testing.T) {
	s := attachedCamera(map[string]any{notifyObjectsKey: []any{"person"}})

	if !migrateNotifyObjects("cam-1", s, nil) {
		t.Fatal("migration reported no change for a camera with an importable notifyObjects")
	}

	if got := typesOf(t, s); !slices.Equal(got, []string{"person"}) {
		t.Errorf("notifyTypes = %v, want [person]", got)
	}
	if override, _ := s.GetValue(notifyOverrideKey, false).(bool); !override {
		t.Error("notifyOverride was not turned on; the imported list would stay inert")
	}
	if done, _ := s.GetValue(notifyObjectsMigratedKey, false).(bool); !done {
		t.Error("the camera was not marked as migrated")
	}

	// The source values are left intact — this plugin never writes notifyObjects.
	if got := stringSliceValue(s.GetValue(notifyObjectsKey, nil)); !slices.Equal(got, []string{"person"}) {
		t.Errorf("notifyObjects = %v, want it untouched at [person]", got)
	}
}

// TestMigrateNotifyObjects_IsFaithfulNotNarrowing pins the behavior chosen over
// intersecting with the plugin-wide set: the import restores what the camera was
// actually configured with, even when that means it starts notifying for a type
// the plugin-wide settings currently have switched off.
func TestMigrateNotifyObjects_IsFaithfulNotNarrowing(t *testing.T) {
	s := attachedCamera(map[string]any{notifyObjectsKey: []any{"person", "vehicle"}})

	if !migrateNotifyObjects("cam-1", s, nil) {
		t.Fatal("migration reported no change")
	}
	if got := typesOf(t, s); !slices.Equal(got, []string{"person", "vehicle"}) {
		t.Errorf("notifyTypes = %v, want [person vehicle] verbatim", got)
	}
}

// TestMigrateNotifyObjects_RunsOnceEvenAfterTheUserOptsOut is what the marker
// buys. Turning the override back off is how someone says "actually, follow the
// plugin-wide settings"; a migration that re-fired on the next attach would
// overrule that silently, forever.
func TestMigrateNotifyObjects_RunsOnceEvenAfterTheUserOptsOut(t *testing.T) {
	s := attachedCamera(map[string]any{notifyObjectsKey: []any{"person"}})

	if !migrateNotifyObjects("cam-1", s, nil) {
		t.Fatal("first migration reported no change")
	}

	// The user reverts to following the plugin-wide settings.
	_ = s.SetValue(notifyOverrideKey, false)
	_ = s.SetValue(notifyTypesKey, notifyTypeOptions)

	if migrateNotifyObjects("cam-1", s, nil) {
		t.Error("migration re-fired after the user opted out; the marker did not hold")
	}
	if override, _ := s.GetValue(notifyOverrideKey, false).(bool); override {
		t.Error("the override was switched back on behind the user's back")
	}
}

// TestMigrateNotifyObjects_ExistingChoiceWins covers a camera already configured
// in the new vocabulary. It must not be touched — and must still be marked, so
// opting out later does not hand it to the migration after all.
func TestMigrateNotifyObjects_ExistingChoiceWins(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]any
	}{
		{"override already on", map[string]any{
			notifyObjectsKey:  []any{"person"},
			notifyOverrideKey: true,
			notifyTypesKey:    []any{"other"},
		}},
		{"types narrowed behind an inactive override", map[string]any{
			notifyObjectsKey: []any{"person"},
			notifyTypesKey:   []any{"animal"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := attachedCamera(tc.values)
			before := typesOf(t, s)

			if migrateNotifyObjects("cam-1", s, nil) {
				t.Error("migration overwrote a choice made in the new settings")
			}
			if got := typesOf(t, s); !slices.Equal(got, before) {
				t.Errorf("notifyTypes = %v, want it unchanged at %v", got, before)
			}
			if done, _ := s.GetValue(notifyObjectsMigratedKey, false).(bool); !done {
				t.Error("a skipped camera was left unmarked; it would be reconsidered after an opt-out")
			}
		})
	}
}

// TestMigrateNotifyObjects_NothingToImport covers the shapes that must leave the
// camera alone AND leave it unmarked, so a later restore of the closed plugin's
// values still gets picked up.
func TestMigrateNotifyObjects_NothingToImport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]any
	}{
		{"key absent", map[string]any{}},
		// Ambiguous between "switched everything off" and "never configured",
		// and guessing "off" silences the camera with no way to notice. See
		// legacyNotifyObjects.
		{"empty list", map[string]any{notifyObjectsKey: []any{}}},
		{"only unrecognized labels", map[string]any{notifyObjectsKey: []any{"bird", "weather"}}},
		{"wrong type entirely", map[string]any{notifyObjectsKey: "person"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := attachedCamera(tc.values)

			if migrateNotifyObjects("cam-1", s, nil) {
				t.Error("migration wrote something for a camera with nothing importable")
			}
			if override, _ := s.GetValue(notifyOverrideKey, false).(bool); override {
				t.Error("the override was turned on with nothing to import")
			}
			if got := typesOf(t, s); !slices.Equal(got, notifyTypeOptions) {
				t.Errorf("notifyTypes = %v, want the untouched default %v", got, notifyTypeOptions)
			}
			if done, _ := s.GetValue(notifyObjectsMigratedKey, false).(bool); done {
				t.Error("marked as migrated with nothing imported; a later restore would be ignored")
			}
		})
	}
}

// TestMigrateNotifyObjects_NormalizesAndOrders covers the coercion: msgpack
// shapes, casing and whitespace, unknown labels dropped, and a stable output
// order that does not depend on how the closed plugin happened to store it.
func TestMigrateNotifyObjects_NormalizesAndOrders(t *testing.T) {
	s := attachedCamera(map[string]any{
		notifyObjectsKey: []any{"VEHICLE", "  person  ", "bird", "person"},
	})

	if !migrateNotifyObjects("cam-1", s, nil) {
		t.Fatal("migration reported no change")
	}
	if got := typesOf(t, s); !slices.Equal(got, []string{"person", "vehicle"}) {
		t.Errorf("notifyTypes = %v, want [person vehicle] — normalized, deduped, ordered", got)
	}
}

// TestMigrateNotifyObjects_TheImportedFilterActuallyApplies closes the loop: the
// point of writing these keys is that notifyLabelFilter then honors them. Uses
// the real filter over the real registry rather than asserting on storage.
func TestMigrateNotifyObjects_TheImportedFilterActuallyApplies(t *testing.T) {
	var r cameraNotifyRegistry
	storage := newFakeCameraSettings()
	storage.values[notifyObjectsKey] = []any{"person"}

	r.add("cam-1", storage, nil)

	// Plugin-wide still allows vehicle; the imported per-camera list does not.
	f := newNotifyLabelFilter(notifyStore{notifyTypesKey: []string{"person", "vehicle"}}, &r)

	if f.NotifyAllowed(camEvent("cam-1", "vehicle")) {
		t.Error("vehicle notified on a camera imported as person-only")
	}
	if !f.NotifyAllowed(camEvent("cam-1", "person")) {
		t.Error("person was suppressed on a camera imported as person-only")
	}
}

// TestMigrateLegacyNotifyBooleans_ImportsWhatTheDeadFallbackNeverCould is the
// regression for the second silent failure: enabledNotifyTypes keys its legacy
// fallback off GetValue returning nil for an unset notifyTypes, but notifyTypes
// declares a DefaultValue, so GetValue returns the full option set instead and
// the fallback has never once executed. A user who switched Vehicle off in
// 5.5/5.6 had it switched back on by the upgrade, with no way to tell.
func TestMigrateLegacyNotifyBooleans_ImportsWhatTheDeadFallbackNeverCould(t *testing.T) {
	s := attachedCamera(map[string]any{
		notifyVehicleKey: false,
		notifyAnimalKey:  false,
	})

	if !migrateLegacyNotifyBooleans("camera cam-1", s, nil) {
		t.Fatal("migration reported no change for a store carrying pre-5.7 booleans")
	}
	if got := typesOf(t, s); !slices.Equal(got, []string{"person", "package", "other"}) {
		t.Errorf("notifyTypes = %v, want [person package other]", got)
	}
	if done, _ := s.GetValue(notifyBooleansMigratedKey, false).(bool); !done {
		t.Error("the store was not marked as migrated")
	}
}

// TestMigrateLegacyNotifyBooleans_LeavesTheDefaultAlone guards a subtle
// interaction rather than mere churn: writing an all-labels-on list would take
// notifyTypes off its default, which is the only signal migrateNotifyObjects
// has for "nobody has touched this" — so a pointless write here would silently
// disable the other migration.
func TestMigrateLegacyNotifyBooleans_LeavesTheDefaultAlone(t *testing.T) {
	s := attachedCamera(map[string]any{
		notifyPersonKey: true, notifyVehicleKey: true, notifyAnimalKey: true,
		notifyPackageKey: true, notifyOtherKey: true,
		notifyObjectsKey: []any{"person"},
	})

	if migrateLegacyNotifyBooleans("camera cam-1", s, nil) {
		t.Error("migration wrote a list identical to the default")
	}
	if got := typesOf(t, s); !slices.Equal(got, notifyTypeOptions) {
		t.Errorf("notifyTypes = %v, want the untouched default", got)
	}

	// The notifyObjects import must still see this camera as untouched.
	if !migrateNotifyObjects("cam-1", s, nil) {
		t.Error("the boolean pass blocked the notifyObjects import")
	}
}

// TestMigrateLegacyNotifyBooleans_SkipsAndMarks covers the cases that must not
// write. A store with no booleans at all stays unmarked — there is nothing to
// decide yet — while one whose list has since been edited is marked, so the
// superseded booleans cannot come back if it is later reset to the default.
func TestMigrateLegacyNotifyBooleans_SkipsAndMarks(t *testing.T) {
	t.Run("no booleans stored", func(t *testing.T) {
		s := attachedCamera(map[string]any{})

		if migrateLegacyNotifyBooleans("plugin", s, nil) {
			t.Error("migration wrote something with no booleans present")
		}
		if done, _ := s.GetValue(notifyBooleansMigratedKey, false).(bool); done {
			t.Error("marked as migrated with nothing to import")
		}
	})

	t.Run("list already edited", func(t *testing.T) {
		s := attachedCamera(map[string]any{
			notifyVehicleKey: false,
			notifyTypesKey:   []any{"person"},
		})

		if migrateLegacyNotifyBooleans("plugin", s, nil) {
			t.Error("migration overwrote a list the user had already edited")
		}
		if got := typesOf(t, s); !slices.Equal(got, []string{"person"}) {
			t.Errorf("notifyTypes = %v, want it unchanged at [person]", got)
		}
		if done, _ := s.GetValue(notifyBooleansMigratedKey, false).(bool); !done {
			t.Error("a superseded store was left unmarked; a reset would resurrect the booleans")
		}
	})
}

// TestMigrateLegacyNotifyBooleans_RunsOnce pins the marker, same contract as the
// notifyObjects import.
func TestMigrateLegacyNotifyBooleans_RunsOnce(t *testing.T) {
	s := attachedCamera(map[string]any{notifyVehicleKey: false})

	if !migrateLegacyNotifyBooleans("plugin", s, nil) {
		t.Fatal("first pass reported no change")
	}
	_ = s.SetValue(notifyTypesKey, notifyTypeOptions)

	if migrateLegacyNotifyBooleans("plugin", s, nil) {
		t.Error("migration re-fired after the list was reset to the default")
	}
}

// TestMigrateNotifyObjects_NonWritableStorageIsSkipped documents the type
// assertion in add(): storage that cannot write is simply not migrated, rather
// than being marked or panicking.
func TestMigrateNotifyObjects_NonWritableStorageIsSkipped(t *testing.T) {
	var r cameraNotifyRegistry
	r.add("cam-1", readOnlyCameraSettings{notifyObjectsKey: []any{"person"}}, nil)

	if _, ok := r.CameraNotifySettings("cam-1"); !ok {
		t.Error("cam-1 did not register; a non-writable storage must still resolve for filtering")
	}
}

// readOnlyCameraSettings satisfies recorder.CameraStorage but deliberately not
// notifyMigrationStore.
type readOnlyCameraSettings map[string]any

func (r readOnlyCameraSettings) GetValue(key string, fallback ...any) any {
	if v, ok := r[key]; ok {
		return v
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return nil
}

func (r readOnlyCameraSettings) HasSchema(string) bool { return false }

func (r readOnlyCameraSettings) AddSchema(*sdk.JsonSchema) error { return nil }
