package addoninstall

import (
	"os"
	"testing"
)

func TestReadGeneralSettingsBackgroundAfter(t *testing.T) {
	cases := []struct {
		name string
		file string // "" means no settings file
		want int
	}{
		{"no file gives the default", "", DefaultBackgroundAfterMinutes},
		{"key missing gives the default", `{"session_timeout_minutes": 5}`, DefaultBackgroundAfterMinutes},
		{"a value in range is read", `{"background_after_minutes": 45}`, 45},
		{"the lowest value", `{"background_after_minutes": 1}`, 1},
		{"the highest value", `{"background_after_minutes": 1440}`, 1440},
		{"below the range gives the default", `{"background_after_minutes": 0}`, DefaultBackgroundAfterMinutes},
		{"above the range gives the default", `{"background_after_minutes": 1441}`, DefaultBackgroundAfterMinutes},
		{"a fraction gives the default", `{"background_after_minutes": 2.5}`, DefaultBackgroundAfterMinutes},
		{"a string gives the default", `{"background_after_minutes": "10"}`, DefaultBackgroundAfterMinutes},
		{"null gives the default", `{"background_after_minutes": null}`, DefaultBackgroundAfterMinutes},
	}
	for _, c := range cases {
		target := Target{UserDataDir: t.TempDir()}
		if c.file != "" {
			if err := os.WriteFile(SettingsPath(target), []byte(c.file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got, err := ReadGeneralSettings(target)
		if err != nil || got.BackgroundAfterMinutes != c.want {
			t.Errorf("%s: got %+v, %v; want %d", c.name, got, err, c.want)
		}
	}
}

func TestReadGeneralSettingsRejectsAFileThatIsNotAnObject(t *testing.T) {
	target := Target{UserDataDir: t.TempDir()}
	if err := os.WriteFile(SettingsPath(target), []byte(`[1, 2]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGeneralSettings(target); err == nil {
		t.Fatal("a settings file that is not a JSON object was accepted")
	}
}
