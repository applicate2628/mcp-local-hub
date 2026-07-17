package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestClassifyPersistedGUIPort(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want guiPortIntent
	}{
		{name: "absent", raw: "", want: guiPortIntent{Kind: guiPortIntentUnset}},
		{name: "whitespace", raw: " \t\r\n", want: guiPortIntent{Kind: guiPortIntentUnset}},
		{name: "minimum", raw: "1024", want: guiPortIntent{Kind: guiPortIntentValid, Port: 1024}},
		{name: "maximum", raw: "65535", want: guiPortIntent{Kind: guiPortIntentValid, Port: 65535}},
		{name: "trimmed valid", raw: " 9125 ", want: guiPortIntent{Kind: guiPortIntentValid, Port: 9125}},
		{name: "persisted zero is invalid", raw: "0", want: guiPortIntent{Kind: guiPortIntentInvalid, Raw: "0", Reason: guiPortInvalidOutOfRange}},
		{name: "below range", raw: "1023", want: guiPortIntent{Kind: guiPortIntentInvalid, Raw: "1023", Reason: guiPortInvalidOutOfRange}},
		{name: "above range", raw: "65536", want: guiPortIntent{Kind: guiPortIntentInvalid, Raw: "65536", Reason: guiPortInvalidOutOfRange}},
		{name: "not an integer", raw: "not-a-port", want: guiPortIntent{Kind: guiPortIntentInvalid, Raw: "not-a-port", Reason: guiPortInvalidNotInteger}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPersistedGUIPort(tt.raw); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("classifyPersistedGUIPort(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRestartV3_PortArgvMatrix(t *testing.T) {
	for _, precedence := range []struct {
		name        string
		flagChanged bool
		flagValue   int
		persisted   string
		want        int
	}{
		{name: "explicit zero beats valid persisted", flagChanged: true, flagValue: 0, persisted: "9300", want: 0},
		{name: "explicit port beats invalid persisted", flagChanged: true, flagValue: 8123, persisted: "bad", want: 8123},
		{name: "valid persisted beats ephemeral", persisted: "9300", want: 9300},
		{name: "invalid persisted falls back to ephemeral", persisted: "0", want: 0},
	} {
		t.Run("manual-precedence/"+precedence.name, func(t *testing.T) {
			if got := resolveGuiPort(precedence.flagChanged, precedence.flagValue, precedence.persisted); got != precedence.want {
				t.Fatalf("resolveGuiPort(%v, %d, %q) = %d, want %d", precedence.flagChanged, precedence.flagValue, precedence.persisted, got, precedence.want)
			}
		})
	}

	type argvShape struct {
		name      string
		argv      []string
		wantValid []string
		wantOther []string
		wantErr   bool
	}
	shapes := []argvShape{
		{name: "none", argv: []string{"gui", "--no-tray"}, wantValid: []string{"gui", "--no-tray"}, wantOther: []string{"gui", "--no-tray"}},
		{name: "separate", argv: []string{"gui", "--port", "9125", "--no-tray"}, wantValid: []string{"gui", "--no-tray"}, wantOther: []string{"gui", "--port", "9125", "--no-tray"}},
		{name: "equals", argv: []string{"gui", "--port=9125", "--no-browser"}, wantValid: []string{"gui", "--no-browser"}, wantOther: []string{"gui", "--port=9125", "--no-browser"}},
		{name: "explicit zero", argv: []string{"gui", "--port", "0"}, wantValid: []string{"gui"}, wantOther: []string{"gui", "--port", "0"}},
		{name: "repeated mixed", argv: []string{"gui", "--port", "9100", "--no-tray", "--port=9200"}, wantValid: []string{"gui", "--no-tray"}, wantOther: []string{"gui", "--port", "9100", "--no-tray", "--port=9200"}},
		{name: "after terminator", argv: []string{"gui", "--no-tray", "--", "--port", "9125"}, wantValid: []string{"gui", "--no-tray", "--", "--port", "9125"}, wantOther: []string{"gui", "--no-tray", "--", "--port", "9125"}},
		{name: "before and after terminator", argv: []string{"gui", "--port=9200", "--", "--port", "9300"}, wantValid: []string{"gui", "--", "--port", "9300"}, wantOther: []string{"gui", "--port=9200", "--", "--port", "9300"}},
		{name: "unregistered single-dash long form", argv: []string{"gui", "-port", "9125"}, wantErr: true},
		{name: "missing value", argv: []string{"gui", "--port"}, wantErr: true},
	}
	persisted := []struct {
		name  string
		raw   string
		valid bool
	}{
		{name: "unset", raw: ""},
		{name: "valid", raw: "9300", valid: true},
		{name: "invalid parse", raw: "abc"},
		{name: "invalid low zero", raw: "0"},
		{name: "invalid high", raw: "70000"},
	}

	for _, shape := range shapes {
		for _, setting := range persisted {
			t.Run(shape.name+"/"+setting.name, func(t *testing.T) {
				intent := classifyPersistedGUIPort(setting.raw)
				got, err := RebuildSelfRestartArgv(shape.argv, newGuiCmdReal().Flags(), intent)
				if shape.wantErr {
					if err == nil {
						t.Fatalf("RebuildSelfRestartArgv(%q, %q) succeeded, want parser error", shape.argv, setting.raw)
					}
					return
				}
				if err != nil {
					t.Fatalf("RebuildSelfRestartArgv(%q, %q): %v", shape.argv, setting.raw, err)
				}
				want := shape.wantOther
				if setting.valid {
					want = shape.wantValid
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("RebuildSelfRestartArgv(%q, %q) = %q, want %q", shape.argv, setting.raw, got, want)
				}
			})
		}
	}
}

func TestInvalidPersistedGUIPortWarningNamesFallbackWithoutClaimingApplication(t *testing.T) {
	intent := classifyPersistedGUIPort("0")
	for _, fallback := range []guiPortFallbackSource{guiPortFallbackExplicitFlag, guiPortFallbackEphemeral} {
		got := formatInvalidGUIPortWarning(intent, fallback)
		for _, want := range []string{"gui-port-persisted-invalid", `raw="0"`, "reason=out-of-range", "fallback=" + string(fallback)} {
			if !strings.Contains(got, want) {
				t.Errorf("warning %q missing %q", got, want)
			}
		}
		if strings.Contains(got, "applied") || strings.Contains(got, "using persisted") {
			t.Fatalf("warning %q falsely claims the invalid value took effect", got)
		}
	}
}
