package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func executeForOutput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Execute(context.Background(), args, strings.NewReader(""), &output, &output)
	return output.String(), err
}

func TestRootHelpMatchesApplicableCodexCommandSurface(t *testing.T) {
	output, err := executeForOutput(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"alt [OPTIONS] [PROMPT]", "exec", "resume", "completion",
		"-C, --cd", "--dangerously-bypass-approvals-and-sandbox", "-V, --version",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("root help is missing %q:\n%s", expected, output)
		}
	}
	for _, forbidden := range []string{"\n  run ", "\n  version ", "--model", "--profile", "--sandbox", "--ask-for-approval"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("root help exposed obsolete or inapplicable Codex surface %q:\n%s", forbidden, output)
		}
	}
}

func TestExecAndResumeHelpUseCodexNamesWithALTSemantics(t *testing.T) {
	execHelp, err := executeForOutput(t, "exec", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"alt exec [PROMPT]", "Aliases:", "exec, e", "--team"} {
		if !strings.Contains(execHelp, expected) {
			t.Fatalf("exec help is missing %q:\n%s", expected, execHelp)
		}
	}
	for _, obsolete := range []string{"--profile", "--workspace"} {
		if strings.Contains(execHelp, obsolete) {
			t.Fatalf("exec help retained %q:\n%s", obsolete, execHelp)
		}
	}

	resumeHelp, err := executeForOutput(t, "resume", "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"alt resume [SESSION_ID] [PROMPT]", "--last"} {
		if !strings.Contains(resumeHelp, expected) {
			t.Fatalf("resume help is missing %q:\n%s", expected, resumeHelp)
		}
	}
}

func TestCompletionDefaultsToBashAndRejectsUnknownShell(t *testing.T) {
	output, err := executeForOutput(t, "completion")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "bash completion") {
		t.Fatalf("default completion is not bash:\n%s", output)
	}
	if _, err := executeForOutput(t, "completion", "nushell"); err == nil {
		t.Fatal("unsupported completion shell succeeded")
	}
}

func TestVersionAndDangerousAliasDoNotStartTheTUI(t *testing.T) {
	output, err := executeForOutput(t, "--yolo", "--version")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output) == "" {
		t.Fatal("--version produced no version")
	}
}

func TestResumeLastFailsDeterministicallyWhenHistoryIsEmpty(t *testing.T) {
	output, err := executeForOutput(t, "--data-dir", t.TempDir(), "resume", "--last")
	if err == nil || !strings.Contains(err.Error(), "no saved sessions") {
		t.Fatalf("resume --last error = %v, output = %q", err, output)
	}
}
