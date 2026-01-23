package models

import (
	"testing"
)

func TestSplitExeAndArgs(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantExe     string
		wantArgs    string
		wantErr     bool
		wantErrText string
	}{
		// Happy path - Quoted executable
		{
			name:     "quoted exe with args",
			input:    `"C:\Program Files\app.exe" /arg1 /arg2`,
			wantExe:  `"C:\Program Files\app.exe"`,
			wantArgs: `/arg1 /arg2`,
			wantErr:  false,
		},
		{
			name:     "quoted exe without args",
			input:    `"C:\Program Files\app.exe"`,
			wantExe:  `"C:\Program Files\app.exe"`,
			wantArgs: ``,
			wantErr:  false,
		},
		{
			name:     "quoted exe with leading/trailing whitespace",
			input:    `  "C:\app.exe" /arg  `,
			wantExe:  `"C:\app.exe"`,
			wantArgs: `/arg`,
			wantErr:  false,
		},

		// Happy path - Unquoted executable
		{
			name:     "unquoted exe with args",
			input:    `C:\app.exe /arg1 /arg2`,
			wantExe:  `C:\app.exe`,
			wantArgs: `/arg1 /arg2`,
			wantErr:  false,
		},
		{
			name:     "unquoted exe without args",
			input:    `C:\app.exe`,
			wantExe:  `C:\app.exe`,
			wantArgs: ``,
			wantErr:  false,
		},
		{
			name:     "unquoted exe with spaces in path (splits at first .exe)",
			input:    `C:\Program Files\app.exe /arg1`,
			wantExe:  `C:\Program Files\app.exe`,
			wantArgs: `/arg1`,
			wantErr:  false,
		},
		{
			name:     "multiple .exe in path (should split at first)",
			input:    `C:\tool.exe.backup\app.exe /arg`,
			wantExe:  `C:\tool.exe`,
			wantArgs: `.backup\app.exe /arg`,
			wantErr:  false,
		},

		// Case insensitivity
		{
			name:     "uppercase .EXE",
			input:    `C:\app.EXE /arg`,
			wantExe:  `C:\app.EXE`,
			wantArgs: `/arg`,
			wantErr:  false,
		},
		{
			name:     "mixed case .Exe",
			input:    `C:\app.Exe /arg`,
			wantExe:  `C:\app.Exe`,
			wantArgs: `/arg`,
			wantErr:  false,
		},

		// Error cases
		{
			name:        "empty string",
			input:       ``,
			wantErr:     true,
			wantErrText: "empty command line",
		},
		{
			name:        "whitespace only",
			input:       `   `,
			wantErr:     true,
			wantErrText: "empty command line",
		},
		{
			name:        "unterminated quote",
			input:       `"C:\app.exe`,
			wantErr:     true,
			wantErrText: "unterminated quoted executable",
		},
		{
			name:        "empty quotes",
			input:       `""`,
			wantErr:     true,
			wantErrText: "empty quoted executable",
		},
		{
			name:        "no .exe found (unquoted)",
			input:       `C:\app /arg1`,
			wantErr:     true,
			wantErrText: "no .exe found in command line",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotExe, gotArgs, err := SplitExeAndArgs(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Errorf("SplitExeAndArgs() expected error containing %q, got nil", tt.wantErrText)
					return
				}
				if err.Error() != tt.wantErrText {
					t.Errorf("SplitExeAndArgs() error = %q, want %q", err.Error(), tt.wantErrText)
				}
				return
			}

			if err != nil {
				t.Errorf("SplitExeAndArgs() unexpected error = %v", err)
				return
			}

			if gotExe != tt.wantExe {
				t.Errorf("SplitExeAndArgs() exe = %q, want %q", gotExe, tt.wantExe)
			}

			if gotArgs != tt.wantArgs {
				t.Errorf("SplitExeAndArgs() args = %q, want %q", gotArgs, tt.wantArgs)
			}
		})
	}
}
