//go:build linux

package tooling

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"altv1/internal/provider"
)

func TestProcessSessionStreamsInputAndConfinesWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the ALT executable to verify its private sandbox entrypoint")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "alt-process-test")
	build := exec.Command("go", "build", "-o", binary, "./cmd/alt")
	build.Dir = repository
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ALT process test binary: %v\n%s", err, output)
	}

	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside.txt")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ALT_SECRET_TEST", "must-not-reach-command")
	runtime, err := NewRuntimeWithOptions(context.Background(), workspace, RuntimeOptions{
		SensitiveEnvironment: []string{"ALT_SECRET_TEST"},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.executable = binary
	defer runtime.Close()

	first, err := runtime.processes.start(context.Background(), "agent:test", ExecCommandInput{
		Command:     "printf ready; read value; printf '|%s' \"$value\"",
		YieldTimeMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Running || first.SessionID == 0 || first.Output != "ready" {
		t.Fatalf("first process result = %#v", first)
	}
	if first.SessionID <= 0 {
		t.Fatalf("process session ID is not a positive private handle: %d", first.SessionID)
	}
	unknownID := first.SessionID + 1
	_, err = runtime.processes.write(context.Background(), "agent:test", WriteStdinInput{
		SessionID: unknownID,
	})
	wantUnknown := fmt.Sprintf("unknown process id %d for this assignment", unknownID)
	if err == nil || err.Error() != wantUnknown {
		t.Fatalf("unknown process error = %v, want %q", err, wantUnknown)
	}
	_, err = runtime.processes.write(context.Background(), "agent:other", WriteStdinInput{
		SessionID: first.SessionID,
	})
	wantIsolated := fmt.Sprintf("unknown process id %d for this assignment", first.SessionID)
	if err == nil || err.Error() != wantIsolated {
		t.Fatalf("cross-assignment process error = %v, want %q", err, wantIsolated)
	}
	reply, err := runtime.processes.write(context.Background(), "agent:test", WriteStdinInput{
		SessionID:   first.SessionID,
		Chars:       "received\n",
		YieldTimeMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reply.Output != "|received" {
		t.Fatalf("process reply = %#v", reply)
	}
	final := reply
	if reply.Running {
		final, err = runtime.processes.write(context.Background(), "agent:test", WriteStdinInput{
			SessionID:   first.SessionID,
			YieldTimeMS: 2000,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if final.Running || final.ExitCode == nil || *final.ExitCode != 0 {
		t.Fatalf("final process result = %#v", final)
	}
	_, err = runtime.processes.write(context.Background(), "agent:test", WriteStdinInput{
		SessionID: first.SessionID,
	})
	if err == nil || err.Error() != wantIsolated {
		t.Fatalf("completed process error = %v, want %q", err, wantIsolated)
	}

	escape, err := runtime.processes.start(context.Background(), "agent:test", ExecCommandInput{
		Command: "if printf forbidden > " + shellTestQuote(outside) +
			"; then exit 91; else printf confined; fi",
		YieldTimeMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	escapeOutput := escape.Output
	if escape.Running {
		escape, err = runtime.processes.write(context.Background(), "agent:test", WriteStdinInput{
			SessionID:   escape.SessionID,
			YieldTimeMS: 2000,
		})
		if err != nil {
			t.Fatal(err)
		}
		escapeOutput += escape.Output
	}
	if escape.Running || escape.ExitCode == nil || *escape.ExitCode != 0 ||
		!strings.Contains(escapeOutput, "confined") {
		t.Fatalf("sandbox escape result = %#v", escape)
	}
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("confined command created outside file: %v", err)
	}

	secret, err := runtime.processes.start(context.Background(), "agent:test", ExecCommandInput{
		Command:     `if test -z "$ALT_SECRET_TEST"; then printf scrubbed; else exit 91; fi`,
		YieldTimeMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret = collectTestProcess(t, runtime, "agent:test", secret)
	if secret.ExitCode == nil || *secret.ExitCode != 0 || secret.Output != "scrubbed" {
		t.Fatalf("credential environment result = %#v", secret)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	network, err := runtime.processes.start(context.Background(), "agent:test", ExecCommandInput{
		Command: "if /usr/bin/curl --silent --connect-timeout 1 http://" + listener.Addr().String() +
			"; then exit 91; else printf network-confined; fi",
		YieldTimeMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	network = collectTestProcess(t, runtime, "agent:test", network)
	if network.ExitCode == nil || *network.ExitCode != 0 ||
		!strings.Contains(network.Output, "network-confined") {
		t.Fatalf("network confinement result = %#v", network)
	}

	namespace, err := runtime.processes.start(context.Background(), "agent:test", ExecCommandInput{
		Command: "if test ! -e /proc/" + fmt.Sprint(os.Getpid()) +
			"; then printf pid-isolated; else exit 91; fi",
		YieldTimeMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	namespace = collectTestProcess(t, runtime, "agent:test", namespace)
	if namespace.ExitCode == nil || *namespace.ExitCode != 0 ||
		namespace.Output != "pid-isolated" {
		t.Fatalf("PID namespace result = %#v", namespace)
	}
}

func TestDangerousBypassSkipsFilesystemSandbox(t *testing.T) {
	if testing.Short() {
		t.Skip("executes a host command to verify the explicit bypass")
	}
	parent := t.TempDir()
	workspace := filepath.Join(parent, "workspace")
	outside := filepath.Join(parent, "outside.txt")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime, err := NewRuntimeWithOptions(context.Background(), workspace, RuntimeOptions{
		DangerouslyBypassApprovalsAndSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	result, err := runtime.processes.start(context.Background(), "agent:test", ExecCommandInput{
		Command:     "printf bypassed > " + shellTestQuote(outside),
		YieldTimeMS: 2000,
	})
	if err != nil {
		t.Fatal(err)
	}
	result = collectTestProcess(t, runtime, "agent:test", result)
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("bypass result = %#v", result)
	}
	body, err := os.ReadFile(outside)
	if err != nil || string(body) != "bypassed" {
		t.Fatalf("outside write = %q, %v", body, err)
	}
}

func collectTestProcess(
	t *testing.T,
	runtime *Runtime,
	owner string,
	result ProcessResult,
) ProcessResult {
	t.Helper()
	var combined strings.Builder
	combined.WriteString(result.Output)
	for result.Running || result.HasMore {
		var err error
		result, err = runtime.processes.write(context.Background(), owner, WriteStdinInput{
			SessionID: result.SessionID, YieldTimeMS: 2000,
		})
		if err != nil {
			t.Fatal(err)
		}
		combined.WriteString(result.Output)
	}
	result.Output = combined.String()
	return result
}

func TestFinishedProcessOutputPagesToTheDiscoveredModelEnvelope(t *testing.T) {
	workspace := t.TempDir()
	runtime, err := NewRuntimeWithOptions(context.Background(), workspace, RuntimeOptions{
		DangerouslyBypassApprovalsAndSandbox: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	const owner = "agent:bounded-output"
	runtime.setContextBudget(owner, newContextBudget(provider.ModelLimits{
		ContextWindow: provider.NewTokenLimit(1_024, provider.LimitSourceGatewayCatalog),
	}))

	result, err := runtime.processes.start(context.Background(), owner, ExecCommandInput{
		Command:     "head -c 5000 /dev/zero | tr '\\0' x",
		YieldTimeMS: 2_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for {
		if len(result.Output) > 1_024 {
			t.Fatalf("one model-visible process page exceeded the discovered envelope: %d", len(result.Output))
		}
		output.WriteString(result.Output)
		if !result.Running && !result.HasMore {
			break
		}
		result, err = runtime.processes.write(context.Background(), owner, WriteStdinInput{
			SessionID: result.SessionID, YieldTimeMS: 2_000,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if output.Len() != 5_000 || strings.Trim(output.String(), "x") != "" {
		t.Fatalf("paged process transcript was not exact: bytes=%d", output.Len())
	}
	if result.ExitCode == nil || *result.ExitCode != 0 {
		t.Fatalf("paged process did not preserve final status: %#v", result)
	}
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
