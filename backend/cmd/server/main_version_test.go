package main

import "testing"

func TestResolveBuildVersions(t *testing.T) {
	tests := []struct {
		name               string
		version            string
		runtimeVersion     string
		embedded           string
		embeddedRuntime    string
		wantVersion        string
		wantRuntimeVersion string
	}{
		{
			name:               "custom display keeps pinned runtime",
			version:            "2.19.3-codeq",
			embedded:           "2.19.2-codeq",
			embeddedRuntime:    "2.19.1",
			wantVersion:        "2.19.3-codeq",
			wantRuntimeVersion: "2.19.1",
		},
		{
			name:               "display and runtime have independent defaults",
			embedded:           "2.19.2-codeq",
			embeddedRuntime:    "2.19.1",
			wantVersion:        "2.19.2-codeq",
			wantRuntimeVersion: "2.19.1",
		},
		{
			name:               "explicit runtime override",
			version:            "2.19.0-custom",
			runtimeVersion:     "2.18.0",
			embedded:           "2.19.0",
			embeddedRuntime:    "2.19.1",
			wantVersion:        "2.19.0-custom",
			wantRuntimeVersion: "2.18.0",
		},
		{
			name:               "empty embedded uses development fallback",
			wantVersion:        "0.0.0-dev",
			wantRuntimeVersion: "0.0.0-dev",
		},
		{
			name:               "custom display with empty embedded keeps development runtime",
			version:            "custom-build",
			wantVersion:        "custom-build",
			wantRuntimeVersion: "0.0.0-dev",
		},
		{
			name:               "trim explicit versions",
			version:            " \t2.19.0-custom\r\n",
			runtimeVersion:     "\n 2.18.0\t ",
			embedded:           " \t2.19.0\r\n",
			embeddedRuntime:    " \t2.19.1\r\n",
			wantVersion:        "2.19.0-custom",
			wantRuntimeVersion: "2.18.0",
		},
		{
			name:               "whitespace overrides default to trimmed embedded",
			version:            " \t\r\n",
			runtimeVersion:     "\n\t ",
			embedded:           " \t2.19.2-codeq\r\n",
			embeddedRuntime:    " \t2.19.1\r\n",
			wantVersion:        "2.19.2-codeq",
			wantRuntimeVersion: "2.19.1",
		},
		{
			name:               "whitespace embedded uses development fallback",
			embedded:           " \t\r\n",
			embeddedRuntime:    " \t\r\n",
			wantVersion:        "0.0.0-dev",
			wantRuntimeVersion: "0.0.0-dev",
		},
		{
			name:               "missing runtime never uses the codeq display version",
			embedded:           "2.19.2-codeq",
			wantVersion:        "2.19.2-codeq",
			wantRuntimeVersion: "0.0.0-dev",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotVersion, gotRuntimeVersion := resolveBuildVersions(tt.version, tt.runtimeVersion, tt.embedded, tt.embeddedRuntime)
			if gotVersion != tt.wantVersion || gotRuntimeVersion != tt.wantRuntimeVersion {
				t.Errorf("resolveBuildVersions(%q, %q, %q, %q) = (%q, %q), want (%q, %q)",
					tt.version, tt.runtimeVersion, tt.embedded, tt.embeddedRuntime,
					gotVersion, gotRuntimeVersion, tt.wantVersion, tt.wantRuntimeVersion)
			}
		})
	}
}
