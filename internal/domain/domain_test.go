package domain

import (
	"reflect"
	"runtime"
	"testing"
)

func TestValidateHost(t *testing.T) {
	for _, ok := range []string{"localhost", "127.0.0.1", "::1", "192.168.1.100", "freecad.lan", "my-host.example.com."} {
		if err := ValidateHost(ok); err != nil {
			t.Errorf("ValidateHost(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "http://host", "-bad", "a..b", "host:9875", "white space"} {
		if err := ValidateHost(bad); err == nil {
			t.Errorf("ValidateHost(%q) accepted", bad)
		}
	}
}

func TestSplitCommand(t *testing.T) {
	got, err := SplitCommand("flatpak run --command=freecadcmd org.freecad.FreeCAD")
	if err != nil || !reflect.DeepEqual(got, []string{"flatpak", "run", "--command=freecadcmd", "org.freecad.FreeCAD"}) {
		t.Fatalf("got %q, %v", got, err)
	}
	if runtime.GOOS == "windows" {
		got, err = SplitCommand(`"C:\Program Files\FreeCAD 1.1\bin\freecadcmd.exe" C:\Users\O'Neil\x`)
		want := []string{`C:\Program Files\FreeCAD 1.1\bin\freecadcmd.exe`, `C:\Users\O'Neil\x`}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	} else {
		// As shlex.split: inside double quotes a backslash escapes only " \ $ `.
		got, err = SplitCommand(`"/opt/a\b/free cad" "q\"uote" 'a b' c\ d`)
		want := []string{`/opt/a\b/free cad`, `q"uote`, "a b", "c d"}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", `"unterminated`} {
		if _, err := SplitCommand(bad); err == nil {
			t.Errorf("SplitCommand(%q) accepted", bad)
		}
	}
}

func TestSettingsFromEnv(t *testing.T) {
	for _, k := range []string{EnvHost, EnvPort, EnvToken, EnvOnlyTextFeedback, EnvFreecadCmd, EnvFreecadGUI} {
		t.Setenv(k, "")
	}
	s, err := SettingsFromEnv(" stored ", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "127.0.0.1" || s.Port != DefaultRPCPort || s.Token != "stored" || s.OnlyTextFeedback || s.FreecadCmd != nil {
		t.Fatalf("defaults = %+v", s)
	}

	t.Setenv(EnvHost, "192.168.1.100")
	t.Setenv(EnvPort, "9999")
	t.Setenv(EnvToken, "from-env")
	t.Setenv(EnvOnlyTextFeedback, "true")
	t.Setenv(EnvFreecadCmd, "flatpak run --command=freecadcmd org.freecad.FreeCAD")
	s, err = SettingsFromEnv("stored", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Host != "192.168.1.100" || s.Port != 9999 || s.Token != "from-env" || !s.OnlyTextFeedback || len(s.FreecadCmd) != 4 {
		t.Fatalf("from env = %+v", s)
	}

	for k, v := range map[string]string{EnvHost: "not a host", EnvPort: "0", EnvOnlyTextFeedback: "maybe"} {
		t.Run(k, func(t *testing.T) {
			t.Setenv(k, v)
			if _, err := SettingsFromEnv("", "", ""); err == nil {
				t.Fatalf("%s=%q accepted", k, v)
			}
		})
	}
}

// TestSettingsFromEnvResolvesLocalhostToLoopback covers the live-fixes
// review N2 fix: "localhost" and "localhost." (a trailing dot, an absolute
// DNS name IsLoopbackHost already accepts) both resolve to 127.0.0.1
// itself, never left as the name, which can resolve to ::1 first and be
// captured by something else bound there (WSL's own port forwarding).
func TestSettingsFromEnvResolvesLocalhostToLoopback(t *testing.T) {
	for _, k := range []string{EnvHost, EnvPort, EnvToken, EnvOnlyTextFeedback, EnvFreecadCmd, EnvFreecadGUI} {
		t.Setenv(k, "")
	}
	for _, host := range []string{"localhost", "localhost.", "LOCALHOST", "LocalHost."} {
		t.Setenv(EnvHost, host)
		s, err := SettingsFromEnv("", "", "")
		if err != nil {
			t.Fatalf("%q: %v", host, err)
		}
		if s.Host != "127.0.0.1" {
			t.Errorf("%q resolved to %q, want 127.0.0.1", host, s.Host)
		}
	}
}
